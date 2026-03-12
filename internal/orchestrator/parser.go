package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolCallJSON is the shape we expect inside [tool_call]...[/tool_call].
// Args uses interface{} to accept mixed types (string, number, bool) from LLMs.
type toolCallJSON struct {
	Tool string                 `json:"tool"`
	Args map[string]interface{} `json:"args"`
}

// DefaultParser parses LLM response text for tool call blocks. It supports two formats:
//
// Format A (structured):
//
//	[tool_call]
//	{"tool": "plugin.action", "args": {"key": "value"}}
//	[/tool_call]
//
// Format B (inline, common LLM output):
//
//	[tool_call] plugin.action
//	{"key": "value", "count": 10}
//	[/tool_call]    (closing tag optional)
//
// Returns nil if no tool calls are found (response is the final answer).
var DefaultParser ToolCallParser = defaultParser{}

type defaultParser struct{}

func (defaultParser) Parse(response string) []ToolCall {
	var calls []ToolCall
	rest := response
	for {
		start := strings.Index(rest, "[tool_call]")
		if start < 0 {
			break
		}
		rest = rest[start+len("[tool_call]"):]

		// Extract body: prefer [/tool_call] closing tag, fall back to end of string or next [tool_call].
		var body string
		end := strings.Index(rest, "[/tool_call]")
		nextStart := strings.Index(rest, "[tool_call]")
		switch {
		case end >= 0:
			body = strings.TrimSpace(rest[:end])
			rest = rest[end+len("[/tool_call]"):]
		case nextStart >= 0:
			body = strings.TrimSpace(rest[:nextStart])
			rest = rest[nextStart:]
		default:
			body = strings.TrimSpace(rest)
			rest = ""
		}

		if body == "" {
			continue
		}

		// Format A: {"tool": "plugin.action", "args": {...}}
		var block toolCallJSON
		if err := json.Unmarshal([]byte(body), &block); err == nil && block.Tool != "" {
			plugin, action, err := parseToolName(block.Tool)
			if err != nil {
				continue
			}
			args := toStringMap(block.Args)
			calls = append(calls, ToolCall{
				ID:     fmt.Sprintf("call-%d", len(calls)+1),
				Plugin: plugin,
				Action: action,
				Args:   args,
			})
			continue
		}

		// Format B: plugin.action\n{json_args}
		// Format C: plugin.action(key=value, key=value)
		// Skip if body starts with '{' — that's malformed Format A, not an inline call.
		if !strings.HasPrefix(body, "{") {
			if call, ok := parseInlineToolCall(body, len(calls)+1); ok {
				calls = append(calls, call)
			}
		}
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// parseInlineToolCall parses these formats:
//
//	plugin.action\n{json_args}           (Format B)
//	plugin.action(key=value, key=value)  (Format C)
//	plugin.action                        (no args)
func parseInlineToolCall(body string, callNum int) (ToolCall, bool) {
	lines := strings.SplitN(body, "\n", 2)
	firstLine := strings.TrimSpace(lines[0])

	// Format C: plugin.action(key=value, key=value)
	if paren := strings.IndexByte(firstLine, '('); paren > 0 {
		toolName := firstLine[:paren]
		plugin, action, err := parseToolName(toolName)
		if err != nil {
			return ToolCall{}, false
		}
		args := make(map[string]string)
		argsStr := strings.TrimSuffix(strings.TrimSpace(firstLine[paren+1:]), ")")
		if argsStr != "" {
			for _, pair := range strings.Split(argsStr, ", ") {
				eq := strings.IndexByte(pair, '=')
				if eq > 0 {
					val := strings.TrimSpace(pair[eq+1:])
					val = stripSurroundingQuotes(val)
					args[strings.TrimSpace(pair[:eq])] = val
				}
			}
		}
		return ToolCall{
			ID:     fmt.Sprintf("call-%d", callNum),
			Plugin: plugin,
			Action: action,
			Args:   args,
		}, true
	}

	// Format B: plugin.action\n{json_args} or just plugin.action
	plugin, action, err := parseToolName(firstLine)
	if err != nil {
		return ToolCall{}, false
	}

	args := make(map[string]string)
	if len(lines) > 1 {
		jsonPart := strings.TrimSpace(lines[1])
		if jsonPart != "" {
			var rawArgs map[string]interface{}
			if err := json.Unmarshal([]byte(jsonPart), &rawArgs); err == nil {
				args = toStringMap(rawArgs)
			}
		}
	}

	return ToolCall{
		ID:     fmt.Sprintf("call-%d", callNum),
		Plugin: plugin,
		Action: action,
		Args:   args,
	}, true
}

// toStringMap converts map[string]interface{} to map[string]string.
// Zero-value non-string types (0, false, nil) are skipped because LLMs
// often emit them as placeholders for omitted optional parameters.
func toStringMap(m map[string]interface{}) map[string]string {
	if m == nil {
		return make(map[string]string)
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			result[k] = val
		case nil:
			// skip
		case float64:
			if val == 0 {
				continue
			}
			result[k] = fmt.Sprintf("%v", val)
		case bool:
			if !val {
				continue
			}
			result[k] = "true"
		default:
			result[k] = fmt.Sprintf("%v", val)
		}
	}
	return result
}

// stripSurroundingQuotes removes matching outer quotes (single or double)
// that LLMs sometimes wrap around argument values.
func stripSurroundingQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// parseToolName splits "plugin.action" into ("plugin", "action").
func parseToolName(s string) (plugin, action string, err error) {
	dot := strings.LastIndex(s, ".")
	if dot <= 0 || dot == len(s)-1 {
		return "", "", fmt.Errorf("invalid tool name %q", s)
	}
	return s[:dot], s[dot+1:], nil
}
