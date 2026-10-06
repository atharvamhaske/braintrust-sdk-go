package cloudflare

// this file parses the Workers AI accounts/{account_id}/ai/run/{model} endpoint.
//
// Workers AI runs about 14 different task types (text generation, text and
// multimodal embeddings, image classification, object detection, speech,
// translation, summarization, image captioning, text-to-image, ...) through
// this one endpoint. Two pairs of request shapes are genuinely
// indistinguishable on the wire (a bare {"image": [...]} body is either
// image classification or object detection; a bare {"text": "..."} body is
// either text classification or single-string text embeddings), so this
// tracer does not attempt to classify the task from the request at all.
//
// It classifies responses by their shape: {"data": [...], "shape": [...]} is
// embeddings, a response with "usage" is text generation, and other JSON or
// binary media responses use the canonical media-operation payload. Streaming
// isn't handled because cloudflare-go v7's AI.Run has no streaming variant.
//
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// aiRunTracer is a tracer for the Workers AI ai/run/{model} endpoint.
type aiRunTracer struct {
	cfg      *middlewareConfig
	model    string
	metadata map[string]any
	request  map[string]any
}

func newAIRunTracer(cfg *middlewareConfig, model string) *aiRunTracer {
	return &aiRunTracer{
		cfg:   cfg,
		model: model,
		metadata: map[string]any{
			"provider": "cloudflare",
			// Overwritten in TagSpan with the API response's own model
			// string when the task type reports one (text generation
			// does); this is just the fallback.
			"model": model,
		},
	}
}

func (rt *aiRunTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	var raw map[string]any
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, nil, err
	}
	rt.request = raw

	// The task isn't known yet (see file doc comment), so the span starts
	// with a generic name; TagSpan renames it once the response reveals
	// the task.
	ctx, span := rt.cfg.tracer().Start(ctx, "cloudflare.ai.run", trace.WithTimestamp(t))

	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "llm"}); err != nil {
		return ctx, span, err
	}

	// Completion calls use the canonical OpenAI-style messages array.
	if messages, ok := raw["messages"]; ok {
		if err := internal.SetJSONAttr(span, "braintrust.input_json", messages); err != nil {
			return ctx, span, err
		}
	} else if prompt, ok := raw["prompt"].(string); ok {
		messages := []any{map[string]any{"role": "user", "content": prompt}}
		if err := internal.SetJSONAttr(span, "braintrust.input_json", messages); err != nil {
			return ctx, span, err
		}
	} else if err := internal.SetJSONAttr(span, "braintrust.input_json", raw); err != nil {
		return ctx, span, err
	}

	// Fields the instrumentation spec explicitly allows in metadata.
	for _, field := range []string{"max_tokens", "temperature", "top_p", "frequency_penalty", "presence_penalty", "response_format"} {
		if v, ok := raw[field]; ok {
			rt.metadata[field] = v
		}
	}
	// Domain-specific fields beyond the spec's base allowlist, useful for
	// Workers AI callers: top_k/seed control generation determinism, and
	// stream/task tell the reader what kind of call this was.
	for _, field := range []string{"top_k", "seed", "stream"} {
		if v, ok := raw[field]; ok {
			rt.metadata[field] = v
		}
	}

	if tools, ok := raw["tools"].([]any); ok && len(tools) > 0 {
		rt.metadata["tools"] = convertToolDefinitions(tools)
	}
	for _, field := range []string{"tool_choice", "parallel_tool_calls", "max_tool_calls"} {
		if value, ok := raw[field]; ok {
			rt.metadata[field] = value
		}
	}

	return ctx, span, internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata)
}

// convertToolDefinitions converts Workers AI's tool definitions into the
// OpenAI Chat Completions shape the instrumentation spec requires
// (metadata.tools = [{type:"function", function:{name,description,parameters}}]).
// Workers AI's API accepts three different wire shapes for a tool
// definition (flat, an alternate flat "object" shape, and one already
// nested under "function"); this handles all three.
func convertToolDefinitions(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			// Already OpenAI-nested on the wire.
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		fn := map[string]any{}
		if name, ok := tool["name"]; ok {
			fn["name"] = name
		}
		if desc, ok := tool["description"]; ok {
			fn["description"] = desc
		}
		if params, ok := tool["parameters"]; ok {
			fn["parameters"] = params
		}
		if strict, ok := tool["strict"].(bool); ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func (rt *aiRunTracer) TagSpan(span trace.Span, body io.Reader) error {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return nil
	}

	var result any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return nil
	}

	resultObj, isObject := result.(map[string]any)
	_, hasData := resultObj["data"]
	_, hasShape := resultObj["shape"]
	// A tool-calling-only reply sets "response" to JSON null rather than
	// omitting it, so key presence is checked instead of a non-nil value.
	// "usage" is only ever present on text generation replies.
	_, hasUsage := resultObj["usage"]
	switch {
	case isObject && hasData && hasShape:
		span.SetName("cloudflare.ai.embeddings")
		rt.metadata["task"] = "embeddings"
		return rt.tagEmbeddings(span, resultObj)
	case isObject && hasUsage:
		span.SetName("cloudflare.ai.text_generation")
		rt.metadata["task"] = "text_generation"
		return rt.tagTextGeneration(span, resultObj)
	default:
		input, operation := genericMediaInput(rt.request)
		if err := internal.SetJSONAttr(span, "braintrust.input_json", input); err != nil {
			return err
		}
		span.SetName("cloudflare.ai." + operation)
		model := rt.model
		if resolved, ok := resultObj["model"].(string); ok && resolved != "" {
			model = resolved
		}
		rt.metadata = map[string]any{"provider": "cloudflare", "model": model}
		if err := internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata); err != nil {
			return err
		}
		if text, ok := result.(string); ok {
			return internal.SetJSONAttr(span, "braintrust.output_json", map[string]any{
				"content": []any{map[string]any{"type": "text", "text": text}},
			})
		}
		return internal.SetJSONAttr(span, "braintrust.output_json", map[string]any{"content": []any{}, "annotations": result})
	}
}

