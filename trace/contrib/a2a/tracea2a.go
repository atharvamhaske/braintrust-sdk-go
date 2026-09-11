// Package tracea2a provides OpenTelemetry tracing for Agent2Agent (A2A)
// protocol clients and servers using the official Go SDK
// (github.com/a2aproject/a2a-go).
//
// Both a2aclient.Client and a2asrv.RequestHandler expose a transport-agnostic
// CallInterceptor extension point (used for every transport: gRPC, JSON-RPC,
// HTTP+JSON), so tracing hooks in above the wire format rather than wrapping
// an *http.Client like the HTTP-based provider integrations in this repo.
//
//	client, err := a2aclient.NewFromCard(ctx, card)
//	tracea2a.InstrumentClient(client)
//
//	requestHandler := a2asrv.NewHandler(executor, tracea2a.InstrumentServer())
//
// Use either Orchestrion auto-instrumentation or the manual calls above for a
// given client/handler, not both: a2aclient.Client.AddCallInterceptor keeps
// no record of previously attached interceptors, so combining Orchestrion's
// injected instrumentation with a manual InstrumentClient call on the same
// *already-Orchestrion-instrumented* client attaches a second interceptor
// and produces duplicate spans. InstrumentClient itself guards against being
// called twice on the same client, but it cannot see interceptors attached
// through a2aclient.WithInterceptors at construction time (which is how
// Orchestrion instruments client construction calls).
//
// Streaming calls (SendStreamingMessage/ResubscribeToTask on the client,
// OnSendMessageStream/OnResubscribeToTask on the server) only end their span
// once a terminal event is observed (see isTerminalEvent). If a caller stops
// iterating a stream before a terminal event arrives - an early break,
// context cancellation, or client-side error - the span is never ended and
// is silently dropped by the exporter, since unfinished spans are never
// emitted. This is a limitation of tracing per-event interceptors rather
// than a single request/response pair.
package tracea2a

import (
	"context"
	"sync"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

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
type callState struct {
	span   trace.Span
	method string
	events []any
}

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("braintrust")
}

// instrumentedClients guards against attaching more than one tracing
// interceptor to the same *a2aclient.Client via repeated InstrumentClient
// calls. It cannot see interceptors attached via a2aclient.WithInterceptors
// (how Orchestrion instruments client construction) - see the package doc.
var instrumentedClients sync.Map // *a2aclient.Client -> struct{}

// InstrumentClient adds Braintrust tracing to an a2aclient.Client via its
// CallInterceptor extension point. Safe to call more than once on the same
// client: only the first call attaches an interceptor.
func InstrumentClient(client *a2aclient.Client) {
	if client == nil {
		return
	}
	if _, alreadyInstrumented := instrumentedClients.LoadOrStore(client, struct{}{}); alreadyInstrumented {
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

// clientTracer implements a2aclient.CallInterceptor.
type clientTracer struct {
	a2aclient.PassthroughInterceptor
	tr trace.Tracer
}

func (c *clientTracer) Before(ctx context.Context, req *a2aclient.Request) (context.Context, error) {
	ctx, span := c.tr.Start(ctx, "a2a."+req.Method, trace.WithSpanKind(trace.SpanKindClient))
	setMetadata(span, req.Method, "client")
	setInput(span, req.Payload)
	state := &callState{span: span, method: req.Method}
	return context.WithValue(ctx, spanContextKey{}, state), nil
}

func (c *clientTracer) After(ctx context.Context, resp *a2aclient.Response) error {
	state, ok := ctx.Value(spanContextKey{}).(*callState)
	if !ok {
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
	ctx, span := s.tr.Start(ctx, "a2a."+callCtx.Method(), trace.WithSpanKind(trace.SpanKindServer))
	setMetadata(span, callCtx.Method(), "server")
	setInput(span, req.Payload)
	state := &callState{span: span, method: callCtx.Method()}
	return context.WithValue(ctx, spanContextKey{}, state), nil
}

func (s *serverTracer) After(ctx context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	state, ok := ctx.Value(spanContextKey{}).(*callState)
	if !ok {
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
		state.span.RecordError(err)
		state.span.SetStatus(codes.Error, err.Error())
		state.span.End()
		return
	}

	if payload != nil {
		state.events = append(state.events, payload)
	}

	if streaming && !isTerminalEvent(payload) {
		return
	}

	setOutput(state.span, state.method, state.events)
	state.span.End()
}

// isTerminalEvent reports whether payload is a terminal result for a
// streaming call. This is exhaustive over a2a-go v0.3.15's four concrete
// implementers of the a2a.Event/a2a.SendMessageResult interfaces (Message,
// Task, TaskStatusUpdateEvent, TaskArtifactUpdateEvent - see a2a/core.go's
// isEvent() implementations). If a2a-go (pre-1.0) adds a new Event variant,
// it falls into the default case and is treated as non-terminal, which for a
// stream that only ever emits the new type would leak an unended span until
// this switch is updated to recognize it.
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
