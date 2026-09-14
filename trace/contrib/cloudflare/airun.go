package cloudflare

// this file parses the Workers AI accounts/{account_id}/ai/run/{model} endpoint.
//
// Every task (text generation, embeddings, ...) shares this one endpoint, so
// the task is inferred from which fields are present in the request body:
// "messages" means text generation, "image" means multimodal embeddings, and
// a bare "text" means text embeddings.

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

type aiRunTask string

const (
	taskTextGeneration      aiRunTask = "text_generation"
	taskTextEmbeddings      aiRunTask = "text_embeddings"
	taskMultimodalEmbedding aiRunTask = "multimodal_embeddings"
)

// aiRunTracer is a tracer for the Workers AI ai/run/{model} endpoint.
type aiRunTracer struct {
	cfg      *middlewareConfig
	model    string
	task     aiRunTask
	metadata map[string]any
}

func newAIRunTracer(cfg *middlewareConfig, model string) *aiRunTracer {
	return &aiRunTracer{
		cfg:   cfg,
		model: model,
		metadata: map[string]any{
			"provider": "cloudflare",
			"model":    model,
		},
	}
}

func (rt *aiRunTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	var raw map[string]any
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, nil, err
	}

	rt.task = classifyAIRunTask(raw)
	rt.metadata["task"] = string(rt.task)

	ctx, span := rt.cfg.tracer().Start(ctx, "cloudflare.ai."+string(rt.task), trace.WithTimestamp(t))

	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "llm"}); err != nil {
		return ctx, span, err
	}

	if err := internal.SetJSONAttr(span, "braintrust.input_json", aiRunInput(rt.task, raw)); err != nil {
		return ctx, span, err
	}

	for _, field := range []string{"max_tokens", "temperature", "top_p", "top_k", "seed", "stream", "tools", "response_format"} {
		if v, ok := raw[field]; ok {
			rt.metadata[field] = v
		}
	}

	return ctx, span, internal.SetJSONAttr(span, "braintrust.metadata", rt.metadata)
}

// classifyAIRunTask decides which of Workers AI's task shapes a request body is.
func classifyAIRunTask(raw map[string]any) aiRunTask {
	if _, ok := raw["messages"]; ok {
		return taskTextGeneration
	}
	if _, ok := raw["image"]; ok {
		return taskMultimodalEmbedding
	}
	return taskTextEmbeddings
}

// aiRunInput extracts the part of the request that represents the actual
// input, dropping generation parameters already captured in metadata.
func aiRunInput(task aiRunTask, raw map[string]any) any {
	switch task {
	case taskTextGeneration:
		return raw["messages"]
	case taskMultimodalEmbedding:
		input := map[string]any{}
		if text, ok := raw["text"]; ok {
			input["text"] = text
		}
		if image, ok := raw["image"]; ok {
			input["image"] = image
		}
		return input
	default:
		return raw["text"]
	}
}

func (rt *aiRunTracer) TagSpan(span trace.Span, body io.Reader) error {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return err
	}

	var result map[string]any
	if err := json.Unmarshal(envelope.Result, &result); err != nil {
		return err
	}

	switch rt.task {
	case taskTextGeneration:
		return rt.tagTextGeneration(span, result)
	default:
		return rt.tagEmbeddings(span, result)
	}
}

func (rt *aiRunTracer) tagTextGeneration(span trace.Span, result map[string]any) error {
	output := map[string]any{"response": result["response"]}
	if toolCalls, ok := result["tool_calls"]; ok {
		output["tool_calls"] = toolCalls
	}
	if err := internal.SetJSONAttr(span, "braintrust.output_json", output); err != nil {
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

// tagEmbeddings covers both text_embeddings and multimodal_embeddings. Both
// return the identical {data, shape} response shape, so the raw vectors are
// summarized to a count and dimension rather than copied in full.
func (rt *aiRunTracer) tagEmbeddings(span trace.Span, result map[string]any) error {
	summary := map[string]any{"count": 0}
	if data, ok := result["data"].([]any); ok {
		summary["count"] = len(data)
	}
	if shape, ok := result["shape"]; ok {
		summary["shape"] = shape
	}
	return internal.SetJSONAttr(span, "braintrust.output_json", summary)
}

// Ensure our tracer implements the shared interface.
var _ internal.MiddlewareTracer = &aiRunTracer{}
