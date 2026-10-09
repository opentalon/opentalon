package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func toolUse(content string, ids ...string) provider.Message {
	m := provider.Message{Role: provider.RoleAssistant, Content: content}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, provider.ToolCall{ID: id, Name: "agents__create"})
	}
	return m
}

func toolResult(id string) provider.Message {
	return provider.Message{Role: provider.RoleTool, Content: "ok", ToolCallID: id}
}

func user(s string) provider.Message { return provider.Message{Role: provider.RoleUser, Content: s} }

// assertPaired fails if msgs breaks the native tool-calling invariant.
func assertPaired(t *testing.T, msgs []provider.Message) {
	t.Helper()
	for i, m := range msgs {
		if m.Role == provider.RoleTool {
			j := i - 1
			for j >= 0 && msgs[j].Role == provider.RoleTool {
				j--
			}
			found := false
			if j >= 0 {
				for _, tc := range msgs[j].ToolCalls {
					found = found || tc.ID == m.ToolCallID
				}
			}
			if !found {
				t.Errorf("tool result %q at %d has no tool call right before it", m.ToolCallID, i)
			}
		}
		for _, tc := range m.ToolCalls {
			found := false
			for j := i + 1; j < len(msgs) && msgs[j].Role == provider.RoleTool; j++ {
				found = found || msgs[j].ToolCallID == tc.ID
			}
			if !found {
				t.Errorf("tool call %q at %d has no tool result right after it", tc.ID, i)
			}
		}
	}
}

func TestPairToolMessages(t *testing.T) {
	sys := provider.Message{Role: provider.RoleSystem, Content: "sys"}
	tests := []struct {
		name string
		in   []provider.Message
		want int // messages kept
	}{
		{"valid pair untouched", []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a")}, 4},
		// #311: the tool_use write failed, so the result directly follows the user turn.
		{"result without call dropped", []provider.Message{sys, user("u"), toolResult("a"), user("next")}, 3},
		{"call without result dropped", []provider.Message{sys, user("u"), toolUse("", "a"), user("next")}, 3},
		{"unanswered call keeps its text", []provider.Message{sys, user("u"), toolUse("let me check", "a"), user("next")}, 4},
		{"partially answered call", []provider.Message{sys, user("u"), toolUse("", "a", "b"), toolResult("b")}, 4},
		{"result for another call dropped", []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a"), toolResult("x")}, 4},
		{"duplicate result dropped", []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a"), toolResult("a")}, 4},
		{"leading orphan after trim", []provider.Message{sys, toolResult("a"), user("u")}, 2},
		{"text-format results untouched", []provider.Message{sys, user("[plugin_output] x"), user("u")}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pairToolMessages(context.Background(), tt.in)
			if len(got) != tt.want {
				t.Errorf("kept %d messages, want %d: %+v", len(got), tt.want, got)
			}
			assertPaired(t, got)
		})
	}
}

func TestPairToolMessages_StripsOnlyTheUnansweredCall(t *testing.T) {
	got := pairToolMessages(context.Background(), []provider.Message{user("u"), toolUse("", "a", "b"), toolResult("b")})
	if calls := got[1].ToolCalls; len(calls) != 1 || calls[0].ID != "b" {
		t.Errorf("tool calls = %+v, want only b", calls)
	}
	got = pairToolMessages(context.Background(), []provider.Message{user("u"), toolUse("let me check", "a"), user("next")})
	if got[1].ToolCalls != nil || got[1].Content != "let me check" {
		t.Errorf("assistant = %+v, want its text without tool calls", got[1])
	}
}

func TestSummaryCut_NeverSplitsAToolPair(t *testing.T) {
	msgs := []provider.Message{user("u1"), toolUse("", "a"), toolResult("a"), user("u2"), toolUse("", "b"), toolResult("b")}
	tests := []struct{ keep, want int }{
		{keep: 1, want: 4}, // would keep only result b → keep its call too
		{keep: 2, want: 4},
		{keep: 3, want: 3},
		{keep: 4, want: 1}, // would start at result a → back to its call
		{keep: 6, want: 0},
	}
	for _, tt := range tests {
		if got := summaryCut(msgs, tt.keep); got != tt.want {
			t.Errorf("summaryCut(keep=%d) = %d, want %d", tt.keep, got, tt.want)
		}
		assertPaired(t, msgs[summaryCut(msgs, tt.keep):])
	}
}

func TestFitRequestToWindow_KeepsTheCallOfALoneFinalResult(t *testing.T) {
	big := toolResult("a")
	big.Content = strings.Repeat("r", 4000)
	req := &provider.CompletionRequest{Messages: []provider.Message{
		{Role: provider.RoleSystem, Content: block("s")},
		user(block("u")),
		toolUse("", "a"),
		big,
	}}
	fit(t, req, 600, 0, 1.0)
	if len(req.Messages) != 3 || len(req.Messages[1].ToolCalls) == 0 {
		t.Fatalf("want system + tool call + result, got %d messages", len(req.Messages))
	}
	assertPaired(t, req.Messages)
}

// pairCheckingLLM records every request and answers in plain text.
type pairCheckingLLM struct{ requests [][]provider.Message }

func (l *pairCheckingLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.requests = append(l.requests, append([]provider.Message(nil), req.Messages...))
	return &provider.CompletionResponse{Content: "done"}, nil
}

func (l *pairCheckingLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// #311: a session whose history lost one half of a tool pair (a failed write of
// the assistant tool_use row) must still produce a request the provider accepts.
func TestRun_UnpairedHistoryIsRepairedBeforeSending(t *testing.T) {
	llm := &pairCheckingLLM{}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "sess"})
	_ = sessions.AddMessage("sess", user("create an agent"))
	_ = sessions.AddMessage("sess", toolResult("toolu_lost")) // its tool_use row was never written
	_ = sessions.AddMessage("sess", toolUse("", "toolu_unanswered"))
	_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleAssistant, Content: "Agent created."})
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})

	if _, err := orch.Run(context.Background(), "sess", "thanks"); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) == 0 {
		t.Fatal("no LLM request was made")
	}
	for _, req := range llm.requests {
		assertPaired(t, req)
	}
}
