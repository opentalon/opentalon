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
