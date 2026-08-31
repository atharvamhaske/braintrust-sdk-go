package evalrunner

import (
	"encoding/json"

	"github.com/braintrustdata/braintrust-sdk-go/eval"
)

// EvalRequest is the request body for POST /eval.
type EvalRequest struct {
	// Name is the registered evaluator name (required).
	Name string `json:"name"`

	// Data specifies the evaluation dataset (required).
	Data EvalData `json:"data"`

	// ExperimentName overrides the experiment name (optional).
	ExperimentName string `json:"experiment_name,omitempty"`

	// ProjectID overrides the project ID (optional).
	ProjectID string `json:"project_id,omitempty"`

	// Parameters holds the resolved parameter values selected in the playground
	// (a flat name->value map). Merged over declared defaults and surfaced to the
	// task via eval.TaskHooks.Parameters.
	Parameters map[string]any `json:"parameters,omitempty"`

	// Parent specifies the parent span for tracing (optional).
	Parent *ParentInfo `json:"parent,omitempty"`
}

// EvalData specifies where evaluation data comes from.
//
// The playground sends a dataset_id, but bt resolves that into
// {project_id, dataset_name} before spawning us and discards every other key,
// so ProjectID+DatasetName is the shape that actually arrives in practice.
// See bt/src/eval.rs:1256 (resolve_dataset_ref_for_eval_request).
type EvalData struct {
	// Data is an inline array of test cases.
	Data json.RawMessage `json:"data,omitempty"`

	// DatasetID loads a dataset by ID.
	DatasetID string `json:"dataset_id,omitempty"`

	// DatasetName loads a dataset by name, scoped by ProjectID or ProjectName.
	DatasetName string `json:"dataset_name,omitempty"`

	// ProjectID scopes DatasetName lookups. This is what bt sends.
	ProjectID string `json:"project_id,omitempty"`

	// ProjectName scopes DatasetName lookups (optional).
	ProjectName string `json:"project_name,omitempty"`
}

// ParentInfo specifies parent span context for tracing.
type ParentInfo struct {
	ObjectType      string          `json:"object_type,omitempty"`
	ObjectID        string          `json:"object_id,omitempty"`
	PropagatedEvent json.RawMessage `json:"propagated_event,omitempty"`
}

// listResponse is the response for GET/POST /list.
type listResponse map[string]evalInfo

// evalInfo describes a registered evaluator in the list response.
type evalInfo struct {
	Scores     []scoreInfo     `json:"scores"`
	Parameters *parametersMeta `json:"parameters,omitempty"`
}

// scoreInfo describes a scorer in the list response.
type scoreInfo struct {
	Name string `json:"name"`
}

// parametersMeta wraps parameters with the protocol-required metadata.
type parametersMeta struct {
	Type   string                      `json:"type"`
	Schema map[string]wireParameterDef `json:"schema"`
	Source *string                     `json:"source"`
}

// wireParameterDef is the wire format for a parameter in the dev server protocol.
// Scalar ("data") parameters are wrapped with type "data" and a nested schema
// object; "model" (and "prompt") parameters use their own top-level type and omit
// the nested schema.
type wireParameterDef struct {
	Type        string       `json:"type"`
	Schema      *schemaField `json:"schema,omitempty"`
	Default     any          `json:"default,omitempty"`
	Description string       `json:"description,omitempty"`
}

// schemaField is the inner schema for a wire parameter definition.
type schemaField struct {
	Type string `json:"type"`
}

// toWireParameterDef converts a user-declared [eval.ParameterDef] into its
// dev-server wire form. "model" and "prompt" parameters map to their own
// top-level type with no nested schema; everything else is a scalar "data"
// parameter carrying a nested JSON-schema-ish {type} object.
func toWireParameterDef(def eval.ParameterDef) wireParameterDef {
	switch def.Type {
	case eval.ParameterTypeModel, eval.ParameterTypePrompt:
		return wireParameterDef{
			Type:        def.Type,
			Default:     def.Default,
			Description: def.Description,
		}
	default:
		return wireParameterDef{
			Type:        "data",
			Schema:      &schemaField{Type: def.Type},
			Default:     def.Default,
			Description: def.Description,
		}
	}
}

