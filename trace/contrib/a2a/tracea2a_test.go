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

	// The client and server run over a real HTTP wire (JSON-RPC), so without
	// explicit propagation the server span would start a brand new,
	// unrelated trace instead of continuing the client's - this asserts the
	// traceparent injected into CallMeta (client) and extracted from
	// RequestMeta (server) actually links them into one trace.
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
// the same client attaches only one interceptor's worth of tracing per call -
// the ownership check in startCall makes any redundant attachment a no-op.
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

// countingTracerProvider wraps a TracerProvider to count every Tracer.Start
// call, independent of whether the resulting span is ever ended/exported.
// This catches a narrower bug than counting exported spans: if the ownership
// check in startCall's early-return were missing, a second attached
// interceptor's Before would still call tr.Start() and create a real span -
// it just wouldn't be the one an After call ends, so it silently leaks
// without ever reaching the exporter. Counting exported spans alone can't
// see that leak; counting Start calls can.
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

// TestDoubleInstrumentation_TwoInterceptorInstances simulates the scenario
// the package doc warns about: Orchestrion attaching one interceptor
// instance to a client while a manual InstrumentClient call attaches a
// second, independently-created one. This is a stronger check than
// TestInstrumentClient_Idempotent, which only covers calling the same
// InstrumentClient function twice - here the two interceptors are genuinely
// different objects, exactly like Orchestrion's own NewClientInterceptor()
// call combined with a manual InstrumentClient call would produce.
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

	// The server side also gets a real request through the same client call,
	// so subtract its one Start to isolate the client-side count.
	assert.Equal(t, int64(2), starts.Load(),
		"exactly one client-side span and one server-side span must be started - a second client interceptor must not call Start at all, not just fail to export")
}

// TestInstrumentClient_StreamAbandonedOnContextCancel proves the
// context.AfterFunc fallback in startCall: if a caller cancels a streaming
// call's context after receiving a non-terminal event and before a terminal
// one arrives, the span still ends (marked as abandoned) instead of leaking
// forever - closing the gap the package doc previously only documented.
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

	// context.AfterFunc runs its callback in its own goroutine, so the span
	// may not be ended the instant cancel() returns.
	var spans []oteltest.Span
	require.Eventually(t, func() bool {
		spans = exporter.Flush()
		return len(spans) > 0
	}, time.Second, 10*time.Millisecond, "span must eventually end once the call context is canceled")

	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	assert.Equal(t, codes.Error, clientSpan.Status().Code)
	assert.Contains(t, clientSpan.Status().Description, "abandoned")
}

// TestStartCall_AlreadyDoneContext_NoRace guards against a real data race
// found in review: context.AfterFunc calls its callback immediately, in its
// own goroutine, if ctx is already done when registered. That callback calls
// callState.end, which reads state.stopWatch - concurrently with startCall's
// own goroutine still assigning that field from context.AfterFunc's return
// value. Run with -race; this only fails under -race, not on a plain `go
// test` run, since the race itself doesn't corrupt any observable state here.
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