func genericMediaInput(request map[string]any) (map[string]any, string) {
	input := map[string]any{"operation": "predict"}
	if text, ok := request["text"].(string); ok {
		input["operation"] = "classify"
		input["prompt"] = text
		return input, "classify"
	}
	if image, ok := request["image"].(string); ok {
		input["operation"] = "classify"
		input["content"] = []any{imagePart(image, request)}
		return input, "classify"
	}
	if audio, ok := request["audio"].(string); ok {
		input["operation"] = "transcribe"
		if prompt, ok := request["prompt"].(string); ok {
			input["prompt"] = prompt
		}
		if params := selectFields(request, []string{"language", "format", "timestamp_granularities"}); len(params) > 0 {
			input["parameters"] = params
		}
		contentType := mediaContentType(request, "audio/mpeg")
		filename := mediaFilename(contentType)
		input["content"] = []any{map[string]any{"type": "file", "file": map[string]any{"filename": filename, "file_data": mediaURL(audio, contentType)}}}
		return input, "transcribe"
	}
	if prompt, ok := request["prompt"].(string); ok {
		input["prompt"] = prompt
	}
	return input, "predict"
}

// TagResponse adds canonical media output for Workers AI models that return
// image or audio bytes. It records media only after the application consumes
// the complete response body; early close leaves the binary output absent.
func (rt *aiRunTracer) TagResponse(span trace.Span, response *http.Response, body io.Reader, complete bool) error {
	if response == nil {
		return rt.TagSpan(span, body)
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "json") || strings.HasSuffix(contentType, "+json") {
		return rt.TagSpan(span, body)
	}
	if !complete {
		return nil
	}
	if contentType == "application/octet-stream" || contentType == "" {
		contentType = mediaContentType(rt.request, "")
	}
	if !strings.HasPrefix(contentType, "image/") && !strings.HasPrefix(contentType, "audio/") {
		return nil
	}

	operation := "generate"
	parameters := []string{"n", "size", "aspect_ratio", "quality", "style", "seed", "background", "output_format"}
	if strings.HasPrefix(contentType, "audio/") {
		operation = "speech"
		parameters = []string{"voice", "format", "speed", "language"}
	}
	input := map[string]any{"operation": operation}
	if prompt, ok := rt.request["prompt"].(string); ok {
		input["prompt"] = prompt
	} else if text, ok := rt.request["text"].(string); ok && operation == "speech" {
		input["prompt"] = text
	}
	if params := selectFields(rt.request, parameters); len(params) > 0 {
		input["parameters"] = params
	}
	if err := internal.SetJSONAttr(span, "braintrust.input_json", input); err != nil {
		return err
	}

	// Request configuration belongs in the canonical media input parameters,
	// not LLM metadata. Keep only provider/model identifiers for media calls.
	rt.metadata = map[string]any{"provider": "cloudflare", "model": rt.model}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata); err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	filename := mediaFilename(contentType)
	dataURI := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
	var part map[string]any
	if strings.HasPrefix(contentType, "image/") {
		part = map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI}}
	} else {
		part = map[string]any{"type": "file", "file": map[string]any{"filename": filename, "file_data": dataURI, "byte_size": len(data)}}
	}
	span.SetName("cloudflare.ai." + operation)
	return internal.SetJSONAttr(span, "braintrust.output_json", map[string]any{"content": []any{part}})
}

func imagePart(image string, request map[string]any) map[string]any {
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": mediaURL(image, mediaContentType(request, "image/png"))},
	}
}

func mediaURL(value, contentType string) string {
	if strings.HasPrefix(value, "data:") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return value
	}
	return "data:" + contentType + ";base64," + value
}

func mediaContentType(request map[string]any, fallback string) string {
	format, _ := request["format"].(string)
	if format == "" {
		format, _ = request["output_format"].(string)
	}
	if format != "" {
		if !strings.HasPrefix(format, ".") {
			format = "." + format
		}
		if contentType := mime.TypeByExtension(format); contentType != "" {
			if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
				return parsed
			}
		}
	}
	return fallback
}