// progressEvent streams one case's output back to the playground while the run
// is in flight. Scores do not travel here -- they reach Braintrust as spans.
//
// Every field except origin is required by bt, and Data must be a string
// carrying JSON-encoded JSON. A frame that does not fit is dropped silently.
// See bt/src/eval.rs:2820 (SseProgressEventData).
type progressEvent struct {
	ID         string         `json:"id"`
	ObjectType string         `json:"object_type"`
	Name       string         `json:"name"`
	Format     string         `json:"format"`
	OutputType string         `json:"output_type"`
	Event      string         `json:"event"`
	Data       string         `json:"data"`
	Origin     map[string]any `json:"origin,omitempty"`
}

// startEvent announces the experiment a run is writing to, so the playground
// can link to it before the run finishes.
//
// bt's ExperimentStart accepts snake_case aliases as well, but we emit
// camelCase to match summaryEvent, which does not.
// See bt/src/eval.rs:2747.
type startEvent struct {
	ProjectName    string `json:"projectName,omitempty"`
	ExperimentName string `json:"experimentName,omitempty"`
	ProjectID      string `json:"projectId,omitempty"`
	ExperimentID   string `json:"experimentId,omitempty"`
	ExperimentURL  string `json:"experimentUrl,omitempty"`
}

// summaryEvent is the aggregate emitted once a run completes.
//
// The JSON names here are load-bearing. bt deserializes this into a strict
// camelCase struct with NO snake_case aliases, and if it does not fit, bt
// discards the event without logging anything at all -- the run then reports
// "Eval runner did not return a summary" with no clue why. projectName,
// experimentName and scores have no serde default and are therefore required.
// See bt/src/eval.rs:2763 (ExperimentSummary) and :2916 (the silent drop).
type summaryEvent struct {
	ProjectName    string                  `json:"projectName"`
	ExperimentName string                  `json:"experimentName"`
	ProjectID      string                  `json:"projectId,omitempty"`
	ExperimentID   string                  `json:"experimentId,omitempty"`
	ProjectURL     string                  `json:"projectUrl,omitempty"`
	ExperimentURL  string                  `json:"experimentUrl,omitempty"`
	Scores         map[string]scoreSummary `json:"scores"`
}

// scoreSummary is one aggregated score. bt requires name and score; the
// comparison counts default to zero. These names are literal -- bt applies no
// case conversion to this struct. See bt/src/eval.rs:2790.
type scoreSummary struct {
	Name         string   `json:"name"`
	Score        float64  `json:"score"`
	Diff         *float64 `json:"diff,omitempty"`
	Improvements int      `json:"improvements"`
	Regressions  int      `json:"regressions"`
}

// newSummaryEvent builds the summary frame from averaged scores. Scores is
// always non-nil: bt's field is a required map, so a JSON null would take the
// whole event down with it.
func newSummaryEvent(averages map[string]float64, result experimentRef) summaryEvent {
	scores := make(map[string]scoreSummary, len(averages))
	for name, score := range averages {
		scores[name] = scoreSummary{Name: name, Score: score}
	}

	summary := summaryEvent{Scores: scores}
	if result != nil {
		summary.ProjectName = result.ProjectName()
		summary.ExperimentName = result.Name()
		summary.ProjectID = result.ProjectID()
		summary.ExperimentID = result.ID()
		if permalink, err := result.Permalink(); err == nil {
			summary.ExperimentURL = permalink
		}
	}
	return summary
}

// experimentRef is the slice of *eval.Result the summary needs, named as an
// interface so tests can build a summary without running an eval.
type experimentRef interface {
	ID() string
	Name() string
	ProjectID() string
	ProjectName() string
	Permalink() (string, error)
}

// errorEvent reports a failure to the playground. bt requires message; status
// lets the UI distinguish a missing evaluator (404) or a bad request (400) from
// an internal failure. See bt/src/eval.rs:2799 (EvalErrorPayload).
type errorEvent struct {
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
	Status  int    `json:"status,omitempty"`
}
