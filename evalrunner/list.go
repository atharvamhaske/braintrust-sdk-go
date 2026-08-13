package evalrunner

import (
	"encoding/json"
	"fmt"
)

// manifest describes every registered eval: its scorers and the parameter
// controls the playground should render.
func (r *Runner) manifest() listResponse {
	resp := make(listResponse, len(r.evaluators))
	for name, e := range r.evaluators {
		if !matchesFilters(r.env.Filters, name) {
			continue
		}

		info := evalInfo{Scores: make([]scoreInfo, 0)}
		for _, sn := range e.scorerNames() {
			info.Scores = append(info.Scores, scoreInfo{Name: sn})
		}

		if schema := e.parameterSchema(); len(schema) > 0 {
			wireSchema := make(map[string]wireParameterDef, len(schema))
			for k, v := range schema {
				wireSchema[k] = toWireParameterDef(v)
			}
			info.Parameters = &parametersMeta{
				Type:   "braintrust.staticParameters",
				Schema: wireSchema,
				Source: nil,
			}
		}

		resp[name] = info
	}
	return resp
}

// runList prints the manifest as a single line of JSON on stdout.
//
// This is the one place stdout carries protocol data. bt scans the child's
// stdout lines in reverse for the last one that parses as JSON and returns it
// to the browser verbatim (bt/src/eval.rs:1573), so anything else printed
// afterwards would win. Nothing else in this package writes to stdout.
//
// No authentication happens here: a manifest needs no API access, so listing
// works without credentials.
func (r *Runner) runList() error {
	encoded, err := json.Marshal(r.manifest())
	if err != nil {
		return fmt.Errorf("failed to encode eval manifest: %w", err)
	}

	if _, err := fmt.Fprintf(r.stdout, "%s\n", encoded); err != nil {
		return fmt.Errorf("failed to write eval manifest: %w", err)
	}
	return nil
}
