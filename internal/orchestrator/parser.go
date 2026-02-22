package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolCallJSON is the shape we expect inside [tool_call]...[/tool_call].
type toolCallJSON struct {
	Tool string            `json:"tool"`
	Args map[string]string `json:"args"`
}

// DefaultParser parses LLM response text for [tool_call]...[/tool_call] blocks
// with JSON like {"tool": "plugin.action", "args": {...}}. Returns nil if no
// tool calls are found (response is the final answer).
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
		end := strings.Index(rest, "[/tool_call]")
		if end < 0 {
			break
		}
		body := strings.TrimSpace(rest[:end])
		rest = rest[end+len("[/tool_call]"):]
		var block toolCallJSON
		if err := json.Unmarshal([]byte(body), &block); err != nil {
			continue
		}
		if block.Tool == "" {
			continue
		}
		plugin, action, err := parseToolName(block.Tool)
		if err != nil {
			continue
		}
		if block.Args == nil {
			block.Args = make(map[string]string)
		}
		calls = append(calls, ToolCall{
			ID:     fmt.Sprintf("call-%d", len(calls)+1),
			Plugin: plugin,
			Action: action,
			Args:   block.Args,
		})
	}
	if len(calls) == 0 {
		return nil
	}
	return calls
}

// parseToolName splits "plugin.action" into ("plugin", "action").
func parseToolName(s string) (plugin, action string, err error) {
	dot := strings.LastIndex(s, ".")
	if dot <= 0 || dot == len(s)-1 {
		return "", "", fmt.Errorf("invalid tool name %q", s)
	}
	return s[:dot], s[dot+1:], nil
}
