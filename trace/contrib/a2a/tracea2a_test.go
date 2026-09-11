package tracea2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
)

// A2A tests use a real local httptest server speaking JSON-RPC rather than
// VCR: both the client and server are this SDK's own code, so there is no
// external API to record a cassette against.

type echoExecutor struct{}

func (echoExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "hi " + textOf(reqCtx.Message)})
	return q.Write(ctx, reply)
}

func (echoExecutor) Cancel(_ context.Context, _ *a2asrv.RequestContext, _ eventqueue.Queue) error {
	return nil
}

// multiEventExecutor emits two non-terminal TaskArtifactUpdateEvents, then a
// final TaskStatusUpdateEvent - a real task-based stream shape, unlike
// echoExecutor's single terminal Message. The final status event carries no
// artifact content of its own, so a correct implementation must accumulate
// the artifacts rather than overwrite output with just the last event.
type multiEventExecutor struct{}

func (multiEventExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	if err := q.Write(ctx, a2a.NewArtifactEvent(reqCtx, a2a.TextPart{Text: "part one"})); err != nil {
		return err
	}
	if err := q.Write(ctx, a2a.NewArtifactEvent(reqCtx, a2a.TextPart{Text: "part two"})); err != nil {
		return err
	}
	final := a2a.NewStatusUpdateEvent(reqCtx, a2a.TaskStateCompleted, nil)
	final.Final = true
	return q.Write(ctx, final)
}

func (multiEventExecutor) Cancel(_ context.Context, _ *a2asrv.RequestContext, _ eventqueue.Queue) error {
	return nil
}

func textOf(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	for _, part := range msg.Parts {
		if text, ok := part.(a2a.TextPart); ok {
			return text.Text
		}
	}
	return ""
}

func setupServerWithExecutor(t *testing.T, executor a2asrv.AgentExecutor) *httptest.Server {
	t.Helper()
	handler := a2asrv.NewHandler(executor, InstrumentServer())
	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func setupServer(t *testing.T) *httptest.Server {
	t.Helper()
	return setupServerWithExecutor(t, echoExecutor{})
}

func setupClient(t *testing.T, serverURL string) *a2aclient.Client {
	t.Helper()
	client, err := a2aclient.NewFromEndpoints(t.Context(), []a2a.AgentInterface{
		{Transport: a2a.TransportProtocolJSONRPC, URL: serverURL + "/invoke"},
	})
	require.NoError(t, err)
	InstrumentClient(client)
	t.Cleanup(func() { _ = client.Destroy() })
	return client
}

func setupOtel(t *testing.T) *oteltest.Exporter {
	t.Helper()
	tp, exporter := oteltest.Setup(t)
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(original) })
	return exporter
}

func TestInstrumentClient_SendMessage(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	result, err := client.SendMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendMessage", "client")
	require.Equal(t, "a2a", clientSpan.Metadata()["provider"])
	require.Contains(t, clientSpan.Attr("braintrust.output_json").String(), "hi Braintrust")
	clientSpan.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "task"})

	serverSpan := findSpanWithRole(t, spans, "a2a.OnSendMessage", "server")
	require.Equal(t, "server", serverSpan.Metadata()["role"])
}

func TestInstrumentClient_SendStreamingMessage(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	var gotFinal bool
	for event, err := range client.SendStreamingMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	}) {
		require.NoError(t, err)
		require.NotNil(t, event)
		gotFinal = true
	}
	require.True(t, gotFinal, "should have received at least one event")

	// The span must be ended (exported) even though echoExecutor's reply is a
	// single terminal Message rather than a TaskStatusUpdateEvent - this is
	// the case the isTerminalEvent fix covers.
	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	require.Contains(t, clientSpan.Attr("braintrust.output_json").String(), "hi Braintrust")
}

