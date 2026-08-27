// Package evalhooks carries the callbacks the eval runner needs to observe a
// run while it is still in flight.
//
// These live under internal/ deliberately. The remote eval runner needs to know
// which experiment a run is writing to, and needs each case's result as it
// lands, but the shape of eval callbacks is not settled across the Braintrust
// SDKs yet. Keeping the types here means [github.com/braintrustdata/braintrust-sdk-go/eval]
// can expose a hooks field that only this module can populate, so nothing is
// promised to callers before the design is agreed.
package evalhooks

// Hooks are the callbacks fired during an eval run. A nil *Hooks is valid and
// fires nothing, so callers never need nil checks.
type Hooks struct {
	// OnCaseComplete is called after each case completes (task + scorers).
	// It is called from worker goroutines and must be safe for concurrent use.
	OnCaseComplete func(CaseProgress)

	// OnExperimentStart is called once, after the experiment has been
	// registered and before any case runs. It is the only way to learn the
	// experiment's identity while the run is still going: the Result is not
	// available until the run finishes.
	OnExperimentStart func(ExperimentInfo)
}

// CaseComplete fires OnCaseComplete if it is set.
func (h *Hooks) CaseComplete(cp CaseProgress) {
	if h == nil || h.OnCaseComplete == nil {
		return
	}
	h.OnCaseComplete(cp)
}

// ExperimentStart fires OnExperimentStart if it is set.
func (h *Hooks) ExperimentStart(info ExperimentInfo) {
	if h == nil || h.OnExperimentStart == nil {
		return
	}
	h.OnExperimentStart(info)
}

// CaseProgress contains the result of a single completed evaluation case.
type CaseProgress struct {
	Output any
	Scores map[string]float64
	Error  error

	// ID is the eval span ID, used to correlate SSE progress events with OTLP
	// span data.
	ID string

	// Origin contains dataset provenance when the case came from a dataset.
	Origin map[string]any
}

// ExperimentInfo identifies the experiment a run is writing to.
type ExperimentInfo struct {
	ExperimentID   string
	ExperimentName string
	ProjectID      string
	ProjectName    string

	// ExperimentURL links to the experiment in the Braintrust UI. Empty when
	// the org name or experiment ID could not be determined.
	ExperimentURL string
}
