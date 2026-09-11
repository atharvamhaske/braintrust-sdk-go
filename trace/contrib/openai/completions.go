package openai

// this file parses the legacy v1/completions API.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// completionsTracer is a tracer for the openai v1/completions POST endpoint.
// See docs here: https://platform.openai.com/docs/api-reference/completions/create
type completionsTracer struct {
	cfg       *middlewareConfig
	streaming bool
	metadata  map[string]any
	startTime time.Time
}

func newCompletionsTracer(cfg *middlewareConfig) *completionsTracer {
	return &completionsTracer{
		cfg:       cfg,
		streaming: false,
		metadata: map[string]any{
			"provider": "openai",
			"endpoint": "/v1/completions",
		},
	}
}

func (ct *completionsTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	ct.startTime = t
	ctx, span := ct.cfg.tracer().Start(
		ctx,
		"Completion",
		trace.WithTimestamp(t),
	)

	var raw map[string]interface{}
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, span, err
	}

	metadataFields := []string{
		"model",
		"best_of",
		"echo",
		"frequency_penalty",
		"logit_bias",
		"logprobs",
		"max_tokens",
		"n",
		"presence_penalty",
		"seed",
		"stop",
		"stream",
		"stream_options",
		"suffix",
		"temperature",
		"top_p",
		"user",
	}

	for _, field := range metadataFields {
		if value, exists := raw[field]; exists {
			ct.metadata[field] = value
			if field == "stream" {
				if value, ok := value.(bool); ok {
					ct.streaming = value
				}
			}
		}
	}

	if prompt, ok := raw["prompt"]; ok {
		if err := internal.SetJSONAttr(span, "braintrust.input_json", prompt); err != nil {
			return ctx, span, err
		}
	}

	if err := internal.SetJSONAttr(span, "braintrust.metadata", ct.metadata); err != nil {
		return ctx, span, err
	}

	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "llm"}); err != nil {
		return ctx, span, err
	}

	return ctx, span, nil
}

func (ct *completionsTracer) TagSpan(span trace.Span, body io.Reader) error {
	if ct.streaming {
		return ct.parseStreamingResponse(span, body)
	}
	return ct.parseResponse(span, body)
}

func (ct *completionsTracer) parseResponse(span trace.Span, body io.Reader) error {
	timeToFirstToken := time.Since(ct.startTime)

	var raw map[string]interface{}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return err
	}

	return ct.handleCompletionResponse(span, raw, timeToFirstToken)
}

func (ct *completionsTracer) handleCompletionResponse(span trace.Span, rawMsg map[string]any, timeToFirstToken time.Duration) error {
	metadataFields := []string{
		"id",
		"object",
		"created",
		"system_fingerprint",
	}
	for _, field := range metadataFields {
		if v, ok := rawMsg[field]; ok {
			ct.metadata[field] = v
		}
	}

	if err := internal.SetJSONAttr(span, "braintrust.metadata", ct.metadata); err != nil {
		return err
	}

	metrics := make(map[string]any)
	if usage, ok := rawMsg["usage"].(map[string]any); ok {
		for k, v := range parseUsageTokens(usage) {
			metrics[k] = v
		}
	}
	metrics["time_to_first_token"] = timeToFirstToken.Seconds()
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	if choices, ok := rawMsg["choices"]; ok {
		if err := internal.SetJSONAttr(span, "braintrust.output_json", choices); err != nil {
			return err
		}
	}

	return nil
}

// parseStreamingResponse handles v1/completions streaming. Unlike chat
// completions (whose chunks are deltas merged onto a message), each
// completions chunk is a full Completion object per OpenAI's own docs, so
// this just accumulates each choice index's text fragments directly rather
// than needing chat's delta/tool-call merge logic.
func (ct *completionsTracer) parseStreamingResponse(span trace.Span, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	texts := make(map[int64]*strings.Builder)
	finishReasons := make(map[int64]any)
	var order []int64
	var timeToFirstToken time.Duration

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		line = strings.TrimPrefix(line, "data: ")
		if line == "[DONE]" {
			break
		}

		if timeToFirstToken == 0 {
			timeToFirstToken = time.Since(ct.startTime)
		}

		var chunk map[string]any
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			return err
		}

		if usage, ok := chunk["usage"].(map[string]any); ok {
			ct.metadata["usage"] = usage
		}

		choices, ok := chunk["choices"].([]any)
		if !ok {
			continue
		}
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			_, index := internal.ToInt64(choice["index"])
			if _, seen := texts[index]; !seen {
				texts[index] = &strings.Builder{}
				order = append(order, index)
			}
			if text, ok := choice["text"].(string); ok {
				texts[index].WriteString(text)
			}
			if fr, exists := choice["finish_reason"]; exists && fr != nil {
				finishReasons[index] = fr
			}
		}
	}

	if len(order) > 0 {
		output := make([]map[string]any, 0, len(order))
		for _, index := range order {
			output = append(output, map[string]any{
				"index":         index,
				"text":          texts[index].String(),
				"finish_reason": finishReasons[index],
			})
		}
		if err := internal.SetJSONAttr(span, "braintrust.output_json", output); err != nil {
			return err
		}
	}

	if err := internal.SetJSONAttr(span, "braintrust.metadata", ct.metadata); err != nil {
		return err
	}

	metrics := make(map[string]any)
	if usage, ok := ct.metadata["usage"].(map[string]any); ok {
		for k, v := range parseUsageTokens(usage) {
			metrics[k] = v
		}
	}
	if timeToFirstToken > 0 {
		metrics["time_to_first_token"] = timeToFirstToken.Seconds()
	}
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	return scanner.Err()
}
