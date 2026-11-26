package orchestrator

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
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

func toolResultWith(id, content string) provider.Message {
	return provider.Message{Role: provider.RoleTool, Content: content, ToolCallID: id}
}

func unrecorded(id string) provider.Message {
	return provider.Message{Role: provider.RoleTool, Content: unrecordedToolResultNotice, ToolCallID: id}
}

func user(s string) provider.Message { return provider.Message{Role: provider.RoleUser, Content: s} }

func assistant(s string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: s}
}

// assertPaired fails if msgs breaks the native tool-calling invariant: every
// call has a non-empty id unique within its message and is answered right
// after its message, and every result answers a call right before it.
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
					found = found || (tc.ID != "" && tc.ID == m.ToolCallID)
				}
			}
			if !found {
				t.Errorf("tool result %q at %d has no tool call right before it", m.ToolCallID, i)
			}
		}
		seen := map[string]bool{}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || seen[tc.ID] {
				t.Errorf("tool call at %d has an empty or repeated id %q", i, tc.ID)
			}
			seen[tc.ID] = true
			answers := 0
			for j := i + 1; j < len(msgs) && msgs[j].Role == provider.RoleTool; j++ {
				if msgs[j].ToolCallID == tc.ID {
					answers++
				}
			}
			if answers != 1 {
				t.Errorf("tool call %q at %d has %d results right after it, want 1", tc.ID, i, answers)
			}
		}
	}
}

