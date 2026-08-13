package evalrunner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run a real compiled runner as a child process. They are the only
// place that can observe what bt actually observes: the process's argv
// handling, its stdout, and its exit code. Everything else is verifiable
// in-process, so keep this file small -- each test pays a Go build.

func buildRunnerFixture(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping subprocess test in short mode")
	}

	binary := filepath.Join(t.TempDir(), "btrunner")
	cmd := exec.Command("go", "build", "-o", binary, "./testdata/btrunner")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build runner fixture: %v\n%s", err, out)
	}
	return binary
}

// runFixture executes the built runner with the given environment and
// arguments, returning stdout, stderr and the exit code.
func runFixture(t *testing.T, binary string, env map[string]string, args ...string) (string, string, int) {
	t.Helper()

	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "BRAINTRUST_API_KEY=")
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	var exitErr *exec.ExitError
	if err != nil {
		if !asExitError(err, &exitErr) {
			t.Fatalf("failed to run fixture: %v", err)
		}
		exitCode = exitErr.ExitCode()
	}

	return stdout.String(), stderr.String(), exitCode
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// bt builds argv for its own runner scripts and splices in its embedded script
// path plus the eval files, so a Go binary invoked through --runner receives
// arguments that mean nothing to it. Reading them would break the prototype
// invocation and any future bt argv change.
func TestSubprocess_IgnoresArgv(t *testing.T) {
	binary := buildRunnerFixture(t)

	stdout, stderr, code := runFixture(t, binary,
		map[string]string{"BT_EVAL_DEV_MODE": "list", "BT_EVAL_SSE_SOCK": "/nonexistent.sock"},
		"/tmp/bt/eval-runner.py", "./dummy.eval.py", "--not-a-real-flag",
	)

	require.Equal(t, 0, code, "argv should be ignored entirely\nstderr: %s", stderr)

	var manifest map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &manifest))
	assert.Contains(t, manifest, "food-classifier")
}

// bt scans the child's stdout lines in reverse for the last JSON-parseable one
// and returns it to the browser as the manifest, so anything else we print
// there can be mistaken for the manifest.
func TestSubprocess_ListModeStdoutIsOnlyTheManifest(t *testing.T) {
	binary := buildRunnerFixture(t)

	stdout, _, code := runFixture(t, binary,
		map[string]string{"BT_EVAL_DEV_MODE": "list", "BT_EVAL_SSE_SOCK": "/nonexistent.sock"})

	require.Equal(t, 0, code)

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	require.Len(t, lines, 1, "stdout must hold exactly the manifest, got:\n%s", stdout)
	assert.True(t, json.Valid([]byte(lines[0])))
}

// List mode must not need credentials: bt polls it while a playground is open,
// and the manifest needs no API access.
func TestSubprocess_ListModeWorksWithoutCredentials(t *testing.T) {
	binary := buildRunnerFixture(t)

	stdout, stderr, code := runFixture(t, binary,
		map[string]string{"BT_EVAL_DEV_MODE": "list", "BT_EVAL_SSE_SOCK": "/nonexistent.sock"})

	assert.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "food-classifier")
}

// bt appends its own `error` frame whenever a child exits non-zero without
// having sent one, and the playground renders that as a failed run. So a
// reported failure must still exit 0.
func TestSubprocess_ReportedFailureExitsZero(t *testing.T) {
	binary := buildRunnerFixture(t)

	bt := startFakeBT(t)
	_, stderr, code := runFixture(t, binary, map[string]string{
		"BT_EVAL_DEV_MODE":         "eval",
		"BT_EVAL_DEV_REQUEST_JSON": `{"name":"does-not-exist","data":{"data":[]}}`,
		"BT_EVAL_SSE_SOCK":         bt.sockPath,
	})

	assert.Equal(t, 0, code, "stderr: %s", stderr)

	frames := bt.collected(t)
	require.NotEmpty(t, frames, "the failure must be reported over the socket instead")
	assert.Equal(t, "error", frames[0].event)
}

// Running the binary directly must not reach Braintrust or hang: it should say
// what is registered and stop.
func TestSubprocess_BareRunListsAndExits(t *testing.T) {
	binary := buildRunnerFixture(t)

	stdout, stderr, code := runFixture(t, binary, map[string]string{})

	assert.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, stdout, "Registered evals:")
	assert.Contains(t, stdout, "food-classifier")
}
