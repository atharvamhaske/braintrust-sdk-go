package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeStreamAccumulatorPreservesServerToolUseType(t *testing.T) {
	a := NewClaudeStreamAccumulator()

	events := []map[string]any{
		{
			"type":  "content_block_start",
			"index": float64(0),
			"content_block": map[string]any{
				"type":  "server_tool_use",
				"id":    "srvtoolu_01",
				"name":  "web_search",
				"input": map[string]any{},
			},
		},
		{
			"type":  "content_block_delta",
			"index": float64(0),
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": `{"query"`,
			},
		},
		{
			"type":  "content_block_delta",
			"index": float64(0),
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": `: "search terms"}`,
			},
		},
	}
	for _, event := range events {
		a.Add(event)
	}

	output := a.Output()
	require.Len(t, output, 1)
	content, ok := output[0]["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "server_tool_use", block["type"])
	assert.Equal(t, map[string]any{"query": "search terms"}, block["input"])
}

func TestClaudeStreamAccumulatorPreservesMCPToolUseType(t *testing.T) {
	a := NewClaudeStreamAccumulator()

	events := []map[string]any{
		{
			"type":  "content_block_start",
			"index": float64(0),
			"content_block": map[string]any{
				"type":        "mcp_tool_use",
				"id":          "mcptoolu_01",
				"name":        "list_files",
				"server_name": "filesystem",
				"input":       map[string]any{},
			},
		},
		{
			"type":  "content_block_delta",
			"index": float64(0),
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": `{"path": "/tmp"}`,
			},
		},
	}
	for _, event := range events {
		a.Add(event)
	}

	output := a.Output()
	require.Len(t, output, 1)
	content, ok := output[0]["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "mcp_tool_use", block["type"])
	assert.Equal(t, "filesystem", block["server_name"])
	assert.Equal(t, map[string]any{"path": "/tmp"}, block["input"])
}

func TestClaudeStreamAccumulatorPlainToolUseStillWorks(t *testing.T) {
	a := NewClaudeStreamAccumulator()

	events := []map[string]any{
		{
			"type":  "content_block_start",
			"index": float64(0),
			"content_block": map[string]any{
				"type": "tool_use",
				"id":   "toolu_01",
				"name": "get_weather",
			},
		},
		{
			"type":  "content_block_delta",
			"index": float64(0),
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": `{"location": "Paris"}`,
			},
		},
	}
	for _, event := range events {
		a.Add(event)
	}

	output := a.Output()
	require.Len(t, output, 1)
	content, ok := output[0]["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)

	block, ok := content[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "tool_use", block["type"])
	assert.Equal(t, map[string]any{"location": "Paris"}, block["input"])
}