func TestPairToolMessages(t *testing.T) {
	sys := provider.Message{Role: provider.RoleSystem, Content: "sys"}
	thinkingCall := toolUse("", "a")
	thinkingCall.Thinking = []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"","signature":"sig-1"}`)}
	tests := []struct {
		name   string
		in     []provider.Message
		want   []provider.Message
		report toolPairingReport
	}{
		{
			name: "valid pairs kept, results in call order",
			in:   []provider.Message{sys, user("u"), toolUse("", "a", "b"), toolResult("b"), toolResult("a"), assistant("done")},
			want: []provider.Message{sys, user("u"), toolUse("", "a", "b"), toolResult("a"), toolResult("b"), assistant("done")},
		},
		{
			// #311: the tool_use write failed, so the result directly follows the user turn.
			name:   "result without call dropped",
			in:     []provider.Message{sys, user("u"), toolResult("a"), user("next")},
			want:   []provider.Message{sys, user("u"), user("next")},
			report: toolPairingReport{OrphanResultIDs: []string{"a"}},
		},
		{
			// The result write failed: the call may have run, so it stays, answered.
			name:   "call without result answered as unrecorded",
			in:     []provider.Message{sys, user("u"), toolUse("", "a"), user("next")},
			want:   []provider.Message{sys, user("u"), toolUse("", "a"), unrecorded("a"), user("next")},
			report: toolPairingReport{UnansweredCallIDs: []string{"a"}, UnansweredTools: []string{"agents__create"}},
		},
		{
			name:   "partially answered call",
			in:     []provider.Message{sys, user("u"), toolUse("checking", "a", "b"), toolResult("b")},
			want:   []provider.Message{sys, user("u"), toolUse("checking", "a", "b"), unrecorded("a"), toolResult("b")},
			report: toolPairingReport{UnansweredCallIDs: []string{"a"}, UnansweredTools: []string{"agents__create"}},
		},
		{
			// A notification appended between the two halves of a pair.
			name:   "result separated from its call moved back",
			in:     []provider.Message{sys, user("u"), toolUse("", "a"), assistant("note"), toolResult("a"), assistant("done")},
			want:   []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a"), assistant("note"), assistant("done")},
			report: toolPairingReport{MovedResultIDs: []string{"a"}},
		},
		{
			name:   "thinking stays on a moved call message",
			in:     []provider.Message{sys, toolResult("x"), user("u"), thinkingCall, assistant("note"), toolResult("a")},
			want:   []provider.Message{sys, user("u"), thinkingCall, toolResult("a"), assistant("note")},
			report: toolPairingReport{MovedResultIDs: []string{"a"}, OrphanResultIDs: []string{"x"}},
		},
		{
			// Call ids repeat across rounds: the second call-1's result must not
			// be taken by the first, unanswered call-1.
			name: "result of a later call with the same id stays with it",
			in: []provider.Message{sys, user("u"), toolUse("", "call-1"), user("hidden note"),
				toolUse("", "call-1"), toolResultWith("call-1", "ROUND-2 RESULT")},
			want: []provider.Message{sys, user("u"), toolUse("", "call-1"), unrecorded("call-1"), user("hidden note"),
				toolUse("", "call-1"), toolResultWith("call-1", "ROUND-2 RESULT")},
			report: toolPairingReport{UnansweredCallIDs: []string{"call-1"}, UnansweredTools: []string{"agents__create"}},
		},
		{
			// A user turn ends the exchange: a result after it is not the call's.
			name: "result past a user turn is not moved back",
			in:   []provider.Message{sys, user("u"), toolUse("", "call-1"), user("u2"), toolResultWith("call-1", "RESULT OF CALL B")},
			want: []provider.Message{sys, user("u"), toolUse("", "call-1"), unrecorded("call-1"), user("u2")},
			report: toolPairingReport{UnansweredCallIDs: []string{"call-1"}, UnansweredTools: []string{"agents__create"},
				OrphanResultIDs: []string{"call-1"}},
		},
		{
			name:   "result for another call dropped",
			in:     []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a"), toolResult("x")},
			want:   []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a")},
			report: toolPairingReport{OrphanResultIDs: []string{"x"}},
		},
		{
			name:   "duplicate result dropped",
			in:     []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a"), toolResult("a")},
			want:   []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a")},
			report: toolPairingReport{OrphanResultIDs: []string{"a"}},
		},
		{
			name:   "leading orphan after a trim",
			in:     []provider.Message{sys, toolResult("a"), user("u")},
			want:   []provider.Message{sys, user("u")},
			report: toolPairingReport{OrphanResultIDs: []string{"a"}},
		},
		{
			// openai.go drops a result with an empty id, which would leave the call unanswered.
			name:   "empty id call and result dropped",
			in:     []provider.Message{sys, user("u"), toolUse("", ""), toolResult(""), user("next")},
			want:   []provider.Message{sys, user("u"), user("next")},
			report: toolPairingReport{OrphanResultIDs: []string{""}, DroppedCallCount: 1},
		},
		{
			name:   "empty id call keeps its text",
			in:     []provider.Message{sys, user("u"), toolUse("let me check", "")},
			want:   []provider.Message{sys, user("u"), assistant("let me check")},
			report: toolPairingReport{DroppedCallCount: 1},
		},
		{
			name:   "repeated call id dropped",
			in:     []provider.Message{sys, user("u"), toolUse("", "a", "a"), toolResult("a")},
			want:   []provider.Message{sys, user("u"), toolUse("", "a"), toolResult("a")},
			report: toolPairingReport{DroppedCallCount: 1},
		},
		{
			name: "text-format results untouched",
			in:   []provider.Message{sys, user("[plugin_output] x"), assistant("[tool_call] y"), user("u")},
			want: []provider.Message{sys, user("[plugin_output] x"), assistant("[tool_call] y"), user("u")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, report := pairToolMessages(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("messages:\n got %+v\nwant %+v", got, tt.want)
			}
			if !reflect.DeepEqual(report, tt.report) {
				t.Errorf("report = %+v, want %+v", report, tt.report)
			}
			if report.empty() != reflect.DeepEqual(tt.report, toolPairingReport{}) {
				t.Errorf("report.empty() = %v", report.empty())
			}
			assertPaired(t, got)
		})
	}
}

// scriptedPairLLM records every request and plays its responses in order,
// then answers in plain text.
type scriptedPairLLM struct {
	responses []*provider.CompletionResponse
	requests  [][]provider.Message
}

func (l *scriptedPairLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.requests = append(l.requests, append([]provider.Message(nil), req.Messages...))
	if i := len(l.requests) - 1; i < len(l.responses) {
		return l.responses[i], nil
	}
	return &provider.CompletionResponse{Content: "done"}, nil
}

func (l *scriptedPairLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// #311: a session whose history lost one half of each of two tool pairs must
// still produce requests the provider accepts, and the damage is reported once
// per turn, not once per LLM round.
func TestRun_UnpairedHistoryIsRepairedBeforeSending(t *testing.T) {
	llm := &scriptedPairLLM{responses: []*provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{{ID: "call-now", Name: "inv__list"}}},
	}}
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{Name: "inv", Actions: []Action{{Name: "list", AlwaysInclude: true, ReadOnly: true}}}, &echoExecutor{})
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "sess"})
	_ = sessions.AddMessage("sess", user("create an agent"))
	_ = sessions.AddMessage("sess", toolResult("toolu_lost")) // its tool_use row was never written
	_ = sessions.AddMessage("sess", toolUse("", "toolu_unanswered"))
	_ = sessions.AddMessage("sess", assistant("Agent created."))
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{EventSink: sink})

	if _, err := orch.Run(context.Background(), "sess", "list my items"); err != nil {
		t.Fatal(err)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("LLM requests = %d, want 2 (tool round + answer)", len(llm.requests))
	}
	for _, req := range llm.requests {
		assertPaired(t, req)
		found := false
		for _, m := range req {
			found = found || (m.ToolCallID == "toolu_unanswered" && m.Content == unrecordedToolResultNotice)
		}
		if !found {
			t.Error("unanswered call was not answered with the unrecorded-result notice")
		}
	}
	if n := countEventType(sink.snapshot(), events.TypeToolMessagesRepaired); n != 1 {
		t.Errorf("tool_messages_repaired events = %d, want 1 per turn", n)
	}
}

func TestPairingReported_ReportsEachRepairOncePerTurn(t *testing.T) {
	var seen pairingReported
	first := toolPairingReport{UnansweredCallIDs: []string{"a"}, UnansweredTools: []string{"t"}, OrphanResultIDs: []string{"x"}, DroppedCallCount: 1}
	if got := seen.fresh(first); !reflect.DeepEqual(got, first) {
		t.Errorf("first report = %+v, want all of it", got)
	}
	if got := seen.fresh(first); !got.empty() {
		t.Errorf("same damage again = %+v, want nothing new", got)
	}
	// A later round finds more damage on top of the same: only the new part.
	second := toolPairingReport{
		UnansweredCallIDs: []string{"a", "b"}, UnansweredTools: []string{"t", "u"},
		MovedResultIDs: []string{"m"}, OrphanResultIDs: []string{"x"}, DroppedCallCount: 3,
	}
	want := toolPairingReport{UnansweredCallIDs: []string{"b"}, UnansweredTools: []string{"u"}, MovedResultIDs: []string{"m"}, DroppedCallCount: 2}
	if got := seen.fresh(second); !reflect.DeepEqual(got, want) {
		t.Errorf("second report = %+v, want %+v", got, want)
	}
}

// Call ids repeat across rounds: a second unanswered "call-1", for another tool
// or the same one, is new damage and must be reported.
func TestPairingReported_RepeatedIDsAreNewDamage(t *testing.T) {
	var seen pairingReported
	seen.fresh(toolPairingReport{UnansweredCallIDs: []string{"call-1"}, UnansweredTools: []string{"a__x"}, OrphanResultIDs: []string{"call-1"}})

	got := seen.fresh(toolPairingReport{
		UnansweredCallIDs: []string{"call-1", "call-1"}, UnansweredTools: []string{"a__x", "b__y"},
		OrphanResultIDs: []string{"call-1", "call-1"},
	})
	want := toolPairingReport{UnansweredCallIDs: []string{"call-1"}, UnansweredTools: []string{"b__y"}, OrphanResultIDs: []string{"call-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fresh = %+v, want %+v", got, want)
	}

	got = seen.fresh(toolPairingReport{UnansweredCallIDs: []string{"call-1", "call-1", "call-1"}, UnansweredTools: []string{"a__x", "b__y", "a__x"}})
	want = toolPairingReport{UnansweredCallIDs: []string{"call-1"}, UnansweredTools: []string{"a__x"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("same tool, same id again = %+v, want %+v", got, want)
	}
}
