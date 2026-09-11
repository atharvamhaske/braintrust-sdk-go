// Package tracea2a provides OpenTelemetry tracing for Agent2Agent (A2A)
// protocol clients and servers using the official Go SDK
// (github.com/a2aproject/a2a-go).
//
//	client, err := a2aclient.NewFromCard(ctx, card)
//	tracea2a.InstrumentClient(client)
//
//	requestHandler := a2asrv.NewHandler(executor, tracea2a.InstrumentServer())
package tracea2a

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// Explicit instance instead of otel.GetTextMapPropagator(), which defaults
// to a no-op unless the app configured one itself.
var a2aPropagator = propagation.TraceContext{}

// callMetaCarrier adapts a2aclient.CallMeta to propagation.TextMapCarrier.
type callMetaCarrier a2aclient.CallMeta

func (c callMetaCarrier) Get(key string) string {
	vals := a2aclient.CallMeta(c).Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (c callMetaCarrier) Set(key, value string) {
	a2aclient.CallMeta(c).Append(key, value)
}

func (c callMetaCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// requestMetaCarrier adapts a2asrv.RequestMeta to propagation.TextMapCarrier.
type requestMetaCarrier struct {
	meta *a2asrv.RequestMeta
}

func (c requestMetaCarrier) Get(key string) string {
	vals, ok := c.meta.Get(key)
	if !ok || len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func (c requestMetaCarrier) Set(string, string) {} // extraction only

func (c requestMetaCarrier) Keys() []string {
	var keys []string
	for k := range c.meta.List() {
		keys = append(keys, k)
	}
	return keys
}

// streamingMethods' response is an iter.Seq2[a2a.Event, error]: After fires
// once per event instead of once per call.
var streamingMethods = map[string]bool{
	"SendStreamingMessage": true,
	"ResubscribeToTask":    true,
	"OnSendMessageStream":  true,
	"OnResubscribeToTask":  true,
}

// taskMethods operate on an actual A2A task, as opposed to admin/config
// calls (AgentCard, push notification config). Only these get
// span_attributes.type = "task".
var taskMethods = map[string]bool{
	"SendMessage":          true,
	"SendStreamingMessage": true,
	"GetTask":              true,
	"CancelTask":           true,
	"ResubscribeToTask":    true,
	"OnSendMessage":        true,
	"OnSendMessageStream":  true,
	"OnGetTask":            true,
	"OnCancelTask":         true,
	"OnResubscribeToTask":  true,
}

type spanContextKey struct{}

// callState accumulates every event payload seen across a call so the final
// output reflects everything produced, not just the last event.
//
// owner is the interceptor instance that created this state. If a second
// tracing interceptor is attached (Orchestrion + manual, or manual twice),
// its Before/After sees state already present and no-ops instead of
// duplicating the span.
type callState struct {
	mu     sync.Mutex
	span   trace.Span
	method string
	events []any
	owner  any
	ended  bool

	stopWatch func() bool
}

func (s *callState) addEvent(payload any) {
	if payload == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, payload)
}

func (s *callState) snapshotEvents() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.events...)
}

// setStopWatch is mutex-guarded because context.AfterFunc invokes its
// callback immediately, in its own goroutine, when ctx is already done -
// racing against the assignment of its own stop function.
func (s *callState) setStopWatch(stop func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopWatch = stop
}

// end runs exactly once per call, however it's triggered.
func (s *callState) end(fn func()) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	stop := s.stopWatch
	s.mu.Unlock()

	fn()
	s.span.End()
	if stop != nil {
		stop()
	}
}

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("braintrust")
}

// InstrumentClient adds Braintrust tracing to an a2aclient.Client.
func InstrumentClient(client *a2aclient.Client) {
	if client == nil {
		return
	}
	client.AddCallInterceptor(NewClientInterceptor())
}

// NewClientInterceptor is exposed for a2aclient.WithInterceptors(...); most
// callers should use InstrumentClient instead.
func NewClientInterceptor() a2aclient.CallInterceptor {
	return &clientTracer{tr: tracer()}
}

// InstrumentServer returns a RequestHandlerOption for a2asrv.NewHandler.
func InstrumentServer() a2asrv.RequestHandlerOption {
	return a2asrv.WithCallInterceptor(&serverTracer{tr: tracer()})
}

