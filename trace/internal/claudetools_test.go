package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeClaudeTools(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"location": map[string]any{"type": "string"}},
		"required":   []any{"location"},
	}

	tests := []struct {
		name  string
		tools any
		want  []any
	}{
		{
			name:  "user-defined tool converts to the OpenAI shape",
			tools: []any{map[string]any{"name": "get_weather", "description": "Get the weather", "input_schema": schema}},
			want: []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_weather", "description": "Get the weather", "parameters": schema},
			}},
		},
		{
			// Anthropic tags user-defined tools with an explicit "custom" type;
			// they are still function-like.
			name:  "explicit custom type converts and keeps strict",
			tools: []any{map[string]any{"type": "custom", "name": "get_weather", "input_schema": schema, "strict": true}},
			want: []any{map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "get_weather", "parameters": schema, "strict": true},
			}},
		},
		{
			// Built-in server-side tools are not function-like, so their native
			// type and config must survive rather than becoming a fake function.
			name:  "built-in tool keeps its native type and config",
			tools: []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": float64(1)}},
			want:  []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": float64(1)}},
		},
		{"non-list is ignored", "tools", nil},
		{"nil is ignored", nil, nil},
		{"entries that are not objects are skipped", []any{"not-a-tool"}, []any{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeClaudeTools(tt.tools))
		})
	}
}

func TestNormalizeClaudeToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		choice any
		want   any
	}{
		{"auto", map[string]any{"type": "auto"}, "auto"},
		{"any means required", map[string]any{"type": "any"}, "required"},
		{"none", map[string]any{"type": "none"}, "none"},
		{
			"named tool",
			map[string]any{"type": "tool", "name": "get_weather"},
			map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
		},
		{"unknown type is dropped", map[string]any{"type": "bogus"}, nil},
		{"non-object is dropped", "auto", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeClaudeToolChoice(tt.choice))
		})
	}
}
