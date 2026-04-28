package openai

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
)

func newTestResponsesTracer(t *testing.T) (*responsesTracer, *oteltest.Exporter) {
	t.Helper()
	tp, exporter := oteltest.Setup(t)
	rt := newResponsesTracer(&middlewareConfig{tracerProvider: tp})
	rt.startTime = time.Now()
	return rt, exporter
}

func parseSSE(t *testing.T, rt *responsesTracer, exporter *oteltest.Exporter, sseBody string) oteltest.Span {
	t.Helper()
	tp, _ := oteltest.Setup(t)
	_, span := tp.Tracer("test").Start(context.Background(), "test")

	// re-use exporter from the tracer
	_, expSpan := rt.cfg.tracerProvider.Tracer("test").Start(context.Background(), "test")
	err := rt.parseStreamingResponse(expSpan, strings.NewReader(sseBody))
	require.NoError(t, err)
	expSpan.End()
	_ = span

	return exporter.FlushOne()
}

// TestResponsesIncompleteStreaming verifies that response.incomplete events
// are captured and status/usage/incomplete_details appear in metadata.
func TestResponsesIncompleteStreaming(t *testing.T) {
	rt, exporter := newTestResponsesTracer(t)

	sseBody := `event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_001","output_index":0,"content_index":0,"delta":"Paris","logprobs":[]}

event: response.incomplete
data: {"type":"response.incomplete","sequence_number":5,"response":{"id":"resp_001","object":"response","created_at":1700000000,"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"error":null,"output":[{"id":"msg_001","type":"message","status":"incomplete","content":[{"type":"output_text","text":"Paris","annotations":[]}],"role":"assistant"}],"model":"gpt-4o-mini","usage":{"input_tokens":25,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":30},"metadata":{}}}

 `

	ts := parseSSE(t, rt, exporter, sseBody)
	assert := assert.New(t)

	metadata := ts.Metadata()
	assert.Equal("incomplete", metadata["status"])

	usage, ok := metadata["usage"].(map[string]any)
	require.True(t, ok, "usage should be present in metadata")
	assert.Equal(float64(25), usage["input_tokens"])
	assert.Equal(float64(5), usage["output_tokens"])
	assert.Equal(float64(30), usage["total_tokens"])
}

// TestResponsesFailedStreaming verifies that response.failed events are captured
// and status appears in metadata.
func TestResponsesFailedStreaming(t *testing.T) {
	rt, exporter := newTestResponsesTracer(t)

	sseBody := `event: response.failed
data: {"type":"response.failed","sequence_number":3,"response":{"id":"resp_002","object":"response","created_at":1700000000,"status":"failed","error":{"code":"server_error","message":"internal server error"},"incomplete_details":null,"output":[],"model":"gpt-4o-mini","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":10},"metadata":{}}}

`

	ts := parseSSE(t, rt, exporter, sseBody)
	assert := assert.New(t)

	metadata := ts.Metadata()
	assert.Equal("failed", metadata["status"])

	usage, ok := metadata["usage"].(map[string]any)
	require.True(t, ok, "usage should be present in metadata even for failed responses")
	assert.Equal(float64(10), usage["total_tokens"])
}

// TestResponsesCompletedStreamingUsage verifies that usage is captured in metadata
// for successful streaming responses.
func TestResponsesCompletedStreamingUsage(t *testing.T) {
	rt, exporter := newTestResponsesTracer(t)

	sseBody := `event: response.completed
data: {"type":"response.completed","sequence_number":10,"response":{"id":"resp_003","object":"response","created_at":1700000000,"status":"completed","error":null,"incomplete_details":null,"output":[{"id":"msg_003","type":"message","status":"completed","content":[{"type":"output_text","text":"Paris","annotations":[]}],"role":"assistant"}],"model":"gpt-4o-mini","usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":0},"output_tokens":10,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":30},"metadata":{}}}

`

	ts := parseSSE(t, rt, exporter, sseBody)
	assert := assert.New(t)

	metadata := ts.Metadata()
	assert.Equal("completed", metadata["status"])

	usage, ok := metadata["usage"].(map[string]any)
	require.True(t, ok, "usage should be present in metadata for completed responses")
	assert.Equal(float64(20), usage["input_tokens"])
	assert.Equal(float64(10), usage["output_tokens"])
	assert.Equal(float64(30), usage["total_tokens"])

	// usage tokens should also appear in metrics
	metrics := ts.Metrics()
	assert.Greater(metrics["tokens"], float64(0))
}
