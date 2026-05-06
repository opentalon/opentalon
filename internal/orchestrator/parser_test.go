package orchestrator

import (
	"testing"
)

func TestParseFormatA(t *testing.T) {
	response := `Let me search for that.
[tool_call]
{"tool": "brave-search.search", "args": {"query": "first human in space"}}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Action != "search" {
		t.Errorf("action = %q", calls[0].Action)
	}
	if calls[0].Args["query"] != "first human in space" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
}

func TestParseFormatB_WithClosingTag(t *testing.T) {
	response := `[tool_call] brave-search.search
{"query": "first human in space", "count": 10}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Action != "search" {
		t.Errorf("action = %q", calls[0].Action)
	}
	if calls[0].Args["query"] != "first human in space" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
	if calls[0].Args["count"] != "10" {
		t.Errorf("count = %q, want %q", calls[0].Args["count"], "10")
	}
}

func TestParseFormatB_NoClosingTag(t *testing.T) {
	response := `[tool_call] brave-search.search
{
  "query": "first human in space history",
  "count": 10
}`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Args["query"] != "first human in space history" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
	if calls[0].Args["count"] != "10" {
		t.Errorf("count = %q", calls[0].Args["count"])
	}
}

func TestParseFormatB_NoArgs(t *testing.T) {
	response := `[tool_call] gitlab.list_repos
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "gitlab" || calls[0].Action != "list_repos" {
		t.Errorf("call = %s.%s", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseMultipleCalls(t *testing.T) {
	response := `[tool_call]
{"tool": "brave-search.search", "args": {"query": "cats"}}
[/tool_call]
[tool_call] gitlab.list_repos
{}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("call[0] plugin = %q", calls[0].Plugin)
	}
	if calls[1].Plugin != "gitlab" {
		t.Errorf("call[1] plugin = %q", calls[1].Plugin)
	}
}

func TestParseNoToolCalls(t *testing.T) {
	calls := DefaultParser.Parse("Hello! How can I help?")
	if calls != nil {
		t.Errorf("expected nil, got %v", calls)
	}
}

