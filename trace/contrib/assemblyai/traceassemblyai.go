// Package assemblyai provides OpenTelemetry tracing for AssemblyAI's LeMUR API.
//
// First, set up tracing with braintrust.New():
//
//	tp := trace.NewTracerProvider()
//	defer tp.Shutdown(context.Background())
//	otel.SetTracerProvider(tp)
//
//	bt, err := braintrust.New(tp,
//		braintrust.WithProject("my-project"),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//
// Then create your AssemblyAI client with tracing:
//
//	client := assemblyai.NewClientWithOptions(
//		assemblyai.WithAPIKey(apiKey),
//		assemblyai.WithHTTPClient(traceassemblyai.WrapClient(nil, traceassemblyai.WithTracerProvider(tp))),
//	)
//
//	// LeMUR calls (Task, Summarize, Question, ActionItems) are now traced.
//	resp, err := client.LeMUR.Task(ctx, assemblyai.LeMURTaskParams{
//		Prompt: assemblyai.String("Summarize this transcript."),
//		LeMURBaseParams: assemblyai.LeMURBaseParams{TranscriptIDs: []string{transcriptID}},
//	})
//
// Coverage notes:
//   - LeMUR's four generation endpoints (Task, Summarize, Question, ActionItems)
//     are instrumented: prompt/context/questions, model, and token usage.
//   - Transcript content (input_text / transcript_ids) is referenced by id/length
//     in metadata rather than copied into the span, since a single request can
//     carry up to 100 hours of transcript text.
//   - Transcription (Transcripts service), real-time streaming (RealTime
//     service), and LeMUR result management (GetResponseData,
//     PurgeRequestData) are not LLM-execution calls and are not instrumented.
package assemblyai

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/logger"
	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// config holds configuration for the HTTP client wrapper.
type config struct {
	tracerProvider trace.TracerProvider
	logger         logger.Logger
}

// Option configures the assemblyai HTTP client wrapper.
type Option func(*config)

// WithTracerProvider sets a custom TracerProvider for the HTTP client wrapper.
// If not provided, the global otel.GetTracerProvider() is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		c.tracerProvider = tp
	}
}

// WithLogger sets a custom logger for the HTTP client wrapper.
// If not provided, logging is disabled.
func WithLogger(log logger.Logger) Option {
	return func(c *config) {
		c.logger = log
	}
}

func (c *config) tracer() trace.Tracer {
	tp := c.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer("braintrust")
}

// Client returns a new http.Client configured with tracing middleware.
// This is equivalent to WrapClient(nil), which wraps the default HTTP transport.
//
// Example:
//
//	httpClient := assemblyai.Client()
func Client(opts ...Option) *http.Client {
	return WrapClient(nil, opts...)
}

// WrapClient wraps an existing http.Client with tracing middleware.
// If client is nil, a new client with the default transport is created.
//
// Example:
//
//	httpClient := assemblyai.WrapClient(nil)
//	client := assemblyai.NewClientWithOptions(assemblyai.WithAPIKey(key), assemblyai.WithHTTPClient(httpClient))
func WrapClient(client *http.Client, opts ...Option) *http.Client {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}

	if client == nil {
		client = &http.Client{}
	}

	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	client.Transport = newRoundTripper(transport, cfg)
	return client
}

// roundTripper wraps an http.RoundTripper with OpenTelemetry tracing.
type roundTripper struct {
	base http.RoundTripper
	cfg  *config
}

func newRoundTripper(base http.RoundTripper, cfg *config) http.RoundTripper {
	return &roundTripper{base: base, cfg: cfg}
}

// RoundTrip implements http.RoundTripper by intercepting requests and responses.
func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	router := func(path string) internal.MiddlewareTracer {
		return lemurRouter(rt.cfg, path)
	}
	middleware := internal.Middleware(router, rt.cfg.logger) //nolint:bodyclose // false positive - returns middleware func, body closed by caller

	next := func(r *http.Request) (*http.Response, error) {
		return rt.base.RoundTrip(r)
	}

	return middleware(req, next)
}

// lemurRouter maps LeMUR API paths to their corresponding tracers. Returns
// nil for endpoints we don't instrument (transcription, real-time, and LeMUR
// result management).
func lemurRouter(cfg *config, path string) internal.MiddlewareTracer {
	switch {
	case strings.HasSuffix(path, "/lemur/v3/generate/task"):
		return newLeMURTracer(cfg, "lemur_task", "task", lemurOutputResponse)
	case strings.HasSuffix(path, "/lemur/v3/generate/summary"):
		return newLeMURTracer(cfg, "lemur_summarize", "summary", lemurOutputResponse)
	case strings.HasSuffix(path, "/lemur/v3/generate/action-items"):
		return newLeMURTracer(cfg, "lemur_action_items", "action_items", lemurOutputResponse)
	case strings.HasSuffix(path, "/lemur/v3/generate/question-answer"):
		return newLeMURTracer(cfg, "lemur_question_answer", "question_answer", lemurOutputQuestionAnswer)
	}
	return nil
}