// startCall starts a span, stores its callState in ctx under owner, and
// arms a context.AfterFunc that ends the span as abandoned if ctx finishes
// before finish() does. No-ops if ctx already carries a callState.
func startCall(ctx context.Context, tr trace.Tracer, name, method, role string, kind trace.SpanKind, payload any, owner any) context.Context {
	if _, exists := ctx.Value(spanContextKey{}).(*callState); exists {
		return ctx
	}

	ctx, span := tr.Start(ctx, name, trace.WithSpanKind(kind))
	setMetadata(span, method, role)
	setInput(span, payload)

	state := &callState{span: span, method: method, owner: owner}
	stop := context.AfterFunc(ctx, func() {
		state.end(func() {
			setOutput(state.span, state.method, state.snapshotEvents())
			state.span.SetStatus(codes.Error, "stream abandoned: call context ended before a terminal event")
		})
	})
	state.setStopWatch(stop)

	return context.WithValue(ctx, spanContextKey{}, state)
}

// clientTracer implements a2aclient.CallInterceptor.
type clientTracer struct {
	a2aclient.PassthroughInterceptor
	tr trace.Tracer
}

func (c *clientTracer) Before(ctx context.Context, req *a2aclient.Request) (context.Context, error) {
	ctx = startCall(ctx, c.tr, "a2a."+req.Method, req.Method, "client", trace.SpanKindClient, req.Payload, c)
	if req.Meta == nil {
		req.Meta = a2aclient.CallMeta{}
	}
	a2aPropagator.Inject(ctx, callMetaCarrier(req.Meta))
	return ctx, nil
}

func (c *clientTracer) After(ctx context.Context, resp *a2aclient.Response) error {
	state, ok := ctx.Value(spanContextKey{}).(*callState)
	if !ok || state.owner != c {
		return nil
	}
	finish(state, resp.Payload, resp.Err, streamingMethods[resp.Method])
	return nil
}

// serverTracer implements a2asrv.CallInterceptor.
type serverTracer struct {
	a2asrv.PassthroughCallInterceptor
	tr trace.Tracer
}

func (s *serverTracer) Before(ctx context.Context, callCtx *a2asrv.CallContext, req *a2asrv.Request) (context.Context, error) {
	ctx = a2aPropagator.Extract(ctx, requestMetaCarrier{meta: callCtx.RequestMeta()})
	return startCall(ctx, s.tr, "a2a."+callCtx.Method(), callCtx.Method(), "server", trace.SpanKindServer, req.Payload, s), nil
}

func (s *serverTracer) After(ctx context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	state, ok := ctx.Value(spanContextKey{}).(*callState)
	if !ok || state.owner != s {
		return nil
	}
	finish(state, resp.Payload, resp.Err, streamingMethods[callCtx.Method()])
	return nil
}

// finish accumulates payload and ends the span once the call reaches a
// terminal point (a non-streaming call is always terminal on its first
// response). Streamed content usually arrives via non-terminal
// TaskArtifactUpdateEvents before a final TaskStatusUpdateEvent that carries
// no content itself, so output must be the accumulated events, not just the
// last one.
func finish(state *callState, payload any, err error, streaming bool) {
	if err != nil {
		state.end(func() {
			state.span.RecordError(err)
			state.span.SetStatus(codes.Error, err.Error())
		})
		return
	}

	state.addEvent(payload)

	if streaming && !isTerminalEvent(payload) {
		return
	}

	state.end(func() {
		setOutput(state.span, state.method, state.snapshotEvents())
	})
}

// isTerminalEvent covers a2a-go v0.3.15's four Event/SendMessageResult
// implementers (Message, Task, TaskStatusUpdateEvent, TaskArtifactUpdateEvent).
// A new variant added upstream would fall into default (non-terminal) until
// this is updated - the context.AfterFunc fallback in startCall still ends
// the span once the call's context ends.
func isTerminalEvent(payload any) bool {
	switch v := payload.(type) {
	case *a2a.Message, *a2a.Task:
		return true
	case *a2a.TaskStatusUpdateEvent:
		return v.Final
	case *a2a.TaskArtifactUpdateEvent:
		return false
	default:
		return false
	}
}

func setMetadata(span trace.Span, method, role string) {
	_ = internal.SetJSONAttr(span, "braintrust.metadata", map[string]any{
		"provider": "a2a",
		"method":   method,
		"role":     role,
	})
}

func setInput(span trace.Span, payload any) {
	if payload == nil {
		return
	}
	_ = internal.SetJSONAttr(span, "braintrust.input_json", payload)
}

// setOutput writes a single event directly, or the full ordered list for a
// multi-event stream.
func setOutput(span trace.Span, method string, events []any) {
	if len(events) == 0 {
		return
	}
	output := any(events)
	if len(events) == 1 {
		output = events[0]
	}
	_ = internal.SetJSONAttr(span, "braintrust.output_json", output)

	if taskMethods[method] {
		_ = internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "task"})
	}
}
