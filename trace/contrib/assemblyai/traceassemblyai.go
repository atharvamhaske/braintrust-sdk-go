// Package assemblyai provides OpenTelemetry tracing for AssemblyAI's LLM
// Gateway (https://llm-gateway.assemblyai.com), an OpenAI-Chat-Completions-
// compatible endpoint that routes to Claude, GPT, Gemini, and other models.
//
// AssemblyAI doesn't publish a Go client for LLM Gateway (their prior
// standalone Go SDK, which wrapped the now-retired LeMUR API, was
// discontinued in April 2025). Since LLM Gateway's request/response shape is
// genuinely OpenAI-compatible, use the official openai-go client pointed at
// AssemblyAI's base URL, with this package's traced HTTP client and an
// auth-header override (LLM Gateway expects a bare API key with no "Bearer"
// prefix):
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
//	client := openai.NewClient(
//		option.WithBaseURL("https://llm-gateway.assemblyai.com/v1/"),
//		option.WithHeader("Authorization", apiKey), // overrides openai-go's default "Bearer " prefix
//		option.WithHTTPClient(traceassemblyai.Client()),
//	)
//
//	// All chat completions calls through this client are now traced.
//	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
//		Model:    "qwen3.5-4b-32k-fast",
//		Messages: []openai.ChatCompletionMessageParamUnion{...},
//	})
//
// Coverage notes:
//   - Only non-streaming chat completions (/v1/chat/completions) are
//     instrumented. LLM Gateway streaming support is unverified and out of
//     scope for now; a streaming request still produces a span, but its
//     response body isn't parsed (no output_json/metrics), since the SSE
//     framing hasn't been confirmed to match OpenAI's.
//   - No orchestrion auto-instrumentation ships for this package. Orchestrion
//     hooks a specific function call (e.g. openai.NewClient), but that
//     constructor is used identically for both real OpenAI and AssemblyAI's
//     gateway — there's no syntactic marker to distinguish them at compile
//     time, so auto-wrapping every openai.NewClient call would incorrectly
//     instrument real OpenAI traffic too. Use the manual client construction
//     shown above.
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
		return llmGatewayRouter(rt.cfg, path)
	}
	middleware := internal.Middleware(router, rt.cfg.logger) //nolint:bodyclose // false positive - returns middleware func, body closed by caller

	next := func(r *http.Request) (*http.Response, error) {
		return rt.base.RoundTrip(r)
	}

	return middleware(req, next)
}

// llmGatewayRouter maps LLM Gateway paths to their corresponding tracers.
// Returns nil for endpoints we don't instrument.
func llmGatewayRouter(cfg *config, path string) internal.MiddlewareTracer {
	if strings.HasSuffix(path, "/v1/chat/completions") {
		return newChatCompletionsTracer(cfg)
	}
	return nil
}
