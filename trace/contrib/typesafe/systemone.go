package typesafe

// this file parses the TypeSafe AI (Jev) /v1/systemone endpoint. Span shape
// (name "typesafe.systemOne", type "question", questions/answers as
// id-tagged lists) matches the official Python and JS SDKs' TypeSafe
// integration, not the generic OpenAI-style shape used elsewhere in this repo.

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

// systemOneTracer is a tracer for the TypeSafe AI POST /v1/systemone endpoint.
// See docs here: https://api.typesafe.ai
type systemOneTracer struct {
	cfg      *middlewareConfig
	metadata map[string]any
}

func newSystemOneTracer(cfg *middlewareConfig) *systemOneTracer {
	return &systemOneTracer{
		cfg: cfg,
		metadata: map[string]any{
			"provider": "typesafe",
		},
	}
}

func (st *systemOneTracer) StartSpan(ctx context.Context, t time.Time, request io.Reader) (context.Context, trace.Span, error) {
	ctx, span := st.cfg.tracer().Start(
		ctx,
		"typesafe.systemOne",
		trace.WithTimestamp(t),
	)

	if err := internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{"type": "question"}); err != nil {
		return ctx, span, err
	}

	var raw map[string]any
	if err := json.NewDecoder(request).Decode(&raw); err != nil {
		return ctx, span, err
	}

	if model, ok := raw["model"]; ok {
		st.metadata["model"] = model
	}

	questions, _ := raw["questions"].(map[string]any)
	input := map[string]any{
		"state":     raw["state"],
		"questions": itemsWithIDs(questions),
	}
	if err := internal.SetJSONAttr(span, "braintrust.input_json", input); err != nil {
		return ctx, span, err
	}

	if err := internal.SetJSONAttr(span, "braintrust.metadata", st.metadata); err != nil {
		return ctx, span, err
	}

	return ctx, span, nil
}

func (st *systemOneTracer) TagSpan(span trace.Span, body io.Reader) error {
	var raw map[string]any
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return err
	}

	if model, ok := raw["model"]; ok {
		st.metadata["model"] = model
	}
	if err := internal.SetJSONAttr(span, "braintrust.metadata", st.metadata); err != nil {
		return err
	}

	answers, _ := raw["answers"].(map[string]any)
	output := map[string]any{"answers": itemsWithIDs(answers)}
	if err := internal.SetJSONAttr(span, "braintrust.output_json", output); err != nil {
		return err
	}

	metrics := map[string]any{}
	if usage, ok := raw["usage"].(map[string]any); ok {
		var prompt, completion int64
		if ok, v := internal.ToInt64(usage["input_tokens"]); ok {
			metrics["prompt_tokens"] = v
			prompt = v
		}
		if ok, v := internal.ToInt64(usage["output_tokens"]); ok {
			metrics["completion_tokens"] = v
			completion = v
		}
		metrics["tokens"] = prompt + completion
	}
	if err := internal.SetJSONAttr(span, "braintrust.metrics", metrics); err != nil {
		return err
	}

	return nil
}

// itemsWithIDs turns a {name: {...fields}} map into a [{...fields, id: name}, ...]
// list, matching how the Python and JS SDKs shape questions/answers.
func itemsWithIDs(values map[string]any) []map[string]any {
	if values == nil {
		return nil
	}
	items := make([]map[string]any, 0, len(values))
	for id, value := range values {
		fields, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item := make(map[string]any, len(fields)+1)
		maps.Copy(item, fields)
		item["id"] = id
		items = append(items, item)
	}
	return items
}
