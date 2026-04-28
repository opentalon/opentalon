package orchestrator

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
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
	var sawBlock bool // true if we found at least one [tool_call] tag
	rest := response
	for {
		start := strings.Index(rest, "[tool_call]")
		if start < 0 {
			break
		}
		sawBlock = true
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
		if strings.HasPrefix(body, "{") {
			// Body is a bare JSON object without a "tool" key — the LLM emitted
			// just the args and dropped the tool name. Return a ToolCall with an
			// empty Plugin so executeCall can give a specific format-hint error
			// instead of the generic "could not be executed" strip-retry message.
			calls = append(calls, ToolCall{
				ID:     fmt.Sprintf("call-%d", len(calls)+1),
				Plugin: "",
				Action: "",
				Args:   make(map[string]string),
			})
		} else {
			if call, ok := parseInlineToolCall(body, len(calls)+1); ok {
				calls = append(calls, call)
			}
		}
	}
	// Fallback: the entire response is a bare JSON tool call without [tool_call] tags.
	// LLMs sometimes emit {"tool": "plugin.action", "args": {...}} as plain text.
	if len(calls) == 0 {
		trimmed := strings.TrimSpace(response)
		var block toolCallJSON
		if json.Unmarshal([]byte(trimmed), &block) == nil && block.Tool != "" {
			plugin, action, err := parseToolName(block.Tool)
			if err == nil {
				return []ToolCall{{
					ID:     "call-1",
					Plugin: plugin,
					Action: action,
					Args:   toStringMap(block.Args),
				}}
			}
		}
		// Fallback: parse Claude's native <function_calls> XML format.
		// The LLM sometimes emits <invoke name="plugin.action"><parameter name="key">value</parameter></invoke>
		// instead of [tool_call]. Parse it rather than rejecting it.
		if xmlCalls := parseXMLFunctionCalls(response); len(xmlCalls) > 0 {
			return xmlCalls
		}
		// We found [tool_call] blocks (or Claude's native XML variants) but
		// none parsed — return a placeholder so executeCall sends the
		// format-hint error back to the LLM instead of the silent
		// strip-retry → "(no response)" path.
		if sawBlock || containsInternalBlock(response) {
			return []ToolCall{{
				ID:     "call-1",
				Plugin: "",
				Action: "",
				Args:   make(map[string]string),
			}}
		}
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
			// Use decimal notation to avoid scientific notation (e.g. 2.004555e+06)
			// that breaks downstream consumers expecting plain integers.
			if val == math.Trunc(val) && !math.IsInf(val, 0) && !math.IsNaN(val) {
				result[k] = strconv.FormatInt(int64(val), 10)
			} else {
				result[k] = strconv.FormatFloat(val, 'f', -1, 64)
			}
		case bool:
			if !val {
				continue
			}
			result[k] = "true"
		case map[string]interface{}, []interface{}:
			// Nested object/array — JSON-encode so tools can json.Unmarshal it
			// back (e.g. scheduler's create_job and remind_me accept an
			// `args` JSON object). Go's default %v formatter would emit
			// "map[key:val]" which no JSON parser accepts.
			b, err := json.Marshal(val)
			if err != nil {
				result[k] = fmt.Sprintf("%v", val)
			} else {
				result[k] = string(b)
			}
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

// parseXMLFunctionCalls extracts tool calls from Claude's native XML format:
//
//	<function_calls>
//	<invoke name="plugin.action">
//	<parameter name="key">value</parameter>
//	</invoke>
//	</function_calls>
//
// Also handles the antml: namespaced variant. Returns nil if no valid calls found.
func parseXMLFunctionCalls(response string) []ToolCall {
	var calls []ToolCall

	// Find all invoke blocks. Supports both <invoke> and namespaced variants.
	rest := response
	for {
		// Find next invoke tag (with or without namespace prefix).
		invokeTag := findNextInvoke(rest)
		if invokeTag.start < 0 {
			break
		}
		rest = rest[invokeTag.start:]

		// Extract name="..." attribute.
		nameAttr := extractAttr(rest, "name")
		if nameAttr == "" {
			rest = rest[1:]
			continue
		}

		// Find the body between > and the closing </invoke> (or self-closing />).
		body, advance := extractInvokeBody(rest)
		if advance <= 0 {
			break
		}
		rest = rest[advance:]

		plugin, action, err := parseToolName(nameAttr)
		if err != nil {
			continue
		}

		args := parseXMLParameters(body)
		calls = append(calls, ToolCall{
			ID:     fmt.Sprintf("call-%d", len(calls)+1),
			Plugin: plugin,
			Action: action,
			Args:   args,
		})
	}
	return calls
}

type invokePos struct{ start int }

func findNextInvoke(s string) invokePos {
	// Check both plain and any namespace-prefixed invoke tags.
	idx := strings.Index(s, "<invoke ")
	idx2 := strings.Index(s, "<invoke>")
	if idx < 0 || (idx2 >= 0 && idx2 < idx) {
		idx = idx2
	}
	return invokePos{start: idx}
}

// extractAttr extracts the value of attr="value" from an XML tag at the start of s.
func extractAttr(s string, attr string) string {
	needle := attr + `="`
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	i += len(needle)
	j := strings.Index(s[i:], `"`)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

// extractInvokeBody returns the body inside an <invoke>...</invoke> block
// and the number of bytes to advance past the block.
func extractInvokeBody(s string) (body string, advance int) {
	// Find the closing > of the opening tag.
	gt := strings.Index(s, ">")
	if gt < 0 {
		return "", 0
	}
	// Self-closing tag: <invoke name="x"/>
	if gt > 0 && s[gt-1] == '/' {
		return "", gt + 1
	}

	bodyStart := gt + 1
	// Find closing </invoke> (any namespace).
	for _, close := range []string{"</invoke>", "</invoke>"} {
		if end := strings.Index(s[bodyStart:], close); end >= 0 {
			return s[bodyStart : bodyStart+end], bodyStart + end + len(close)
		}
	}
	// No closing tag — take everything after >.
	return s[bodyStart:], len(s)
}

// parseXMLParameters extracts parameter name/value pairs from XML like:
// <parameter name="key">value</parameter>
func parseXMLParameters(body string) map[string]string {
	args := make(map[string]string)
	rest := body
	for {
		// Find next <parameter (with or without namespace).
		idx := strings.Index(rest, "<parameter ")
		if idx < 0 {
			break
		}
		rest = rest[idx:]

		name := extractAttr(rest, "name")
		if name == "" {
			rest = rest[1:]
			continue
		}

		// Find > closing the opening tag.
		gt := strings.Index(rest, ">")
		if gt < 0 {
			break
		}
		rest = rest[gt+1:]

		// Find closing parameter tag.
		closeTag := "<" + "/parameter>"
		closeIdx := strings.Index(rest, closeTag)
		if closeIdx < 0 {
			break
		}
		args[name] = strings.TrimSpace(rest[:closeIdx])
		rest = rest[closeIdx+len(closeTag):]
	}
	return args
}

// containsInternalBlock returns true if s contains any internal protocol
// block opening tag.
func containsInternalBlock(s string) bool {
	for _, tags := range internalBlockTags {
		if strings.Contains(s, tags[0]) {
			return true
		}
	}
	return false
}

// internalBlockTags lists the open/close tag pairs for internal protocol
// blocks that must never be forwarded to channel users.
//
// The <function_calls> and <function_calls> pairs are Claude's native
// function-call XML. Our prompt tells models to use [tool_call], but trained
// behaviour occasionally leaks through in the reply; strip it so end users
// don't see raw protocol tags.
var internalBlockTags = [][2]string{
	{"[tool_call]", "[/tool_call]"},
	{"[plugin_output]", "[/plugin_output]"},
	{"<function_calls>", "</function_calls>"},
	{"<" + "antml:function_calls>", "<" + "/antml:function_calls>"},
}

// StripInternalBlocks removes any internal protocol blocks from s
// ([tool_call]...[/tool_call] and [plugin_output]...[/plugin_output]).
// Used to clean up LLM responses so raw protocol content is never forwarded
// to the channel (e.g. when the LLM echoes plugin output or uses an
// unparseable tool name).
func StripInternalBlocks(s string) string {
	for _, tags := range internalBlockTags {
		s = stripTaggedBlocks(s, tags[0], tags[1])
	}
	// TrimSpace normalises the result: block removal can leave leading/trailing
	// newlines, and LLM replies with surrounding whitespace carry no meaning.
	return strings.TrimSpace(s)
}

func stripTaggedBlocks(s, open, close string) string {
	var sb strings.Builder
	rest := s
	for {
		start := strings.Index(rest, open)
		if start < 0 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:start])
		rest = rest[start+len(open):]
		end := strings.Index(rest, close)
		if end >= 0 {
			rest = rest[end+len(close):]
		} else {
			// No closing tag — drop everything after the opening tag.
			break
		}
	}
	return sb.String()
}

