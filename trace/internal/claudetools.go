package internal

// Shared normalization for the Claude Messages API request format, which both
// the Anthropic and Bedrock InvokeModel integrations receive.

// NormalizeClaudeTools converts user-defined tools into the OpenAI Chat
// Completions shape for metadata.tools. Built-in server-side tools (web_search,
// computer, bash, ...) pass through untouched: they aren't function-like, so a
// fabricated function name or schema would misrepresent them.
func NormalizeClaudeTools(value any) []any {
	tools, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if !isClaudeFunctionTool(tool) {
			result = append(result, rawTool)
			continue
		}
		function := map[string]any{}
		for _, key := range []string{"name", "description"} {
			if value, exists := tool[key]; exists {
				function[key] = value
			}
		}
		if parameters, exists := tool["input_schema"]; exists {
			function["parameters"] = parameters
		}
		if strict, exists := tool["strict"]; exists && strict != nil {
			function["strict"] = strict
		}
		result = append(result, map[string]any{"type": "function", "function": function})
	}
	return result
}

// isClaudeFunctionTool reports whether a tool is user-defined. Built-ins carry a
// versioned type (e.g. "web_search_20250305"); user tools carry an input_schema
// with either no type or type "custom".
func isClaudeFunctionTool(tool map[string]any) bool {
	if _, hasSchema := tool["input_schema"]; !hasSchema {
		return false
	}
	toolType, hasType := tool["type"]
	if !hasType || toolType == nil {
		return true
	}
	return toolType == "custom"
}

// NormalizeClaudeToolChoice maps Claude's tool_choice onto the OpenAI
// vocabulary, returning nil for unrecognized values.
func NormalizeClaudeToolChoice(value any) any {
	choice, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	switch choice["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := choice["name"].(string)
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	default:
		return nil
	}
}
