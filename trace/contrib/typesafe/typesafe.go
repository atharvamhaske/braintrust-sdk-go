// Package typesafe provides OpenTelemetry tracing for the unofficial TypeSafe AI
// (Jev) Go client at github.com/atharvamhaske/typesafe-sdk-go.
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
// Then create your TypeSafe client with a traced HTTP client:
//
//	client, err := ts.NewClient(ts.WithHTTPClient(typesafe.Client()))
//
// For tests or custom configurations, you can provide a TracerProvider:
//
//	httpClient := typesafe.Client(typesafe.WithTracerProvider(tp))
//	client, err := ts.NewClient(ts.WithHTTPClient(httpClient))
//
//	// Your SystemOne calls will now be automatically traced
//	resp, err := client.SystemOne(ctx, state, questions)
package typesafe

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/logger"
	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// middlewareConfig holds configuration for the HTTP client wrapper.
type middlewareConfig struct {
	tracerProvider trace.TracerProvider
	logger         logger.Logger
}

// Option configures the HTTP client wrapper.
type Option func(*middlewareConfig)

// WithTracerProvider sets a custom TracerProvider for the HTTP client wrapper.
// If not provided, the global otel.GetTracerProvider() is used.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *middlewareConfig) {
		c.tracerProvider = tp
	}
}

// WithLogger sets a custom logger for the HTTP client wrapper.
// If not provided, logging is disabled.
func WithLogger(log logger.Logger) Option {
	return func(c *middlewareConfig) {
		c.logger = log
	}
}

func (c *middlewareConfig) tracer() trace.Tracer {
	tp := c.tracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer("braintrust")
}

// Client returns a new http.Client configured with tracing middleware.
// This is equivalent to WrapClient(nil), which wraps the default HTTP transport.
func Client(opts ...Option) *http.Client {
	return WrapClient(nil, opts...)
}

// WrapClient wraps an existing http.Client with tracing middleware.
// If client is nil, a new client with the default transport is created.
func WrapClient(client *http.Client, opts ...Option) *http.Client {
	cfg := &middlewareConfig{}
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
	client.Transport = &roundTripper{base: transport, cfg: cfg}
	return client
}

// roundTripper wraps an http.RoundTripper with OpenTelemetry tracing.
type roundTripper struct {
	base http.RoundTripper
	cfg  *middlewareConfig
}

func (rt *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	log := rt.cfg.logger
	if log == nil {
		log = logger.Discard()
	}

	router := func(path string) internal.MiddlewareTracer {
		return typesafeRouter(rt.cfg, path)
	}

	middleware := internal.Middleware(router, log)
	return middleware(req, func(r *http.Request) (*http.Response, error) { //nolint:bodyclose // false positive - body closed by caller
		return rt.base.RoundTrip(r)
	})
}

func typesafeRouter(cfg *middlewareConfig, path string) internal.MiddlewareTracer {
	if strings.HasSuffix(path, "/v1/systemone") {
		return newSystemOneTracer(cfg)
	}
	return nil
}

// Ensure our tracer implements the shared interface.
var _ internal.MiddlewareTracer = &systemOneTracer{}