// parseToolName splits "plugin.action" or "plugin__action" into ("plugin", "action").
// Both parts must be valid identifiers: plugin allows [a-zA-Z0-9_.-],
// action allows [a-zA-Z0-9_-]. This rejects natural-language fragments
// that an LLM might accidentally emit inside [tool_call] blocks.
//
// The dot separator is preferred; double-underscore is a fallback because
// LLMs trained on OpenAI-style function calling frequently emit names like
// "jira__search_issues" instead of "jira.search_issues".
func parseToolName(s string) (plugin, action string, err error) {
	// Preferred: split on last dot.
	if dot := strings.LastIndex(s, "."); dot > 0 && dot < len(s)-1 {
		plugin, action = s[:dot], s[dot+1:]
		if isValidPluginName(plugin) && isValidActionName(action) {
			return plugin, action, nil
		}
	}

	// Fallback: split on first "__" (double underscore).
	if dunder := strings.Index(s, "__"); dunder > 0 && dunder < len(s)-2 {
		plugin, action = s[:dunder], s[dunder+2:]
		if isValidPluginName(plugin) && isValidActionName(action) {
			return plugin, action, nil
		}
	}

	return "", "", fmt.Errorf("invalid tool name %q", s)
}

func isValidPluginName(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func isValidActionName(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
