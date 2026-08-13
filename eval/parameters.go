package eval

import "encoding/json"

// ParameterSchema declares the parameters an eval accepts, keyed by name.
//
// It is the *declaration*; [Parameters] is the *resolved values* for one run.
// Keeping the two names distinct matters, because they are easy to confuse: an
// eval declares a ParameterSchema once, and every run of it produces its own
// Parameters.
//
// Declaring a schema makes the parameters configurable from the Braintrust
// playground, where each entry becomes a control:
//
//	ParameterSchema: eval.ParameterSchema{
//	    "model":       {Type: "model", Default: "gpt-4o", Description: "Model to use"},
//	    "temperature": {Type: "number", Default: 0.0},
//	},
type ParameterSchema map[string]ParameterDef

// ParameterDef declares a single parameter.
type ParameterDef struct {
	// Type selects the control the Braintrust playground renders.
	//
	// "model" renders a model picker whose value arrives as a string. Any other
	// value describes scalar data and is passed through as a JSON Schema type --
	// "string", "number", "integer", "boolean" -- rendered as a plain input.
	Type string

	// Default is used when a run supplies no value for this parameter. It is
	// also what a local run sees, since nothing overrides it there.
	Default any

	// Description is shown alongside the control in the playground.
	Description string
}

// Parameter types with a dedicated control in the Braintrust playground. Any
// other value describes scalar data and is passed through as a JSON Schema
// type -- "string", "number", "integer", "boolean".
const (
	// ParameterTypeModel renders a model picker. Its value reaches the task as
	// a string.
	ParameterTypeModel = "model"

	// ParameterTypePrompt renders a prompt picker.
	ParameterTypePrompt = "prompt"
)

// Resolve merges the supplied values over the schema's declared defaults,
// producing the [Parameters] a run surfaces to the task.
//
// Supplied values win. Names absent from the schema pass through, matching the
// other SDKs -- the playground may send a parameter the code has not declared,
// and dropping it silently would be worse than surfacing it. Returns nil when
// there is nothing to surface, so a task can distinguish "no parameters" from
// "an empty set".
//
// It returns an error so that a parameter type needing conversion or
// validation can reject a bad value here, before a run starts, rather than
// failing partway through a task.
//
// Resolve is idempotent: resolving an already-resolved set returns it
// unchanged. Callers rely on that, because a run may resolve once up front to
// report a bad value and again when the run is assembled.
func (s ParameterSchema) Resolve(values map[string]any) (Parameters, error) {
	if len(s) == 0 && len(values) == 0 {
		return nil, nil
	}

	resolved := make(Parameters, len(s)+len(values))
	for name, def := range s {
		if def.Default != nil {
			resolved[name] = def.Default
		}
	}
	for name, value := range values {
		resolved[name] = value
	}

	if len(resolved) == 0 {
		return nil, nil
	}
	return resolved, nil
}

// Parameters holds the resolved parameter values for a single eval run.
//
// When an eval is driven from the Braintrust playground via the remote eval
// server, the controls a user configures (a model picker, a numeric threshold,
// and so on) arrive as a flat map of values. The runner merges them over the
// defaults declared in [ParameterSchema] and hands the result to the task on
// [TaskHooks.Parameters]. For a local run, whatever is passed in
// [RunOpts.Parameters] is what the task sees.
//
// It is a map[string]any, so you can range over it directly, but the typed
// accessors below are the idiomatic way to read a value: they hide the fact
// that JSON numbers decode as float64 and never panic on a type mismatch
// (returning the zero value instead).
//
//	model := hooks.Parameters.String("model")
//	max   := hooks.Parameters.Int("max_length")
type Parameters map[string]any

// Get returns the raw value for name and whether it was present.
func (p Parameters) Get(name string) (any, bool) {
	v, ok := p[name]
	return v, ok
}

// Has reports whether a value for name is present.
func (p Parameters) Has(name string) bool {
	_, ok := p[name]
	return ok
}

// String returns the value for name as a string, or "" if it is absent or not
// a string.
func (p Parameters) String(name string) string {
	if s, ok := p[name].(string); ok {
		return s
	}
	return ""
}

// Int returns the value for name as an int, or 0 if it is absent or not a
// number. JSON numbers decode as float64, so those are converted here; int and
// json.Number are also accepted.
func (p Parameters) Int(name string) int {
	switch v := p[name].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
		if f, err := v.Float64(); err == nil {
			return int(f)
		}
	}
	return 0
}

// Float64 returns the value for name as a float64, or 0 if it is absent or not a
// number.
func (p Parameters) Float64(name string) float64 {
	switch v := p[name].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}

// Bool returns the value for name as a bool, or false if it is absent or not a
// bool.
func (p Parameters) Bool(name string) bool {
	if b, ok := p[name].(bool); ok {
		return b
	}
	return false
}
