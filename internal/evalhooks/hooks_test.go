package evalhooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A nil *Hooks is the common case -- most runs set no callbacks at all -- so it
// has to be safe to fire without the caller guarding every call site.
func TestNilHooksFireNothing(t *testing.T) {
	var h *Hooks

	assert.NotPanics(t, func() {
		h.CaseComplete(CaseProgress{})
		h.ExperimentStart(ExperimentInfo{})
	})
}

func TestEmptyHooksFireNothing(t *testing.T) {
	h := &Hooks{}

	assert.NotPanics(t, func() {
		h.CaseComplete(CaseProgress{})
		h.ExperimentStart(ExperimentInfo{})
	})
}

func TestHooksFireWhenSet(t *testing.T) {
	var gotCase CaseProgress
	var gotExperiment ExperimentInfo

	h := &Hooks{
		OnCaseComplete:    func(cp CaseProgress) { gotCase = cp },
		OnExperimentStart: func(i ExperimentInfo) { gotExperiment = i },
	}

	h.CaseComplete(CaseProgress{ID: "span-1"})
	h.ExperimentStart(ExperimentInfo{ExperimentID: "exp-1"})

	assert.Equal(t, "span-1", gotCase.ID)
	assert.Equal(t, "exp-1", gotExperiment.ExperimentID)
}