func mediaFilename(contentType string) string {
	filename := "attachment"
	if extensions, err := mime.ExtensionsByType(contentType); err == nil && len(extensions) > 0 {
		filename += extensions[0]
	}
	return filename
}

func selectFields(source map[string]any, fields []string) map[string]any {
	selected := make(map[string]any)
	for _, field := range fields {
		if value, ok := source[field]; ok {
			selected[field] = value
		}
	}
	return selected
}

// tagTextGeneration uses result.choices directly as output: Workers AI's
// chat models already return it in valid OpenAI Chat Completions shape
// (index, finish_reason, message.role/content/tool_calls with tool_calls[].id
// /type/function.name/function.arguments-as-a-JSON-string), which is exactly
// what the instrumentation spec requires for a non-OpenAI/Anthropic/Google
// provider. Cloudflare-specific null fields inside choices/message (audio,
// reasoning, routed_experts, ...) are left in place; the spec doesn't
// forbid extra fields.
func (rt *aiRunTracer) tagTextGeneration(span trace.Span, result map[string]any) error {
	if model, ok := result["model"].(string); ok && model != "" {
		rt.metadata["model"] = model
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata); err != nil {
		return err
	}
	if err := internal.SetJSONAttr(span, "braintrust.output_json", result["choices"]); err != nil {
		return err
	}

	metrics := map[string]any{}
	if usage, ok := result["usage"].(map[string]any); ok {
		if ok, v := internal.ToInt64(usage["prompt_tokens"]); ok {
			metrics["prompt_tokens"] = v
		}
		if ok, v := internal.ToInt64(usage["completion_tokens"]); ok {
			metrics["completion_tokens"] = v
		}
		if ok, v := internal.ToInt64(usage["total_tokens"]); ok {
			metrics["tokens"] = v
		}
	}
	return internal.SetJSONAttr(span, "braintrust.metrics", metrics)
}

// tagEmbeddings covers both text and multimodal embeddings: both return the
// identical {data, shape} shape, so the raw vectors are summarized to a
// count rather than copied in full, matching this repo's other embedding
// integrations (genai, genkit, eino, langchaingo all emit {"count": N}).
func (rt *aiRunTracer) tagEmbeddings(span trace.Span, result map[string]any) error {
	if err := internal.SetJSONAttr(span, "braintrust.input_json", embeddingInput(rt.request)); err != nil {
		return err
	}
	if model, ok := result["model"].(string); ok && model != "" {
		rt.metadata["model"] = model
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata); err != nil {
		return err
	}

	count := 0
	if data, ok := result["data"].([]any); ok {
		count = len(data)
	}
	if err := internal.SetJSONAttr(span, "braintrust.output_json", map[string]any{"count": count}); err != nil {
		return err
	}
	metrics := map[string]any{}
	if usage, ok := result["usage"].(map[string]any); ok {
		if ok, tokens := internal.ToInt64(usage["prompt_tokens"]); ok {
			metrics["prompt_tokens"] = tokens
		}
		if ok, tokens := internal.ToInt64(usage["total_tokens"]); ok {
			metrics["tokens"] = tokens
		} else if promptTokens, ok := metrics["prompt_tokens"]; ok {
			metrics["tokens"] = promptTokens
		}
	}
	return internal.SetJSONAttr(span, "braintrust.metrics", metrics)
}

// embeddingInput converts Workers AI's text/texts request fields into the
// canonical Braintrust embedding input shape. Each independent provider input
// remains one ordered entry, and an explicitly requested output size is kept.
func embeddingInput(request map[string]any) map[string]any {
	inputs := make([]any, 0, 1)
	if text, ok := request["text"].(string); ok {
		inputs = append(inputs, map[string]any{"content": text})
	} else if texts, ok := request["text"].([]any); ok {
		for _, text := range texts {
			inputs = append(inputs, map[string]any{"content": text})
		}
	}
	if len(inputs) == 0 {
		if text, ok := request["texts"].(string); ok {
			inputs = append(inputs, map[string]any{"content": text})
		} else if texts, ok := request["texts"].([]any); ok {
			for _, text := range texts {
				inputs = append(inputs, map[string]any{"content": text})
			}
		}
	}
	if image, ok := request["image"].(string); ok {
		if len(inputs) == 1 {
			if textInputs, isText := inputs[0].(map[string]any); isText {
				if textContent, isString := textInputs["content"].(string); isString {
					textInputs["content"] = []any{
						map[string]any{"type": "text", "text": textContent},
						imagePart(image, request),
					}
					return embeddingInputWithDimensions(inputs, request)
				}
			}
		}
		inputs = append(inputs, map[string]any{"content": []any{imagePart(image, request)}})
	}
	return embeddingInputWithDimensions(inputs, request)
}

func embeddingInputWithDimensions(inputs []any, request map[string]any) map[string]any {
	input := map[string]any{"inputs": inputs}
	if dimensions, ok := request["output_dimensions"]; ok {
		input["output_dimensions"] = dimensions
	}
	return input
}

// Ensure our tracer implements the shared interface.
var _ internal.MiddlewareTracer = &aiRunTracer{}
