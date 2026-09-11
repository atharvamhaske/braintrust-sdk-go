// Package tracea2a provides OpenTelemetry tracing for Agent2Agent (A2A)
// protocol clients and servers using the official Go SDK
// (github.com/a2aproject/a2a-go).
//
// Both a2aclient.Client and a2asrv.RequestHandler expose a transport-agnostic
// CallInterceptor extension point (used for every transport: gRPC, JSON-RPC,
// HTTP+JSON), so tracing hooks in above the wire format rather than wrapping
// an *http.Client like the HTTP-based provider integrations in this repo.
// The client and server run in different processes connected only by that
// wire, so a client and server span for the same call would land in two
// disconnected traces unless a trace context travels across it: this package
// injects a W3C traceparent into the client's CallMeta (which the SDK turns
// into real HTTP headers for JSON-RPC, or gRPC metadata for gRPC) and
// extracts it server-side from RequestMeta, so both sides land in one trace.
//
//	client, err := a2aclient.NewFromCard(ctx, card)
//	tracea2a.InstrumentClient(client)
//
//	requestHandler := a2asrv.NewHandler(executor, tracea2a.InstrumentServer())
//
// It is safe to combine Orchestrion auto-instrumentation with the manual
// calls above on the same client/handler: only the interceptor that first
// observes a given call creates its span, so a second (redundant) attached
// interceptor is a no-op for every call rather than producing a duplicate
// span - see the ownership check in Before/After below.
//
// Streaming calls (SendStreamingMessage/ResubscribeToTask on the client,
// OnSendMessageStream/OnResubscribeToTask on the server) only end their span
// once a terminal event is observed (see isTerminalEvent), or once the call's
// context is done (canceled, timed out, or the request otherwise ends),
// whichever comes first - see the context.AfterFunc registration in
// startCall. A caller that stops iterating a stream early without ever
// canceling or otherwise ending its context (a bare break with the context
// left running past the call) leaves the span open until that context is
// eventually done; if it never is, the span is never ended and is silently
// dropped by the exporter, since unfinished spans are never emitted.
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

// a2aPropagator carries the client span's context to the server across the
// A2A wire so the two sides land in one trace instead of two disconnected
// ones. It is an explicit instance rather than otel.GetTextMapPropagator(),
// which defaults to a no-op unless an application has separately configured
// one (typically for HTTP) - most consumers of this package won't have.
var a2aPropagator = propagation.TraceContext{}

// callMetaCarrier adapts a2aclient.CallMeta (the SDK's own transport-agnostic
// request metadata - real HTTP headers for JSON-RPC, gRPC metadata for gRPC,
// per a2aclient's own docs) to propagation.TextMapCarrier for injection.
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

// requestMetaCarrier adapts a2asrv.RequestMeta (built from the real inbound
// transport metadata - HTTP headers for JSON-RPC, per a2asrv's own JSON-RPC
// handler) to propagation.TextMapCarrier for extraction.
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

// streamingMethods are the client/server methods whose response is an
// iter.Seq2[a2a.Event, error] rather than a single result: the interceptor's
// After hook fires once per event instead of once per call.
var streamingMethods = map[string]bool{
	"SendStreamingMessage": true,
	"ResubscribeToTask":    true,
	"OnSendMessageStream":  true,
	"OnResubscribeToTask":  true,
}

// taskMethods are the methods that operate on an actual A2A task, as opposed
// to administrative/config calls (AgentCard resolution, push notification
// config). Only these get span_attributes.type = "task".
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

// callState accumulates every event payload seen across a (possibly
// streaming) call, so the final output reflects everything the call
// produced rather than just whichever event happened to arrive last.
//
// owner identifies the specific interceptor instance that created this
// state (via context.WithValue in Before). If more than one tracing
// interceptor ends up attached to the same client/handler - manual
// InstrumentClient/InstrumentServer combined with Orchestrion, or two
// manual calls - every attached interceptor's Before/After still runs for
// every call, but only the one matching owner does any work; the rest see
// state already present in the context and no-op. This makes double
// instrumentation harmless instead of producing duplicate spans.
//
// ended guards span.End() so the normal completion path (finish) and the
// context-done fallback (registered in startCall) can't both end - and
// double-report - the same span if they race.
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

