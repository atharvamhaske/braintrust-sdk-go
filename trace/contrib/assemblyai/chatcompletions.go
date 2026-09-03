package assemblyai

// this file parses LLM Gateway's /v1/chat/completions endpoint. Its
// request/response shape is genuinely OpenAI-compatible (verified against a
// live call, not just AssemblyAI's docs), so the field extraction mirrors
// trace/contrib/openai's chatCompletionsTracer closely. Streaming isn't
// handled: LLM Gateway's SSE framing hasn't been confirmed to match OpenAI's,
// so a streaming request still gets a span but its body isn't parsed.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// chatCompletionsTracer is a tracer for LLM Gateway's /v1/chat/completions endpoint.
type chatCompletionsTracer struct {
	cfg       *config
	streaming bool
	metadata  map[string]any
	startTime time.Time
}

func newChatCompletionsTracer(cfg *config) *chatCompletionsTracer {
	return &chatCompletionsTracer{
		cfg: cfg,
		metadata: map[string]any{
			"provider": "assemblyai",
			"endpoint": "/v1/chat/completions",
		},
	}
}

func (ct *chatCompletionsTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	ct.startTime = t
	ctx, span := ct.cfg.tracer().Start(ctx, "assemblyai.chat_completions", trace.WithTimestamp(t))

	var raw map[string]any
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, span, err
	}

	metadataFields := []string{
		"model",
		"max_tokens",
		"temperature",
		"top_p",
		"stop",
		"stream",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
		"model_region",
	}
	for _, field := range metadataFields {
		if value, exists := raw[field]; exists {
			ct.metadata[field] = value
			if field == "stream" {
				if streaming, ok := value.(bool); ok {
					ct.streaming = streaming
				}
			}
		}
	}

	if messages, ok := raw["messages"]; ok {
		if err := internal.SetJSONAttr(span, "braintrust.input_json", messages); err != nil {
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

func (ct *chatCompletionsTracer) TagSpan(span trace.Span, body io.Reader) error {
	// Streaming isn't parsed (see package/file doc) - just leave the span with
	// its request-side attributes rather than risk mis-parsing SSE framing
	// that hasn't been confirmed to match OpenAI's.
	if ct.streaming {
		return nil
	}

	timeToFirstToken := time.Since(ct.startTime)

	var raw map[string]any
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return err
	}

	// AssemblyAI-specific envelope fields, alongside the OpenAI-compatible ones.
	for _, field := range []string{"request_id", "llm_status_code", "response_time"} {
		if v, ok := raw[field]; ok {
			ct.metadata[field] = v
		}
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", ct.metadata); err != nil {
		return err
	}

	metrics := make(map[string]any)
	if usage, ok := raw["usage"].(map[string]any); ok {
		for k, v := range parseUsageTokens(usage) {
			metrics[k] = v
		}
	}
	metrics["time_to_first_token"] = timeToFirstToken.Seconds()
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	if choices, ok := raw["choices"]; ok {
		if err := internal.SetJSONAttr(span, "braintrust.output_json", choices); err != nil {
			return err
		}
	}

	return nil
}

// parseUsageTokens normalizes LLM Gateway's usage object to Braintrust metric
// names. LLM Gateway's usage carries both OpenAI-standard names
// (prompt_tokens/completion_tokens/total_tokens) and AssemblyAI's own aliases
// (input_tokens/output_tokens) simultaneously with identical values, so both
// map to the same target key without conflict.
func parseUsageTokens(usage map[string]any) map[string]int64 {
	metrics := make(map[string]int64)
	for k, v := range usage {
		if strings.HasSuffix(k, "_tokens_details") {
			prefix := strings.TrimSuffix(k, "_tokens_details")
			if details, ok := v.(map[string]any); ok {
				for kd, vd := range details {
					if ok, i := internal.ToInt64(vd); ok {
						metrics[prefix+"_"+kd] = i
					}
				}
			}
			continue
		}
		ok, i := internal.ToInt64(v)
		if !ok {
			continue
		}
		switch k {
		case "input_tokens":
			metrics["prompt_tokens"] = i
		case "output_tokens":
			metrics["completion_tokens"] = i
		case "total_tokens":
			metrics["tokens"] = i
		default:
			metrics[k] = i
		}
	}
	return metrics
}

// Ensure our tracer implements the shared interface.
var _ internal.MiddlewareTracer = &chatCompletionsTracer{}
