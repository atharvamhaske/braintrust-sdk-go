package internal

// Shared normalization for the Claude Messages API request format, used by both
// the native Anthropic integration and the Bedrock InvokeModel integration,
// which send the same payload shape.

// NormalizeClaudeTools converts a Claude request's tool definitions into the
// shape the Braintrust instrumentation spec requires for metadata.tools.
//
// User-defined function tools become OpenAI Chat Completions tool objects.
// Claude's built-in server-side tools (web_search, computer, bash, text_editor,
// code_execution, ...) are passed through with their provider-native type and
// configuration intact, because they are not function-like and must not be
// given a fabricated function name or input schema.
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

// isClaudeFunctionTool reports whether a tool definition is a user-defined
// function tool. Claude identifies built-in server-side tools by a versioned
// type (e.g. "web_search_20250305"); user-defined tools carry an input_schema
// and either no type or the explicit "custom" type.
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
// vocabulary the spec mandates. It returns nil for unrecognized values so that
// no unspecified data is emitted.
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
