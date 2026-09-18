package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// input_json_delta must fill in a block type only when the content_block_start
// did not already provide one, so server-side tool types survive streaming, and
// the accumulated JSON must land in `input` as an object for every tool type.
func TestClaudeStreamAccumulatorToolInputBlocks(t *testing.T) {
	tests := []struct {
		name   string
		block  map[string]any
		deltas []string
		want   map[string]any
	}{
		{
			name:   "server_tool_use keeps its type across split json",
			block:  map[string]any{"type": "server_tool_use", "id": "srvtoolu_01", "name": "web_search", "input": map[string]any{}},
			deltas: []string{`{"query"`, `: "search terms"}`},
			want:   map[string]any{"type": "server_tool_use", "id": "srvtoolu_01", "name": "web_search", "input": map[string]any{"query": "search terms"}},
		},
		{
			name:   "mcp_tool_use keeps its type and server_name",
			block:  map[string]any{"type": "mcp_tool_use", "id": "mcptoolu_01", "name": "list_files", "server_name": "filesystem", "input": map[string]any{}},
			deltas: []string{`{"path": "/tmp"}`},
			want:   map[string]any{"type": "mcp_tool_use", "id": "mcptoolu_01", "name": "list_files", "server_name": "filesystem", "input": map[string]any{"path": "/tmp"}},
		},
		{
			name:   "plain tool_use still works",
			block:  map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_weather"},
			deltas: []string{`{"location": "Paris"}`},
			want:   map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_weather", "input": map[string]any{"location": "Paris"}},
		},
		{
			// A tool taking no arguments streams no input_json_delta at all, so
			// the empty accumulated string must not replace the object input.
			name:  "no input deltas keeps the object input",
			block: map[string]any{"type": "server_tool_use", "id": "srvtoolu_01", "name": "web_search", "input": map[string]any{}},
			want:  map[string]any{"type": "server_tool_use", "id": "srvtoolu_01", "name": "web_search", "input": map[string]any{}},
		},
		{
			name:  "a block reporting no input at all defaults to an object",
			block: map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_time"},
			want:  map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_time", "input": map[string]any{}},
		},
		{
			// Without a content_block_start there is no type to preserve, so the
			// delta supplies the default.
			name:   "deltas with no content_block_start default to tool_use",
			deltas: []string{`{"location": "Paris"}`},
			want:   map[string]any{"type": "tool_use", "input": map[string]any{"location": "Paris"}},
		},
		{
			name:   "malformed json is passed through as a string",
			block:  map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_weather"},
			deltas: []string{`{"location": `},
			want:   map[string]any{"type": "tool_use", "id": "toolu_01", "name": "get_weather", "input": `{"location": `},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewClaudeStreamAccumulator()
			if tt.block != nil {
				a.Add(map[string]any{
					"type":          "content_block_start",
					"index":         float64(0),
					"content_block": tt.block,
				})
			}
			for _, partial := range tt.deltas {
				a.Add(map[string]any{
					"type":  "content_block_delta",
					"index": float64(0),
					"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
				})
			}

			output := a.Output()
			require.Len(t, output, 1)
			assert.Equal(t, "assistant", output[0]["role"])
			content, ok := output[0]["content"].([]any)
			require.True(t, ok)
			require.Len(t, content, 1)
			assert.Equal(t, tt.want, content[0])
		})
	}
}