// setStopWatch records the function that cancels the context.AfterFunc
// watch registered in startCall. Written and read under mu because
// context.AfterFunc calls its callback immediately, in its own goroutine, if
// ctx is already done by the time it's registered - so the callback (which
// calls end) can run concurrently with startCall's own goroutine still
// assigning the watch's stop function to this field.
func (s *callState) setStopWatch(stop func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopWatch = stop
}

// end ends the span exactly once, however it's triggered (normal terminal
// event, error, or the call's context ending before either of those
// happened). Safe to call concurrently with itself.
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

// InstrumentClient adds Braintrust tracing to an a2aclient.Client via its
// CallInterceptor extension point. Safe to call more than once, and safe to
// combine with Orchestrion auto-instrumentation on the same client - see the
// package doc.
func InstrumentClient(client *a2aclient.Client) {
	if client == nil {
		return
	}
	client.AddCallInterceptor(NewClientInterceptor())
}

// NewClientInterceptor returns a new a2aclient.CallInterceptor that adds
// Braintrust tracing. Exposed for a2aclient.WithInterceptors(...), used by
// this package's Orchestrion aspects to instrument client construction
// calls directly; most callers should use InstrumentClient instead.
func NewClientInterceptor() a2aclient.CallInterceptor {
	return &clientTracer{tr: tracer()}
}

// InstrumentServer returns a RequestHandlerOption that adds Braintrust
// tracing to an a2asrv.RequestHandler. Pass it to a2asrv.NewHandler alongside
// any other options.
func InstrumentServer() a2asrv.RequestHandlerOption {
	return a2asrv.WithCallInterceptor(&serverTracer{tr: tracer()})
}

// startCall starts a span for a call, stores its callState in ctx under
// owner, and arms a fallback that ends the span (marked as abandoned) if
// ctx is done before the call reaches a terminal point through the normal
// finish() path. It returns immediately if ctx already carries a callState
// - see the package doc on combining manual and Orchestrion instrumentation.
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

// finish records the response on state's span and ends it once the call
// reaches a terminal point. Non-streaming calls are always terminal on the
// first (and only) response. Streaming calls call this once per event -
// every event's payload is accumulated in state.events, and only a terminal
// event (a plain Message or Task result, or a TaskStatusUpdateEvent with
// Final=true), or an error, ends the span and writes the accumulated output.
// This matters because a real task-based stream typically sends its actual
// content via non-terminal TaskArtifactUpdateEvents, then a final
// TaskStatusUpdateEvent carrying only status/metadata - writing only the
// last event would silently drop everything the artifacts carried.
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

// isTerminalEvent reports whether payload is a terminal result for a
// streaming call. This is exhaustive over a2a-go v0.3.15's four concrete
// implementers of the a2a.Event/a2a.SendMessageResult interfaces (Message,
// Task, TaskStatusUpdateEvent, TaskArtifactUpdateEvent - see a2a/core.go's
// isEvent() implementations). If a2a-go (pre-1.0) adds a new Event variant,
// it falls into the default case and is treated as non-terminal, which for a
// stream that only ever emits the new type would leak an unended span until
// this switch is updated to recognize it - though the context.AfterFunc
// fallback in startCall still ends the span once the call's context ends.
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

// setOutput writes the accumulated event payloads for the call. A
// non-streaming call (or a streaming call with exactly one event) writes
// that single payload directly, keeping the same shape as a non-accumulated
// result; a genuine multi-event stream writes the full ordered list so
// nothing observed along the way is lost.
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
