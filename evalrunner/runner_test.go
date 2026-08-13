package evalrunner

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/braintrustdata/braintrust-sdk-go/eval"
	"github.com/braintrustdata/braintrust-sdk-go/logger"
)

// newTestRunner builds a runner with a fabricated environment and a buffer
// standing in for stdout, so tests never mutate the process environment and
// never write to the real stdout.
func newTestRunner(t *testing.T, vars map[string]string) (*Runner, *bytes.Buffer) {
	t.Helper()

	stdout := &bytes.Buffer{}
	r := New(WithLogger(logger.Discard()))
	r.env = readEnv(mapLookup(vars))
	r.env.Filters = parseFilters(vars["BT_EVAL_FILTER_PARSED"], nil)
	r.stdout = stdout

	return r, stdout
}

func TestRunner_ModeReflectsEnvironment(t *testing.T) {
	r, _ := newTestRunner(t, map[string]string{"BT_EVAL_DEV_MODE": "list"})
	assert.Equal(t, ModeList, r.Mode())

	r, _ = newTestRunner(t, map[string]string{})
	assert.Equal(t, ModeInspect, r.Mode())
}

func TestRun_RejectsUnknownDevMode(t *testing.T) {
	r, _ := newTestRunner(t, map[string]string{"BT_EVAL_DEV_MODE": "banana"})

	err := Run(context.Background(), r)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "banana")
}

func TestInspectMode_ListsRegisteredEvals(t *testing.T) {
	r, stdout := newTestRunner(t, map[string]string{})
	registerTestEval(r, "food-classifier", eval.ParameterSchema{"model": {Type: "model"}})
	registerTestEval(r, "tone-checker", nil)

	require.NoError(t, Run(context.Background(), r))

	out := stdout.String()
	assert.Contains(t, out, "food-classifier")
	assert.Contains(t, out, "scorers: exact_match")
	assert.Contains(t, out, "parameters: model")
	assert.Contains(t, out, "tone-checker")
	assert.Contains(t, out, "bt eval")
}

// Registration order drives the listing so repeated runs read the same way;
// Go's map iteration order would otherwise shuffle it.
func TestInspectMode_PreservesRegistrationOrder(t *testing.T) {
	r, stdout := newTestRunner(t, map[string]string{})
	registerTestEval(r, "zebra", nil)
	registerTestEval(r, "aardvark", nil)

	require.NoError(t, Run(context.Background(), r))

	assert.Less(t,
		bytes.Index(stdout.Bytes(), []byte("zebra")),
		bytes.Index(stdout.Bytes(), []byte("aardvark")),
	)
}

func TestInspectMode_EmptyRunnerExplainsItself(t *testing.T) {
	r, stdout := newTestRunner(t, map[string]string{})

	require.NoError(t, Run(context.Background(), r))

	assert.Contains(t, stdout.String(), "No evals registered")
	assert.Contains(t, stdout.String(), "RegisterEval")
}

// A bare `go run ./cmd/evals` must not reach Braintrust: someone checking that
// their program compiles should not create an experiment as a side effect.
func TestInspectMode_MakesNoAPICalls(t *testing.T) {
	t.Setenv("BRAINTRUST_API_KEY", "")
	t.Setenv("BRAINTRUST_APP_URL", "http://127.0.0.1:1") // would fail loudly if dialled

	r, _ := newTestRunner(t, map[string]string{})
	registerTestEval(r, "food-classifier", nil)

	assert.NoError(t, Run(context.Background(), r))
}

func TestRegisterEval_DuplicateNameReplacesWithoutDuplicatingOrder(t *testing.T) {
	r, _ := newTestRunner(t, map[string]string{})
	registerTestEval(r, "same-name", nil)
	registerTestEval(r, "same-name", eval.ParameterSchema{"m": {Type: "model"}})

	require.Len(t, r.order, 1)
	require.Len(t, r.evaluators, 1)
	assert.NotNil(t, r.evaluators["same-name"].parameterSchema(), "the later registration wins")
}

func TestExitCode_StreamingFailuresDoNotExitNonZero(t *testing.T) {
	// bt appends its own error frame when a child exits non-zero without having
	// sent one, which the playground shows as a failed run even though the
	// summary arrived. So eval mode always exits 0 once it has reported.
	r, _ := newTestRunner(t, map[string]string{
		"BT_EVAL_DEV_MODE": "eval",
		"BT_EVAL_SSE_SOCK": "/nonexistent.sock",
	})
	r.allPassed = false

	assert.Equal(t, 0, r.exitCode())
}

// A batch run has no playground watching, so a non-zero exit is the useful
// signal for CI.
func TestExitCode_BatchFailuresExitNonZero(t *testing.T) {
	r, _ := newTestRunner(t, map[string]string{"BT_EVAL_SSE_SOCK": "/nonexistent.sock"})
	require.Equal(t, ModeBatch, r.Mode())

	assert.Equal(t, 0, r.exitCode())
	r.allPassed = false
	assert.Equal(t, 1, r.exitCode())
}
