package eval

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParameterSchemaResolve_DefaultsOnly(t *testing.T) {
	t.Parallel()

	schema := ParameterSchema{
		"model":      {Type: "model", Default: "gpt-4o"},
		"max_length": {Type: "number", Default: 100.0},
	}

	// No supplied values -> declared defaults apply.
	got, err := schema.Resolve(nil)
	require.NoError(t, err)

	assert.Equal(t, "gpt-4o", got.String("model"))
	assert.Equal(t, 100, got.Int("max_length"))
}

func TestParameterSchemaResolve_SuppliedValueWins(t *testing.T) {
	t.Parallel()

	schema := ParameterSchema{"model": {Type: "model", Default: "gpt-4o"}}

	got, err := schema.Resolve(map[string]any{"model": "claude"})
	require.NoError(t, err)

	assert.Equal(t, "claude", got.String("model"))
}

// A value the schema does not declare is still delivered, matching the other
// SDKs: the playground may send a parameter the code has not declared, and
// dropping it silently would be worse than surfacing it.
func TestParameterSchemaResolve_PassesThroughUndeclaredKeys(t *testing.T) {
	t.Parallel()

	got, err := ParameterSchema(nil).Resolve(map[string]any{"adhoc": "value"})
	require.NoError(t, err)

	assert.Equal(t, "value", got.String("adhoc"))
}

// Nil rather than an empty map, so a task can tell "no parameters" from
// "an empty set".
func TestParameterSchemaResolve_NothingIsNil(t *testing.T) {
	t.Parallel()

	got, err := ParameterSchema(nil).Resolve(nil)
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = ParameterSchema{}.Resolve(map[string]any{})
	require.NoError(t, err)
	assert.Nil(t, got)
}

// A declaration with no default contributes no value, so the task sees the
// parameter as absent rather than as a zero value.
func TestParameterSchemaResolve_DeclarationWithoutDefaultIsAbsent(t *testing.T) {
	t.Parallel()

	got, err := ParameterSchema{"threshold": {Type: "number"}}.Resolve(nil)
	require.NoError(t, err)

	assert.False(t, got.Has("threshold"))
}

// Resolve runs twice per playground request -- once up front so a bad value can
// be reported before anything is created in Braintrust, and again when the run
// is assembled. Resolving an already-resolved set must therefore be a no-op.
func TestParameterSchemaResolve_IsIdempotent(t *testing.T) {
	t.Parallel()

	schema := ParameterSchema{
		"model":     {Type: ParameterTypeModel, Default: "gpt-4o"},
		"threshold": {Type: "number", Default: 0.5},
	}

	once, err := schema.Resolve(map[string]any{"threshold": 0.9})
	require.NoError(t, err)

	twice, err := schema.Resolve(once)
	require.NoError(t, err)

	assert.Equal(t, once, twice)
}
