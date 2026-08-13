package eval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An Eval carries its own dataset so it can run without a caller supplying one.
// The playground always passes RunOpts.Dataset, but `bt eval <dir>` and a
// direct Run() have nothing else to fall back on.
func TestMergeOpts_DatasetFallsBackToEvalDefinition(t *testing.T) {
	t.Parallel()

	definitionDataset := NewDataset([]Case[testInput, testOutput]{
		{Input: testInput{Value: "from-definition"}},
	})

	ev := &Eval[testInput, testOutput]{
		Name:    "my-eval",
		Dataset: definitionDataset,
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{})
	require.NoError(t, err)

	require.NotNil(t, opts.Dataset)
	first, err := opts.Dataset.Next()
	require.NoError(t, err)
	assert.Equal(t, "from-definition", first.Input.Value)
}

func TestMergeOpts_RunOptsDatasetWinsOverDefinition(t *testing.T) {
	t.Parallel()

	ev := &Eval[testInput, testOutput]{
		Name: "my-eval",
		Dataset: NewDataset([]Case[testInput, testOutput]{
			{Input: testInput{Value: "from-definition"}},
		}),
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{
		Dataset: NewDataset([]Case[testInput, testOutput]{
			{Input: testInput{Value: "from-run-opts"}},
		}),
	})
	require.NoError(t, err)

	first, err := opts.Dataset.Next()
	require.NoError(t, err)
	assert.Equal(t, "from-run-opts", first.Input.Value)
}

// The Braintrust playground sends the project the experiment must land in as
// an ID. Without it the runner falls back to creating a project by name, which
// fails outright for callers whose token has no project-create permission.
func TestMergeOpts_CarriesProjectID(t *testing.T) {
	t.Parallel()

	ev := &Eval[testInput, testOutput]{
		Name:        "my-eval",
		ProjectName: "declared-project",
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{ProjectID: "proj-123"})
	require.NoError(t, err)

	assert.Equal(t, "proj-123", opts.ProjectID)
	assert.Equal(t, "declared-project", opts.ProjectName, "the name is still carried for display")
}

func TestMergeOpts_CarriesOnExperimentStart(t *testing.T) {
	t.Parallel()

	ev := &Eval[testInput, testOutput]{
		Name: "my-eval",
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	called := false
	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{
		OnExperimentStart: func(ExperimentInfo) { called = true },
	})
	require.NoError(t, err)

	require.NotNil(t, opts.OnExperimentStart)
	opts.OnExperimentStart(ExperimentInfo{})
	assert.True(t, called)
}

// Parameters are declared on the eval, so a declared default reaches the task
// on every path -- including a plain local Run, which supplies no values. This
// was not possible when the schema lived on the remote-eval registration.
func TestMergeOpts_DeclaredDefaultsReachALocalRun(t *testing.T) {
	t.Parallel()

	ev := &Eval[testInput, testOutput]{
		Name: "my-eval",
		ParameterSchema: ParameterSchema{
			"model":     {Type: "model", Default: "gpt-4o"},
			"threshold": {Type: "number", Default: 0.5},
		},
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{})
	require.NoError(t, err)

	assert.Equal(t, "gpt-4o", opts.Parameters.String("model"))
	assert.InDelta(t, 0.5, opts.Parameters.Float64("threshold"), 0.0001)
}

// A value passed for the run still wins over the declared default.
func TestMergeOpts_RunValuesOverrideDeclaredDefaults(t *testing.T) {
	t.Parallel()

	ev := &Eval[testInput, testOutput]{
		Name:            "my-eval",
		ParameterSchema: ParameterSchema{"model": {Type: "model", Default: "gpt-4o"}},
		Task: T(func(_ context.Context, in testInput) (testOutput, error) {
			return testOutput{}, nil
		}),
	}

	opts, err := mergeOpts(ev, RunOpts[testInput, testOutput]{
		Parameters: Parameters{"model": "claude"},
	})
	require.NoError(t, err)

	assert.Equal(t, "claude", opts.Parameters.String("model"))
}