// TestInstrumentClient_SendStreamingMessage_MultiEvent proves two things
// TestInstrumentClient_SendStreamingMessage's single-event case cannot:
//  1. Non-terminal events do not end the span early - no span is exported
//     until the terminal TaskStatusUpdateEvent arrives.
//  2. The output reflects every event's content, not just the last one - the
//     final status event alone carries no artifact text.
func TestInstrumentClient_SendStreamingMessage_MultiEvent(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServerWithExecutor(t, multiEventExecutor{})
	client := setupClient(t, server.URL)

	// The SDK calls the interceptor's After hook for an event before handing
	// that event to this loop's body (client.go's SendStreamingMessage calls
	// interceptAfter, then yield). So checking exporter state inside the loop,
	// right after receiving a non-terminal event, correctly reflects whether
	// that event's After call exported a span - it must not have.
	var eventCount int
	for event, err := range client.SendStreamingMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "go"}),
	}) {
		require.NoError(t, err)
		eventCount++
		if statusUpdate, ok := event.(*a2a.TaskStatusUpdateEvent); ok {
			require.True(t, statusUpdate.Final)
			continue
		}
		require.IsType(t, &a2a.TaskArtifactUpdateEvent{}, event)
		require.Empty(t, exporter.Flush(), "span must not be exported after a non-terminal event")
	}
	require.Equal(t, 3, eventCount)

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	output := clientSpan.Attr("braintrust.output_json").String()
	assert.Contains(t, output, "part one", "first artifact must survive in accumulated output")
	assert.Contains(t, output, "part two", "second artifact must survive in accumulated output")
	assert.Contains(t, output, "completed", "final status must also be present in accumulated output")
}

func TestInstrumentServer_TracesErrors(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	_, err := client.GetTask(context.Background(), &a2a.TaskQueryParams{ID: "does-not-exist"})
	require.Error(t, err)

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.GetTask", "client")
	require.Equal(t, "a2a", clientSpan.Metadata()["provider"])

	assert.Equal(t, codes.Error, clientSpan.Status().Code)
	assert.NotEmpty(t, clientSpan.Status().Description)

	events := clientSpan.Events()
	require.NotEmpty(t, events, "span must record an exception event on error")
	var recordedErr string
	for _, event := range events {
		for _, attr := range event.Attributes {
			if string(attr.Key) == "exception.message" {
				recordedErr = attr.Value.AsString()
			}
		}
	}
	assert.NotEmpty(t, recordedErr, "exception.message must be recorded on the span")
}

// TestInstrumentClient_Idempotent proves calling InstrumentClient twice on
// the same client attaches only one interceptor - without the sync.Map
// guard, this would produce two client spans per call and leak the first
// interceptor's span (see the package doc's double-instrumentation note).
func TestInstrumentClient_Idempotent(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL) // already instrumented once by setupClient
	InstrumentClient(client)             // second call must be a no-op

	_, err := client.SendMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	})
	require.NoError(t, err)

	spans := exporter.Flush()
	clientSpans := 0
	for _, span := range spans {
		if span.Name() == "a2a.SendMessage" && span.Metadata()["role"] == "client" {
			clientSpans++
		}
	}
	assert.Equal(t, 1, clientSpans, "double InstrumentClient must not attach a duplicate interceptor")
}

func TestSetOutput_TaskTypeOnlyForTaskMethods(t *testing.T) {
	tp, exporter := oteltest.Setup(t)
	tracer := tp.Tracer("test")

	_, taskSpan := tracer.Start(context.Background(), "task-method")
	setOutput(taskSpan, "SendMessage", []any{"result"})
	taskSpan.End()

	_, adminSpan := tracer.Start(context.Background(), "admin-method")
	setOutput(adminSpan, "GetAgentCard", []any{"card"})
	adminSpan.End()

	spans := exporter.Flush()
	require.Len(t, spans, 2)

	got := findSpanWithRole(t, spans, "task-method", "")
	got.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "task"})

	got = findSpanWithRole(t, spans, "admin-method", "")
	assert.False(t, got.HasAttr("braintrust.span_attributes"), "admin/config methods must not be labeled type=task")
}

func findSpanWithRole(t *testing.T, spans []oteltest.Span, name, role string) oteltest.Span {
	t.Helper()
	for _, span := range spans {
		if span.Name() != name {
			continue
		}
		if role == "" || span.Metadata()["role"] == role {
			return span
		}
	}
	t.Fatalf("no span named %q with role %q found among %d spans", name, role, len(spans))
	return oteltest.Span{}
}