func TestParseInvalidToolName(t *testing.T) {
	response := `[tool_call] invalidname
{"query": "test"}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	// Invalid tool name still produces a placeholder so the LLM gets a
	// format-hint error instead of the strip-retry → "(no response)" path.
	if len(calls) != 1 {
		t.Fatalf("expected 1 placeholder call, got %d", len(calls))
	}
	if calls[0].Plugin != "" || calls[0].Action != "" {
		t.Errorf("expected empty Plugin/Action, got %q/%q", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseBareJSONArgs_ReturnsMissingToolCall(t *testing.T) {
	// When the LLM emits [tool_call]{...}[/tool_call] with just args (no "tool" key),
	// the parser should return a ToolCall with empty Plugin/Action so executeCall
	// can return a specific "missing tool name" error instead of dropping it silently.
	response := `[tool_call]
{
  "app_name": "MyApp",
  "app_environment": "production"
}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call for bare JSON args, got %d", len(calls))
	}
	if calls[0].Plugin != "" || calls[0].Action != "" {
		t.Errorf("expected empty Plugin/Action, got %q/%q", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseEmptyBody(t *testing.T) {
	response := `[tool_call][/tool_call]`
	calls := DefaultParser.Parse(response)
	// Empty body still triggers a placeholder so executeCall returns a format-hint
	// error instead of the silent strip-retry → "(no response)" path.
	if len(calls) != 1 {
		t.Fatalf("expected 1 placeholder call for empty body, got %d", len(calls))
	}
	if calls[0].Plugin != "" || calls[0].Action != "" {
		t.Errorf("expected empty Plugin/Action, got %q/%q", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseUnparseableBodyReturnsPlaceholder(t *testing.T) {
	// LLM wrapped natural language inside [tool_call] tags — not a valid tool call.
	// Parser should return a placeholder so the LLM gets a format-hint error
	// instead of the response being silently stripped to "(no response)".
	response := `[tool_call]
fetch all person types
[/tool_call]`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 placeholder call, got %d", len(calls))
	}
	if calls[0].Plugin != "" || calls[0].Action != "" {
		t.Errorf("expected empty Plugin/Action, got %q/%q", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseFormatA_MixedArgTypes(t *testing.T) {
	// LLMs often send "count": 10 (integer) instead of "count": "10" (string)
	response := `[tool_call] {"tool": "brave-search.search", "args": {"query": "SpaceX launch news", "count": 10, "freshness": "pd"}}
`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Args["query"] != "SpaceX launch news" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
	if calls[0].Args["count"] != "10" {
		t.Errorf("count = %q, want %q", calls[0].Args["count"], "10")
	}
	if calls[0].Args["freshness"] != "pd" {
		t.Errorf("freshness = %q", calls[0].Args["freshness"])
	}
}

func TestParseFormatA_LargeNumericID(t *testing.T) {
	// Large numeric IDs must not be converted to scientific notation (e.g. 2.004555e+06).
	response := `[tool_call] {"tool": "timly.timly__assign-item", "args": {"item_id": 2004555, "container_id": 170909}}
`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Args["item_id"] != "2004555" {
		t.Errorf("item_id = %q, want %q", calls[0].Args["item_id"], "2004555")
	}
	if calls[0].Args["container_id"] != "170909" {
		t.Errorf("container_id = %q, want %q", calls[0].Args["container_id"], "170909")
	}
}

func TestParseFormatC_ParenArgs(t *testing.T) {
	response := `[tool_call] brave-search.search(query=SpaceX launch today, freshness=pd)
`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Action != "search" {
		t.Errorf("action = %q", calls[0].Action)
	}
	if calls[0].Args["query"] != "SpaceX launch today" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
	if calls[0].Args["freshness"] != "pd" {
		t.Errorf("freshness = %q", calls[0].Args["freshness"])
	}
}

func TestParseFormatC_NoArgs(t *testing.T) {
	response := `[tool_call] opentalon.list_commands
`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "opentalon" || calls[0].Action != "list_commands" {
		t.Errorf("call = %s.%s", calls[0].Plugin, calls[0].Action)
	}
}

func TestParseFormatA_InlineNoClosingTag(t *testing.T) {
	// LLM puts JSON on same line as [tool_call] without [/tool_call]
	response := `[tool_call] {"tool": "brave-search.search", "args": {"query": "test"}}` + "\n"

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "brave-search" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Args["query"] != "test" {
		t.Errorf("query = %q", calls[0].Args["query"])
	}
}

func TestParseFormatA_HyphenatedMCPAction(t *testing.T) {
	// MCP tools commonly use hyphens in action names (e.g. list-org-units).
	response := `[tool_call]
{"tool": "myapp.myapp__list-org-units", "args": {}}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "myapp" {
		t.Errorf("plugin = %q, want %q", calls[0].Plugin, "myapp")
	}
	if calls[0].Action != "myapp__list-org-units" {
		t.Errorf("action = %q, want %q", calls[0].Action, "myapp__list-org-units")
	}
}

func TestParseFormatA_DoubleUnderscore(t *testing.T) {
	// LLMs trained on OpenAI function calling often emit "plugin__action"
	// instead of "plugin.action".
	response := `[tool_call]
{"tool": "jira__search_issues", "args": {"jql": "assignee = alice@example.com"}}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "jira" {
		t.Errorf("plugin = %q, want %q", calls[0].Plugin, "jira")
	}
	if calls[0].Action != "search_issues" {
		t.Errorf("action = %q, want %q", calls[0].Action, "search_issues")
	}
	if calls[0].Args["jql"] != "assignee = alice@example.com" {
		t.Errorf("jql = %q", calls[0].Args["jql"])
	}
}

func TestParseFormatB_DoubleUnderscore(t *testing.T) {
	response := `[tool_call] jira__search_issues
{"jql": "assignee = alice@example.com"}
[/tool_call]`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "jira" {
		t.Errorf("plugin = %q, want %q", calls[0].Plugin, "jira")
	}
	if calls[0].Action != "search_issues" {
		t.Errorf("action = %q, want %q", calls[0].Action, "search_issues")
	}
}

func TestStripInternalBlocks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "strips tool_call block",
			input: "[tool_call]\n{\"tool\": \"jira__jira_search\", \"args\": {}}\n[/tool_call]",
			want:  "",
		},
		{
			name:  "strips plugin_output block",
			input: "Here are the results:\n[plugin_output]\n{\"issues\": []}\n[/plugin_output]\nDone.",
			want:  "Here are the results:\n\nDone.",
		},
		{
			name:  "strips both block types",
			input: "[tool_call]{\"tool\":\"bad__name\"}[/tool_call]\n[plugin_output]raw output[/plugin_output]\nFinal answer.",
			want:  "Final answer.",
		},
		{
			name:  "preserves surrounding text around tool_call",
			input: "Here is my answer.\n[tool_call]\n{\"tool\": \"bad__name\"}\n[/tool_call]\nDone.",
			want:  "Here is my answer.\n\nDone.",
		},
		{
			name:  "strips multiple blocks",
			input: "[tool_call]block1[/tool_call] middle [tool_call]block2[/tool_call] end",
			want:  "middle  end",
		},
		{
			name:  "no blocks passthrough",
			input: "normal response text",
			want:  "normal response text",
		},
		{
			name:  "no closing tag drops tail",
			input: "before [tool_call]no closing",
			want:  "before",
		},
		{
			name:  "strips claude native function_calls xml",
			input: "Sure thing.\n<function_calls>\n<invoke name=\"scheduler.update_job\">\n<parameter name=\"name\">foo</parameter>\n</invoke>\n</function_calls>\nDone.",
			want:  "Sure thing.\n\nDone.",
		},
		{
			name:  "strips antml namespaced function_calls xml",
			input: "Let me check.\n<" + "antml:function_calls><" + "antml:invoke name=\"x.y\"/><" + "/antml:function_calls>\nok",
			want:  "Let me check.\n\nok",
		},
		{
			name:  "strips function_calls with no closing tag",
			input: "You're absolutely right! Let me update.\n<function_calls>\n<invoke",
			want:  "You're absolutely right! Let me update.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StripInternalBlocks(tc.input)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToStringMapNestedObject(t *testing.T) {
	// When the LLM emits a tool call whose args include a nested object
	// (e.g. scheduler's remind_me with args={"issue_id":"XYZ"}), the nested
	// map must be JSON-encoded so the downstream tool can json.Unmarshal it.
	// Go's default %v formatter would produce "map[issue_id:XYZ]" which fails
	// JSON parsing.
	m := map[string]interface{}{
		"args": map[string]interface{}{"issue_id": "XYZ"},
	}
	got := toStringMap(m)
	if got["args"] != `{"issue_id":"XYZ"}` {
		t.Errorf("nested object = %q, want valid JSON", got["args"])
	}
}

func TestToStringMapNestedArray(t *testing.T) {
	m := map[string]interface{}{
		"tags": []interface{}{"a", "b"},
	}
	got := toStringMap(m)
	if got["tags"] != `["a","b"]` {
		t.Errorf("nested array = %q, want JSON array", got["tags"])
	}
}

func TestParseToolName(t *testing.T) {
	tests := []struct {
		input      string
		wantPlugin string
		wantAction string
		wantErr    bool
	}{
		{"brave-search.search", "brave-search", "search", false},
		{"gitlab.analyze_code", "gitlab", "analyze_code", false},
		{"a.b.c", "a.b", "c", false},
		{"noaction", "", "", true},
		{".nodot", "", "", true},
		{"nodot.", "", "", true},
		// double-underscore format (OpenAI-style function names)
		{"jira__search_issues", "jira", "search_issues", false},
		{"appsignal__get_applications", "appsignal", "get_applications", false},
		{"brave_search__search", "brave_search", "search", false},
		// hyphenated MCP action names
		{"myapp.myapp__list-org-units", "myapp", "myapp__list-org-units", false},
		{"jira__get-issue-details", "jira", "get-issue-details", false},
		// dot is preferred over double-underscore when both could apply
		{"mcp.jira__search", "mcp", "jira__search", false},
		// natural-language fragments must be rejected
		{"` syntax). Let me know which action you'd like to perform!", "", "", true},
		{"plugin.action with spaces", "", "", true},
		{"plugin.action!", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p, a, err := parseToolName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if p != tt.wantPlugin || a != tt.wantAction {
				t.Errorf("got (%q, %q), want (%q, %q)", p, a, tt.wantPlugin, tt.wantAction)
			}
		})
	}
}

func TestParseBareJSONToolCall(t *testing.T) {
	// LLM emits a bare JSON tool call without [tool_call] tags.
	response := `{"tool": "myapp.myapp__list-org-units", "args": {}}`

	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "myapp" {
		t.Errorf("plugin = %q, want %q", calls[0].Plugin, "myapp")
	}
	if calls[0].Action != "myapp__list-org-units" {
		t.Errorf("action = %q, want %q", calls[0].Action, "myapp__list-org-units")
	}
}

func TestParseBareJSONToolCall_WithSurroundingText(t *testing.T) {
	// "Let me look that up." matches narrated intent detection, so
	// it correctly returns a narrated placeholder for retry.
	response := `Let me look that up.
{"tool": "myapp.search", "args": {"q": "test"}}`

	calls := DefaultParser.Parse(response)
	if !IsNarratedPlaceholder(calls) {
		t.Errorf("expected narrated placeholder for bare JSON with narration prefix, got %v", calls)
	}

	// Non-narrated text with bare JSON should NOT be parsed.
	response2 := `Here are some results:
{"tool": "myapp.search", "args": {"q": "test"}}`
	calls2 := DefaultParser.Parse(response2)
	if calls2 != nil {
		t.Errorf("expected nil for bare JSON with non-narrated text, got %v", calls2)
	}
}

func TestParseBareJSONToolCall_NoToolKey(t *testing.T) {
	// Bare JSON without a "tool" key should NOT be parsed.
	response := `{"name": "myapp.search", "args": {"q": "test"}}`

	calls := DefaultParser.Parse(response)
	if calls != nil {
		t.Errorf("expected nil for JSON without tool key, got %v", calls)
	}
}

func TestParseXMLFunctionCalls(t *testing.T) {
	response := `<function_calls>
<invoke name="timly.timly__list-person-types">
<parameter name="per_page">1</parameter>
</invoke>
</function_calls>`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "timly" {
		t.Errorf("plugin = %q, want timly", calls[0].Plugin)
	}
	if calls[0].Action != "timly__list-person-types" {
		t.Errorf("action = %q, want timly__list-person-types", calls[0].Action)
	}
	if calls[0].Args["per_page"] != "1" {
		t.Errorf("per_page = %q, want 1", calls[0].Args["per_page"])
	}
}

func TestParseXMLFunctionCalls_NoArgs(t *testing.T) {
	response := `<function_calls>
<invoke name="timly.timly__list-items"/>
</function_calls>`
	calls := DefaultParser.Parse(response)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Plugin != "timly" {
		t.Errorf("plugin = %q", calls[0].Plugin)
	}
	if calls[0].Action != "timly__list-items" {
		t.Errorf("action = %q", calls[0].Action)
	}
}

func TestParseNarratedToolCall(t *testing.T) {
	// Narrated tool calls return a placeholder — the orchestrator retries with "?".
	matches := []string{
		// Tool-name narration (narratedToolRe):
		"We need to call myplugin.myplugin__list-container-types.",
		"I'll call myplugin__list-items to check.",
		"Let me use myplugin.myplugin__show-person to look that up.",
		"We should call `myplugin.myplugin__list-items`.",
		"Call myplugin.myplugin__list-items and also call myplugin.myplugin__list-persons.",
		"I need to fetch myplugin__list-items to check.",
		"Let me query myplugin.myplugin__list-items.",
		// Tool-name narration with articles (narratedToolRe):
		"We need to call the list-items tool.",
		"call the list-items tool with per_page=1",
		// Intent narration without tool name (narratedIntentRe):
		"We need to call the appropriate tool.",
		"We'll call list-items with per_page=1 to get total count.",
		"We'll fetch the total count of items (including regular items, twins, and stock items).",
		"I'll check how many items you have.",
		"Let me look up your inventory.",
		"I'll get the item count for you.",
		"We'll search for all items in your account.",
		"I'll retrieve the data now.",
		"Let me find the matching records.",
		"I'll list all items.",
		"We'll count the items for you.",
		"I'm going to fetch the list.",
	}
	for _, input := range matches {
		calls := DefaultParser.Parse(input)
		if !IsNarratedPlaceholder(calls) {
			t.Errorf("expected narrated placeholder for %q, got %v", input, calls)
		}
	}
	// No match — should return nil.
	noMatch := []string{
		"Here are your results.",
		"You have 42 items in your account.",
		"The item was successfully assigned.",
	}
	for _, input := range noMatch {
		calls := DefaultParser.Parse(input)
		if calls != nil {
			t.Errorf("expected nil for non-narrated text %q, got %v", input, calls)
		}
	}
}

func TestHasHallucinatedResult(t *testing.T) {
	matches := []string{
		`You have **{{plugin_output.pagination.total}}** org-units.`,
		`There are {{result.items.count}} items in your account.`,
		`The total is {{data.pagination.total}}.`,
	}
	for _, input := range matches {
		if !hasHallucinatedResult(input) {
			t.Errorf("expected hallucinated result match for %q", input)
		}
	}
	noMatch := []string{
		"You have 42 items.",
		"Here are the results.",
		"Use {{ for templates in Go.",
	}
	for _, input := range noMatch {
		if hasHallucinatedResult(input) {
			t.Errorf("unexpected hallucinated result match for %q", input)
		}
	}
}
