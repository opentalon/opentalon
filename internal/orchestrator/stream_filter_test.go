package orchestrator

import "testing"

func TestStreamTagFilter_BasicBlock(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed(`Hello [tool_call]{"tool":"jira.search","args":{}}[/tool_call] world`)
	out += f.Flush()
	want := "Hello _jira → search…_\n world"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_SplitAcrossChunks(t *testing.T) {
	f := newStreamTagFilter(true)
	var out string
	out += f.Feed("Hello [tool")
	out += f.Feed(`_call]{"tool":"git.status","args":{}}[/tool_call]`)
	out += f.Flush()
	want := "Hello _git → status…_\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_InlineFormat(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed("[tool_call] brave-search.search\n{\"query\":\"test\"}\n[/tool_call]")
	out += f.Flush()
	want := "_brave-search → search…_\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_EmptyBlock(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed("[tool_call][/tool_call]")
	out += f.Flush()
	// Empty block body → no friendly label emitted.
	if out != "" {
		t.Errorf("got %q, want empty", out)
	}
}

func TestStreamTagFilter_NoBlock(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed("Just a normal response")
	out += f.Flush()
	if out != "Just a normal response" {
		t.Errorf("got %q", out)
	}
}

func TestStreamTagFilter_UnclosedBlock(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed("before [tool_call]no closing tag")
	out += f.Flush()
	// Unclosed block is dropped; text before is kept.
	if out != "before " {
		t.Errorf("got %q, want %q", out, "before ")
	}
}

func TestStreamTagFilter_MultipleBlocks(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed(`[tool_call]jira.search
{}
[/tool_call]then [tool_call]jira.create_issue
{}
[/tool_call]done`)
	out += f.Flush()
	want := "_jira → search…_\nthen _jira → create_issue…_\ndone"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_DoubleUnderscore(t *testing.T) {
	f := newStreamTagFilter(true)
	out := f.Feed(`[tool_call]jira__search_issues
{}
[/tool_call]`)
	out += f.Flush()
	want := "_jira → search_issues…_\n"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_HiddenMode(t *testing.T) {
	f := newStreamTagFilter(false)
	out := f.Feed(`Hello [tool_call]{"tool":"jira.search","args":{}}[/tool_call] world`)
	out += f.Flush()
	// Labels suppressed — only text outside blocks is emitted.
	want := "Hello  world"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestStreamTagFilter_NarratedToolCallSuppressed(t *testing.T) {
	f := newStreamTagFilter(false)
	out := f.Feed("We will call timly__list-containers with subcategory filter.")
	out += f.Flush()
	if out != "" {
		t.Errorf("narrated tool call should be suppressed, got %q", out)
	}
}

func TestStreamTagFilter_NarratedChunked(t *testing.T) {
	f := newStreamTagFilter(false)
	var out string
	out += f.Feed("We will ")
	out += f.Feed("call timly__list-containers")
	out += f.Feed(" with subcategory filter.")
	out += f.Flush()
	if out != "" {
		t.Errorf("chunked narrated tool call should be suppressed, got %q", out)
	}
}

func TestStreamTagFilter_NarratedFollowedByRealContent(t *testing.T) {
	f := newStreamTagFilter(false)
	var out string
	out += f.Feed("Let me use timly__show-container to look that up.\nHere are the details.")
	out += f.Flush()
	// The narrated sentence should be suppressed, the real content kept.
	if out != "\nHere are the details." {
		t.Errorf("expected real content only, got %q", out)
	}
}

func TestStreamTagFilter_NormalTextPassesThrough(t *testing.T) {
	f := newStreamTagFilter(false)
	out := f.Feed("The container has 5 items inside it.")
	out += f.Flush()
	if out != "The container has 5 items inside it." {
		t.Errorf("normal text should pass through, got %q", out)
	}
}

func TestToolCallFriendlyLabel(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"tool": "jira.search", "args": {}}`, "_jira → search…_\n"},
		{"brave-search.search\n{\"query\":\"test\"}", "_brave-search → search…_\n"},
		{"gitlab.analyze_code(repo=test)", "_gitlab → analyze_code…_\n"},
		{"jira__search_issues\n{}", "_jira → search_issues…_\n"},
		{"", ""},
		{"just some text", ""},
	}
	for _, tc := range tests {
		got := toolCallFriendlyLabel(tc.body)
		if got != tc.want {
			t.Errorf("toolCallFriendlyLabel(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
