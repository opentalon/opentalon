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
	if calls != nil {
		t.Errorf("expected nil for invalid tool name, got %v", calls)
	}
}

func TestParseEmptyBody(t *testing.T) {
	response := `[tool_call][/tool_call]`
	calls := DefaultParser.Parse(response)
	if calls != nil {
		t.Errorf("expected nil for empty body, got %v", calls)
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
