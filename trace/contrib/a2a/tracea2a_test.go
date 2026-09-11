package tracea2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
)

// Tests use a real local httptest server speaking JSON-RPC rather than VCR:
// both sides are this SDK's own code, so there's no external API to record.

type echoExecutor struct{}

func (echoExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "hi " + textOf(reqCtx.Message)})
	return q.Write(ctx, reply)
}

func (echoExecutor) Cancel(_ context.Context, _ *a2asrv.RequestContext, _ eventqueue.Queue) error {
	return nil
}

// multiEventExecutor emits two artifact events then a final status event -
// a real task-based stream, unlike echoExecutor's single terminal Message.
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

	require.Equal(t, clientSpan.Stub.SpanContext.TraceID(), serverSpan.Stub.SpanContext.TraceID(),
		"client and server spans must share one trace")
	require.Equal(t, clientSpan.Stub.SpanContext.SpanID(), serverSpan.Stub.Parent.SpanID(),
		"server span must be a child of the client span")
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

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	require.Contains(t, clientSpan.Attr("braintrust.output_json").String(), "hi Braintrust")
}

// Proves non-terminal events don't end the span early, and output
// accumulates every event's content instead of just the last one.
func TestInstrumentClient_SendStreamingMessage_MultiEvent(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServerWithExecutor(t, multiEventExecutor{})
	client := setupClient(t, server.URL)

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

// countingTracerProvider counts every Tracer.Start call, catching an
// orphaned span (created but never ended) that exported-span counting alone
// would miss.
type countingTracerProvider struct {
	trace.TracerProvider
	starts *atomic.Int64
}

func (p *countingTracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return &countingTracer{Tracer: p.TracerProvider.Tracer(name, opts...), starts: p.starts}
}

type countingTracer struct {
	trace.Tracer
	starts *atomic.Int64
}

func (t *countingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	t.starts.Add(1)
	return t.Tracer.Start(ctx, name, opts...)
}

// Simulates Orchestrion and a manual InstrumentClient call each attaching
// their own interceptor instance to the same client.
func TestDoubleInstrumentation_TwoInterceptorInstances(t *testing.T) {
	tp, exporter := oteltest.Setup(t)
	var starts atomic.Int64
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(&countingTracerProvider{TracerProvider: tp, starts: &starts})
	t.Cleanup(func() { otel.SetTracerProvider(original) })

	server := setupServer(t)

	client, err := a2aclient.NewFromEndpoints(t.Context(), []a2a.AgentInterface{
		{Transport: a2a.TransportProtocolJSONRPC, URL: server.URL + "/invoke"},
	})
	require.NoError(t, err)
	defer func() { _ = client.Destroy() }()

	client.AddCallInterceptor(NewClientInterceptor()) // e.g. Orchestrion
	InstrumentClient(client)                          // e.g. a manual call on top

	_, err = client.SendMessage(context.Background(), &a2a.MessageSendParams{
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
	assert.Equal(t, 1, clientSpans, "two independently attached interceptors must still produce exactly one exported span")

	// 1 client span + 1 server span; a redundant interceptor must not call Start.
	assert.Equal(t, int64(2), starts.Load(),
		"exactly one client-side span and one server-side span must be started - a second client interceptor must not call Start at all, not just fail to export")
}

func TestInstrumentClient_StreamAbandonedOnContextCancel(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServerWithExecutor(t, multiEventExecutor{})
	client := setupClient(t, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var gotFirstEvent bool
	for event, err := range client.SendStreamingMessage(ctx, &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "go"}),
	}) {
		require.NoError(t, err)
		require.IsType(t, &a2a.TaskArtifactUpdateEvent{}, event, "must abandon before a terminal event to prove the fallback, not the normal path")
		gotFirstEvent = true
		cancel()
		break
	}
	require.True(t, gotFirstEvent)

	var spans []oteltest.Span
	require.Eventually(t, func() bool {
		spans = exporter.Flush()
		return len(spans) > 0
	}, time.Second, 10*time.Millisecond, "span must eventually end once the call context is canceled")

	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	assert.Equal(t, codes.Error, clientSpan.Status().Code)
	assert.Contains(t, clientSpan.Status().Description, "abandoned")
}

// Regression test for a data race: context.AfterFunc calls its callback
// immediately, in its own goroutine, if ctx is already done when
// registered. Only fails under -race.
func TestStartCall_AlreadyDoneContext_NoRace(t *testing.T) {
	setupOtel(t)

	ct := &clientTracer{tr: tracer()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done before Before is ever called

	for range 200 {
		_, err := ct.Before(ctx, &a2aclient.Request{Method: "SendMessage"})
		require.NoError(t, err)
	}
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
