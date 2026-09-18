package openai

// this file parses the chat completions API.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// chatCompletionsTracer is a tracer for the openai v1/chat/completions POST endpoint.
// See docs here: https://platform.openai.com/docs/api-reference/chat/create
type chatCompletionsTracer struct {
	cfg       *middlewareConfig
	streaming bool
	metadata  map[string]any
	startTime time.Time
}

func newChatCompletionsTracer(cfg *middlewareConfig) *chatCompletionsTracer {
	return &chatCompletionsTracer{
		cfg:       cfg,
		streaming: false,
		metadata: map[string]any{
			"provider": "openai",
			"endpoint": "/v1/chat/completions",
		},
	}
}

func (ct *chatCompletionsTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	ct.startTime = t
	ctx, span := ct.cfg.tracer().Start(
		ctx,
		"Chat Completion",
		trace.WithTimestamp(t),
	)

	var raw map[string]interface{}
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, span, err
	}

	metadataFields := []string{
		"model",
		"frequency_penalty",
		"logit_bias",
		"logprobs",
		"max_tokens",
		"max_completion_tokens",
		"n",
		"presence_penalty",
		"reasoning_effort",
		"response_format",
		"seed",
		"service_tier",
		"stop",
		"stream",
		"stream_options",
		"temperature",
		"top_p",
		"top_logprobs",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
		"user",
		"prompt_cache_key",
		"safety_identifier",
		"functions",
		"function_call",
		"web_search_options",
		"prediction",
	}

	// handle simple fields here.
	for _, field := range metadataFields {
		if value, exists := raw[field]; exists {
			ct.metadata[field] = value
			// keep track of streaming requests so we can parse the streaming response later.
			if field == "stream" {
				if value, ok := value.(bool); ok {
					ct.streaming = value
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
	if ct.streaming {
		return ct.parseStreamingResponse(span, body)
	}
	return ct.parseResponse(span, body)
}

func (ct *chatCompletionsTracer) parseStreamingResponse(span trace.Span, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	var allResults []map[string]any
	var timeToFirstToken time.Duration

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		line = strings.TrimPrefix(line, "data: ")
		if line == "[DONE]" {
			break // End of stream
		}

		if timeToFirstToken == 0 {
			timeToFirstToken = time.Since(ct.startTime)
		}

		var chunk map[string]any
		err := json.Unmarshal([]byte(line), &chunk)
		if err != nil {
			return err
		}

		allResults = append(allResults, chunk)

		// Handle usage in streaming response (if stream_options.include_usage is true)
		if usage, ok := chunk["usage"]; ok {
			ct.metadata["usage"] = usage
		}
	}

	// Post-process streaming results to match Python SDK behavior
	output := ct.postprocessStreamingResults(allResults)
	if output != nil {
		if err := internal.SetJSONAttr(span, "braintrust.output_json", output); err != nil {
			return err
		}
	}

	// Handle usage metrics
	metrics := make(map[string]any)
	if usage, ok := ct.metadata["usage"].(map[string]any); ok {
		tokenMetrics := parseUsageTokens(usage)
		for k, v := range tokenMetrics {
			metrics[k] = v
		}
	}
	metrics["time_to_first_token"] = timeToFirstToken.Seconds()
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	return scanner.Err()
}

// choiceAccumulator collects one choice index's worth of streamed deltas.
// With n > 1, chunks for different choice indices interleave, so each index
// needs its own accumulator rather than one shared across all of them.
type choiceAccumulator struct {
	role            *string
	content         string
	refusal         string
	toolCalls       []interface{}
	annotations     []interface{}
	logprobsContent []interface{}
	finishReason    interface{}
}

func (ct *chatCompletionsTracer) postprocessStreamingResults(allResults []map[string]any) []map[string]interface{} {
	accumulators := map[int64]*choiceAccumulator{}
	var order []int64

	for _, result := range allResults {
		choices, ok := result["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		for _, choice := range choices {
			choiceMap, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			delta, ok := choiceMap["delta"].(map[string]any)
			if !ok {
				continue
			}

			ok, index := internal.ToInt64(choiceMap["index"])
			if !ok {
				index = 0
			}
			acc, exists := accumulators[index]
			if !exists {
				acc = &choiceAccumulator{}
				accumulators[index] = acc
				order = append(order, index)
			}

			// Handle role (set once from first delta that has it)
			if acc.role == nil {
				if deltaRole, ok := delta["role"].(string); ok {
					acc.role = &deltaRole
				}
			}

			// Handle finish_reason
			if fr, ok := choiceMap["finish_reason"]; ok && fr != nil {
				acc.finishReason = fr
			}

			// Handle content aggregation
			if deltaContent, ok := delta["content"].(string); ok {
				acc.content += deltaContent
			}

			// Handle refusal aggregation. Streamed the same way as content,
			// but on a separate field: the model uses one or the other, not
			// both, per chunk.
			if deltaRefusal, ok := delta["refusal"].(string); ok {
				acc.refusal += deltaRefusal
			}

			// Handle URL citation annotations (arrives whole in one chunk,
			// not built up token-by-token like content).
			if deltaAnnotations, ok := delta["annotations"].([]interface{}); ok {
				acc.annotations = append(acc.annotations, deltaAnnotations...)
			}

			// Handle logprobs. Unlike delta, this is a choice-level field
			// (not nested under delta), and its content array is built up
			// per token across chunks the same way content is.
			if logprobs, ok := choiceMap["logprobs"].(map[string]any); ok {
				if logprobsDelta, ok := logprobs["content"].([]interface{}); ok {
					acc.logprobsContent = append(acc.logprobsContent, logprobsDelta...)
				}
			}

			// Handle tool_calls aggregation (similar to Python SDK logic)
			if deltaToolCalls, ok := delta["tool_calls"].([]interface{}); ok && len(deltaToolCalls) > 0 {
				if toolDelta, ok := deltaToolCalls[0].(map[string]any); ok {
					// Check if this is a new tool call or continuation
					if toolID, ok := toolDelta["id"].(string); ok && toolID != "" {
						// New tool call - check if we need to create a new one
						isNewToolCall := len(acc.toolCalls) == 0
						if !isNewToolCall {
							// Safe to access last tool call since slice is not empty
							if lastTool, ok := acc.toolCalls[len(acc.toolCalls)-1].(map[string]interface{}); ok {
								isNewToolCall = lastTool["id"] != toolID
							} else {
								isNewToolCall = true // type assertion failed, treat as new
							}
						}
						if isNewToolCall {
							newToolCall := map[string]interface{}{
								"id":   toolID,
								"type": toolDelta["type"],
							}
							if function, ok := toolDelta["function"].(map[string]any); ok {
								newToolCall["function"] = function
							}
							acc.toolCalls = append(acc.toolCalls, newToolCall)
						}
					} else if len(acc.toolCalls) > 0 {
						// Continuation of existing tool call - append arguments
						if lastTool, ok := acc.toolCalls[len(acc.toolCalls)-1].(map[string]interface{}); ok {
							if function, ok := lastTool["function"].(map[string]interface{}); ok {
								if deltaFunction, ok := toolDelta["function"].(map[string]any); ok {
									if args, ok := deltaFunction["arguments"].(string); ok {
										if currentArgs, ok := function["arguments"].(string); ok {
											function["arguments"] = currentArgs + args
										} else {
											function["arguments"] = args
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}

	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	results := make([]map[string]interface{}, 0, len(order))
	for _, index := range order {
		acc := accumulators[index]

		var finalRole interface{}
		if acc.role != nil {
			finalRole = *acc.role
		}

		var finalToolCalls interface{}
		if len(acc.toolCalls) > 0 {
			finalToolCalls = acc.toolCalls
		}

		var finalAnnotations interface{}
		if len(acc.annotations) > 0 {
			finalAnnotations = acc.annotations
		}

		var finalLogprobs interface{}
		if len(acc.logprobsContent) > 0 {
			finalLogprobs = map[string]interface{}{"content": acc.logprobsContent}
		}

		var finalRefusal interface{}
		if acc.refusal != "" {
			finalRefusal = acc.refusal
		}

		results = append(results, map[string]interface{}{
			"index": index,
			"message": map[string]interface{}{
				"role":        finalRole,
				"content":     acc.content,
				"refusal":     finalRefusal,
				"tool_calls":  finalToolCalls,
				"annotations": finalAnnotations,
			},
			"logprobs":      finalLogprobs,
			"finish_reason": acc.finishReason,
		})
	}

	return results
}

func (ct *chatCompletionsTracer) parseResponse(span trace.Span, body io.Reader) error {
	// Capture time to first token for non-streaming requests
	timeToFirstToken := time.Since(ct.startTime)

	var raw map[string]interface{}
	err := json.NewDecoder(body).Decode(&raw)
	if err != nil {
		return err
	}

	return ct.handleChatCompletionResponse(span, raw, timeToFirstToken)
}

func (ct *chatCompletionsTracer) handleChatCompletionResponse(span trace.Span, rawMsg map[string]any, timeToFirstToken time.Duration) error {
	metadataFields := []string{
		"id",
		"object",
		"created",
		"system_fingerprint",
		"service_tier",
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
		tokenMetrics := parseUsageTokens(usage)
		for k, v := range tokenMetrics {
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
