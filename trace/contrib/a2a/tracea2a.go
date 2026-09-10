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
package tracea2a

import (
	"context"

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

type spanContextKey struct{}

func tracer() trace.Tracer {
	return otel.GetTracerProvider().Tracer("braintrust")
}

// InstrumentClient adds Braintrust tracing to an a2aclient.Client via its
// CallInterceptor extension point.
func InstrumentClient(client *a2aclient.Client) {
	if client == nil {
		return
	}
	client.AddCallInterceptor(&clientTracer{tr: tracer()})
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
	return context.WithValue(ctx, spanContextKey{}, span), nil
}

func (c *clientTracer) After(ctx context.Context, resp *a2aclient.Response) error {
	span, ok := ctx.Value(spanContextKey{}).(trace.Span)
	if !ok {
		return nil
	}
	finish(span, resp.Payload, resp.Err, streamingMethods[resp.Method])
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
	return context.WithValue(ctx, spanContextKey{}, span), nil
}

func (s *serverTracer) After(ctx context.Context, callCtx *a2asrv.CallContext, resp *a2asrv.Response) error {
	span, ok := ctx.Value(spanContextKey{}).(trace.Span)
	if !ok {
		return nil
	}
	finish(span, resp.Payload, resp.Err, streamingMethods[callCtx.Method()])
	return nil
}

// finish records the response on span and ends it once the call reaches a
// terminal point. Non-streaming calls are always terminal on the first (and
// only) response. Streaming calls (SendStreamingMessage/ResubscribeToTask on
// the client, OnSendMessageStream/OnResubscribeToTask on the server) call
// this once per event - only a terminal event (a plain Message or Task
// result, or a TaskStatusUpdateEvent with Final=true), or an error, ends the
// span, so its duration covers the whole stream rather than just the first
// chunk. TaskArtifactUpdateEvent and non-final TaskStatusUpdateEvent are
// always incremental.
func finish(span trace.Span, payload any, err error, streaming bool) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.End()
		return
	}

	setOutput(span, payload)

	if streaming && !isTerminalEvent(payload) {
		return
	}
	span.End()
}

func isTerminalEvent(payload any) bool {
	switch v := payload.(type) {
	case *a2a.Message, *a2a.Task:
		return true
	case *a2a.TaskStatusUpdateEvent:
		return v.Final
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

func setOutput(span trace.Span, payload any) {
	if payload == nil {
		return
	}
	_ = internal.SetJSONAttr(span, "braintrust.output_json", payload)
	_ = internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "task"})
}
