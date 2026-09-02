package assemblyai

// this file parses AssemblyAI's LeMUR generation endpoints: Task, Summarize,
// ActionItems (all string responses), and Question (a list of Q&A pairs).
// They share an identical request/response envelope, differing only in a
// couple of request fields and the shape of the "response" field.

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// lemurTracer is a tracer shared by all four LeMUR generation endpoints.
type lemurTracer struct {
	cfg      *config
	spanName string
	endpoint string
	outputFn func(raw map[string]any) any
	metadata map[string]any
}

func newLeMURTracer(cfg *config, spanName, endpoint string, outputFn func(map[string]any) any) *lemurTracer {
	return &lemurTracer{
		cfg:      cfg,
		spanName: spanName,
		endpoint: endpoint,
		outputFn: outputFn,
		metadata: map[string]any{
			"provider": "assemblyai",
			"endpoint": endpoint,
		},
	}
}

func (t *lemurTracer) StartSpan(ctx context.Context, start time.Time, request io.Reader) (context.Context, trace.Span, error) {
	ctx, span := t.cfg.tracer().Start(ctx, t.spanName, trace.WithTimestamp(start))

	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "llm"}); err != nil {
		return ctx, span, err
	}

	var raw map[string]any
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, span, err
	}

	if model, ok := raw["final_model"].(string); ok && model != "" {
		t.metadata["model"] = model
	}
	if temperature, exists := raw["temperature"]; exists {
		t.metadata["temperature"] = temperature
	}
	if maxOutputSize, exists := raw["max_output_size"]; exists {
		t.metadata["max_output_size"] = maxOutputSize
	}
	// TranscriptIDs/InputText can carry up to 100 hours of transcript content,
	// so only a reference (ids, or the input text's length) is recorded rather
	// than the transcript body itself.
	if transcriptIDs, ok := raw["transcript_ids"].([]any); ok && len(transcriptIDs) > 0 {
		t.metadata["transcript_ids"] = transcriptIDs
	}
	if inputText, ok := raw["input_text"].(string); ok && inputText != "" {
		t.metadata["input_text_length"] = len(inputText)
	}

	if err := internal.SetJSONAttr(span, "braintrust.input_json", lemurInput(raw)); err != nil {
		return ctx, span, err
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", t.metadata); err != nil {
		return ctx, span, err
	}

	return ctx, span, nil
}

func (t *lemurTracer) TagSpan(span trace.Span, response io.Reader) error {
	var raw map[string]any
	if err := json.NewDecoder(response).Decode(&raw); err != nil {
		return err
	}

	if requestID, ok := raw["request_id"].(string); ok && requestID != "" {
		t.metadata["request_id"] = requestID
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", t.metadata); err != nil {
		return err
	}

	if err := internal.SetJSONAttr(span, "braintrust.output_json", t.outputFn(raw)); err != nil {
		return err
	}

	metrics := make(map[string]any)
	if usage, ok := raw["usage"].(map[string]any); ok {
		for k, v := range parseLeMURUsageTokens(usage) {
			metrics[k] = v
		}
	}
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	return nil
}

// lemurInput extracts the request fields relevant to what was actually asked
// (prompt / context / answer_format / questions), leaving out transcript
// content per the metadata handling above.
func lemurInput(raw map[string]any) map[string]any {
	input := make(map[string]any)
	for _, key := range []string{"prompt", "context", "answer_format", "questions"} {
		if value, exists := raw[key]; exists {
			input[key] = value
		}
	}
	return input
}

// lemurOutputResponse extracts the "response" field verbatim. Used by Task,
// Summarize, and ActionItems, which all return a plain string.
func lemurOutputResponse(raw map[string]any) any {
	return raw["response"]
}

// lemurOutputQuestionAnswer extracts Question's "response" field, a list of
// {question, answer} pairs.
func lemurOutputQuestionAnswer(raw map[string]any) any {
	return raw["response"]
}

// parseLeMURUsageTokens normalizes LeMUR's usage object to Braintrust metric names.
func parseLeMURUsageTokens(usage map[string]any) map[string]int64 {
	metrics := make(map[string]int64)
	inputOK, input := internal.ToInt64(usage["input_tokens"])
	if inputOK {
		metrics["prompt_tokens"] = input
	}
	outputOK, output := internal.ToInt64(usage["output_tokens"])
	if outputOK {
		metrics["completion_tokens"] = output
	}
	if inputOK && outputOK {
		metrics["tokens"] = input + output
	}
	return metrics
}

// Ensure our tracer implements the shared interface.
var _ internal.MiddlewareTracer = &lemurTracer{}
