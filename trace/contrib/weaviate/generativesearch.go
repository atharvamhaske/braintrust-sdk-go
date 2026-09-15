package weaviate

// this file parses the GraphQL Get query's generative search (RAG) results.
//
// Weaviate sends every GraphQL query, generative or not, through one
// endpoint: POST /v1/graphql, body {"query": "<GraphQL query text>"}. A
// generative search is a plain Get query with a generate(...) field nested
// under _additional; nothing in the request tells you the class name
// without parsing the query text, but the response always names it as a
// JSON key: data.Get.<ClassName>. So the class name is read from the
// response, not scraped out of the query string (see the bt-instru skill's
// "Provider-specific reality checks" guidance).
//
// Plain vector search and CRUD (WithNearVector/WithNearText without
// WithGenerativeSearch, Data().Creator(), etc.) are out of scope: they don't
// execute a generative-AI call, so no span is created for them. The router
// only knows a request is POST /v1/graphql; StartSpan is what actually
// decides whether this particular query is a generative search, by checking
// for the literal "generate(" field name GenerativeSearchBuilder always
// emits.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

type generativeSearchTracer struct {
	cfg      *config
	metadata map[string]any
	traced   bool
}

func newGenerativeSearchTracer(cfg *config) *generativeSearchTracer {
	return &generativeSearchTracer{
		cfg: cfg,
		metadata: map[string]any{
			"provider": "weaviate",
		},
	}
}

func (gt *generativeSearchTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	var raw struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, nil, err
	}

	// Not a generative search (plain vector search, schema query, CRUD via
	// GraphQL, etc.) - don't create a span for it. The shared middleware
	// unconditionally calls span.End() and friends on whatever StartSpan
	// returns, so this has to be a real no-op span, not nil: SpanFromContext
	// on a context with no span in it returns exactly that.
	if !strings.Contains(raw.Query, "generate(") {
		return ctx, trace.SpanFromContext(ctx), nil
	}
	gt.traced = true

	ctx, span := gt.cfg.tracer().Start(ctx, "weaviate.graphql.generate", trace.WithTimestamp(t))

	if err := internal.SetJSONAttr(span, "braintrust.input_json", map[string]any{"query": raw.Query}); err != nil {
		return ctx, span, err
	}
	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "llm"}); err != nil {
		return ctx, span, err
	}

	return ctx, span, nil
}

func (gt *generativeSearchTracer) TagSpan(span trace.Span, body io.Reader) error {
	if !gt.traced {
		// StartSpan decided this wasn't a generative search and returned a
		// nil span; the shared middleware still calls TagSpan on whatever it
		// got back, so there is nothing to tag.
		return nil
	}

	var raw struct {
		Data   map[string]json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return err
	}

	if len(raw.Errors) > 0 {
		span.SetStatus(codes.Error, raw.Errors[0].Message)
	}

	var getResult map[string]json.RawMessage
	if getRaw, ok := raw.Data["Get"]; ok {
		if err := json.Unmarshal(getRaw, &getResult); err != nil {
			return err
		}
	}

	var output any
	for className, objectsRaw := range getResult {
		gt.metadata["class_name"] = className
		var objects []map[string]any
		if err := json.Unmarshal(objectsRaw, &objects); err != nil {
			return err
		}
		output = objects
		if generateErr := firstGenerateError(objects); generateErr != "" {
			span.SetStatus(codes.Error, generateErr)
		}
		break // Get queries target exactly one class per request.
	}

	if err := internal.SetJSONAttr(span, "braintrust.metadata", gt.metadata); err != nil {
		return err
	}
	return internal.SetJSONAttr(span, "braintrust.output_json", output)
}

// firstGenerateError returns the first non-empty _additional.generate.error
// across the retrieved objects, if any. A GraphQL-level error (raw.Errors)
// means the query itself was malformed; this is different - the query
// succeeded but the generative provider call for one or more objects failed.
func firstGenerateError(objects []map[string]any) string {
	for _, obj := range objects {
		additional, ok := obj["_additional"].(map[string]any)
		if !ok {
			continue
		}
		generate, ok := additional["generate"].(map[string]any)
		if !ok {
			continue
		}
		if errMsg, ok := generate["error"].(string); ok && errMsg != "" {
			return errMsg
		}
	}
	return ""
}

var _ internal.MiddlewareTracer = &generativeSearchTracer{}
