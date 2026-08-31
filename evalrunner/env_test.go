package evalrunner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bt sets its booleans to the literal string "1" and omits them when false --
// it never writes "0". The other SDK runners nonetheless treat a small set of
// values as falsy and everything else as truthy; we match them so the same
// environment behaves identically across languages.
// See sdk-rust/src/eval/bt_runner.rs env_flag and bt/scripts/eval-runner.py.
func TestEnvFlag(t *testing.T) {
	truthy := []string{"1", "true", "yes", "on", "anything", "FALSE"}
	falsy := []string{"", "0", "false", "no", "off"}

	for _, v := range truthy {
		assert.True(t, envFlag(func(string) string { return v }, "X"), "expected %q to be truthy", v)
	}
	for _, v := range falsy {
		assert.False(t, envFlag(func(string) string { return v }, "X"), "expected %q to be falsy", v)
	}
}

func TestReadEnv_DevModes(t *testing.T) {
	e := readEnv(mapLookup(map[string]string{
		"BT_EVAL_DEV_MODE":         "eval",
		"BT_EVAL_DEV_REQUEST_JSON": `{"name":"x"}`,
		"BT_EVAL_SSE_SOCK":         "/tmp/x.sock",
	}))

	assert.Equal(t, "eval", e.DevMode)
	assert.Equal(t, `{"name":"x"}`, e.RequestJSON)
	assert.Equal(t, "/tmp/x.sock", e.SSESock)
}

func TestReadEnv_NoSendLogsIsEitherVar(t *testing.T) {
	assert.True(t, readEnv(mapLookup(map[string]string{"BT_EVAL_LOCAL": "1"})).NoSendLogs)
	assert.True(t, readEnv(mapLookup(map[string]string{"BT_EVAL_NO_SEND_LOGS": "1"})).NoSendLogs)
	assert.False(t, readEnv(mapLookup(map[string]string{})).NoSendLogs)
}

func TestEnvMode(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
		want Mode
	}{
		{
			name: "bt asks for the manifest",
			vars: map[string]string{"BT_EVAL_DEV_MODE": "list", "BT_EVAL_SSE_SOCK": "/tmp/x.sock"},
			want: ModeList,
		},
		{
			name: "bt asks us to run one eval",
			vars: map[string]string{"BT_EVAL_DEV_MODE": "eval", "BT_EVAL_SSE_SOCK": "/tmp/x.sock"},
			want: ModeEval,
		},
		{
			// `bt eval ./cmd/evals` -- no dev server, but bt still spawned us
			// and always sets the socket path.
			name: "bt CLI batch run",
			vars: map[string]string{"BT_EVAL_SSE_SOCK": "/tmp/x.sock"},
			want: ModeBatch,
		},
		{
			name: "bt CLI batch run over TCP",
			vars: map[string]string{"BT_EVAL_SSE_ADDR": "127.0.0.1:9000"},
			want: ModeBatch,
		},
		{
			// Someone typed `go run ./cmd/evals`. Do not silently create
			// experiments as a side effect of checking that it compiles.
			name: "no bt anywhere",
			vars: map[string]string{},
			want: ModeInspect,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, readEnv(mapLookup(tt.vars)).Mode())
		})
	}
}

func TestEnvMode_UnknownDevModeIsAnError(t *testing.T) {
	e := readEnv(mapLookup(map[string]string{"BT_EVAL_DEV_MODE": "banana"}))
	assert.Equal(t, ModeUnknown, e.Mode())
}

func TestParseFilters_EmptyIsNoFilters(t *testing.T) {
	assert.Empty(t, parseFilters("", nil))
	assert.Empty(t, parseFilters("[]", nil))
}

// Everything about filter parsing is lenient: bt controls this value, and a
// malformed one must degrade to "run everything" rather than kill the run.
func TestParseFilters_MalformedInputIsIgnored(t *testing.T) {
	assert.Empty(t, parseFilters("not json", nil))
	assert.Empty(t, parseFilters(`[{"path":["name"],"pattern":"("}]`, nil), "uncompilable regex is dropped")
}

func TestEvalFilter_Matching(t *testing.T) {
	tests := []struct {
		name     string
		filters  string
		evalName string
		want     bool
	}{
		{"no filters matches everything", "", "food-classifier", true},
		{"empty path matches on name", `[{"path":[],"pattern":"food"}]`, "food-classifier", true},
		{"name path matches on name", `[{"path":["name"],"pattern":"food"}]`, "food-classifier", true},
		{"non-matching pattern excludes", `[{"path":["name"],"pattern":"tone"}]`, "food-classifier", false},
		{"pattern is a substring search, not anchored", `[{"path":["name"],"pattern":"class"}]`, "food-classifier", true},
		{"unknown path includes by default", `[{"path":["metadata","x"],"pattern":"nope"}]`, "food-classifier", true},
		{"any filter matching is enough", `[{"path":["name"],"pattern":"tone"},{"path":["name"],"pattern":"food"}]`, "food-classifier", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filters := parseFilters(tt.filters, nil)
			assert.Equal(t, tt.want, matchesFilters(filters, tt.evalName))
		})
	}
}

func TestReadEnv_ParsesFilters(t *testing.T) {
	e := readEnv(mapLookup(map[string]string{
		"BT_EVAL_FILTER_PARSED": `[{"path":["name"],"pattern":"food"}]`,
	}))

	require.Len(t, e.Filters, 1)
	assert.True(t, matchesFilters(e.Filters, "food-classifier"))
	assert.False(t, matchesFilters(e.Filters, "tone-checker"))
}

func mapLookup(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}
