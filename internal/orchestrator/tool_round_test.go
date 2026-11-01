package orchestrator

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// rowShapes renders stored messages as "role text call-ids" lines, the way the
// tests below compare a session's history; a tool result shows its call id.
func rowShapes(msgs []provider.Message) []string {
	shapes := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts := []string{string(m.Role)}
		if m.Content != "" && m.Role != provider.RoleTool {
			parts = append(parts, m.Content)
		}
		if m.ToolCallID != "" {
			parts = append(parts, m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			ids := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				ids = append(ids, tc.ID)
			}
			parts = append(parts, "["+strings.Join(ids, " ")+"]")
		}
		shapes = append(shapes, strings.Join(parts, " "))
	}
	return shapes
}

func countContent(msgs []provider.Message, content string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == provider.RoleAssistant && m.Content == content {
			n++
		}
	}
	return n
}

// toolRoundOrch builds an orchestrator over a read tool and a write tool that
// needs approval, driven by a scripted native model over the given sessions.
func toolRoundOrch(t *testing.T, sessions *state.SessionStore, rounds ...provider.CompletionResponse) (*Orchestrator, *scriptedNativeLLM) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "tool round fixtures",
		Actions: []Action{
			{Name: "show", Description: "Show an item.", ReadOnly: true, AlwaysInclude: true},
			{Name: "list", Description: "List the items.", ReadOnly: true, AlwaysInclude: true},
			{Name: "delete", Description: "Delete an item.", AlwaysInclude: true},
		},
	}, &countingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	llm := &scriptedNativeLLM{rounds: rounds}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{
			ConfirmationPlugin:  "conf",
			ConfirmationAction:  "check",
			InjectionStateStore: &fakeInjectionStateStore{},
		})
	return orch, llm
}

// A response with text and two tool calls is stored as the tool-calling
// protocol has it: one assistant message with the text and both calls, then
// one result per call, in order. The text stands in the history and in the
// next request once, not once per call.
func TestRun_ResponseTextIsStoredOnceWithAllItsToolCalls(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch, llm := toolRoundOrch(t, sessions,
		provider.CompletionResponse{Content: "Looking both up.", ToolCalls: []provider.ToolCall{
			{ID: "c-show", Name: "p__show", Arguments: map[string]string{}},
			{ID: "c-list", Name: "p__list", Arguments: map[string]string{}},
		}},
		provider.CompletionResponse{Content: "Done."},
	)

	if _, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "show and list"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	want := []string{
		"user show and list",
		"assistant Looking both up. [c-show c-list]",
		"tool c-show",
		"tool c-list",
		"assistant Done.",
	}
	if got := rowShapes(s.Messages); !reflect.DeepEqual(got, want) {
		t.Errorf("stored rows:\n got %q\nwant %q", got, want)
	}
	if len(llm.requests) < 2 {
		t.Fatalf("want a second request after the tool round, got %d", len(llm.requests))
	}
	if n := countContent(llm.requests[1].Messages, "Looking both up."); n != 1 {
		t.Errorf("the next request carries the response's text %d times, want once", n)
	}
}

// A confirmation in the middle of a response: the calls that ran before it
// keep the text on their message, the gated call is stored once approved —
// without the text, which the transcript has shown by then.
func TestRun_ResponseTextStaysWithTheCallsThatRanBeforeAConfirmation(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	ctx := actor.WithSessionID(context.Background(), "s1")
	first, _ := toolRoundOrch(t, sessions,
		provider.CompletionResponse{Content: "Showing it, then deleting it.", ToolCalls: []provider.ToolCall{
			{ID: "c-show", Name: "p__show", Arguments: map[string]string{}},
			{ID: "c-del", Name: "p__delete", Arguments: map[string]string{}},
		}},
		provider.CompletionResponse{Content: "Delete it?"}, // the confirmation's narration
	)
	if _, err := first.Run(ctx, "s1", "show 7, then delete it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	want := []string{
		"user show 7, then delete it",
		"assistant Showing it, then deleting it. [c-show]",
		"tool c-show",
		"assistant Delete it?",
	}
	if got := rowShapes(s.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows before the approval:\n got %q\nwant %q", got, want)
	}

	second, _ := toolRoundOrch(t, sessions, provider.CompletionResponse{Content: "Deleted."})
	if _, err := second.Run(actor.WithConfirmationDecision(ctx, "approve"), "s1", "approve"); err != nil {
		t.Fatalf("approval Run: %v", err)
	}
	s, err = sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	want = append(want, "user approve", "assistant [c-del]", "tool c-del", "assistant Deleted.")
	if got := rowShapes(s.Messages); !reflect.DeepEqual(got, want) {
		t.Errorf("rows after the approval:\n got %q\nwant %q", got, want)
	}
	if n := countContent(s.Messages, "Showing it, then deleting it."); n != 1 {
		t.Errorf("the response's text is stored %d times, want once", n)
	}
}

// A confirmation on the first call of a response: the calls it interrupts
// share one message, which takes the text, and their NOT EXECUTED results
// follow it.
func TestRun_ResponseTextGoesToTheCallsAConfirmationInterrupts(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch, _ := toolRoundOrch(t, sessions,
		provider.CompletionResponse{Content: "Deleting, then listing.", ToolCalls: []provider.ToolCall{
			{ID: "c-del", Name: "p__delete", Arguments: map[string]string{}},
			{ID: "c-show", Name: "p__show", Arguments: map[string]string{}},
			{ID: "c-list", Name: "p__list", Arguments: map[string]string{}},
		}},
		provider.CompletionResponse{Content: "Delete it?"},
	)
	if _, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "delete 7, then show and list"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	want := []string{
		"user delete 7, then show and list",
		"assistant Deleting, then listing. [c-show c-list]",
		"tool c-show",
		"tool c-list",
		"assistant Delete it?",
	}
	if got := rowShapes(s.Messages); !reflect.DeepEqual(got, want) {
		t.Errorf("stored rows:\n got %q\nwant %q", got, want)
	}
	for _, m := range s.Messages {
		if m.Role == provider.RoleTool && !strings.Contains(m.Content, "NOT EXECUTED") {
			t.Errorf("interrupted call %s must be answered NOT EXECUTED, got %q", m.ToolCallID, m.Content)
		}
	}
}

// The sliding window keeps a tool round whole: a cut that would land among the
// round's results moves back to the message with the calls, and a round that
// is all the window has left stays as it is. Plain turns cut as before.
func TestApplySlidingWindow_KeepsAToolRoundWhole(t *testing.T) {
	round := []provider.Message{
		{Role: provider.RoleAssistant, Content: "Looking both up.", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "p__show"}, {ID: "c2", Name: "p__list"}}},
		{Role: provider.RoleTool, Content: "one", ToolCallID: "c1"},
		{Role: provider.RoleTool, Content: "two", ToolCallID: "c2"},
	}
	talk := []provider.Message{
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, Content: "hello"},
		{Role: provider.RoleUser, Content: "show and list"},
	}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, NewToolRegistry(), state.NewMemoryStore(""),
		state.NewSessionStore(""), OrchestratorOpts{ContextMessages: 2})
	ctx := context.Background()

	cases := []struct {
		name string
		msgs []provider.Message
		want []string
	}{
		{"a cut among the results moves back to the calls", append(append([]provider.Message{}, talk...), round...),
			[]string{"assistant Looking both up. [c1 c2]", "tool c1", "tool c2"}},
		{"a round that is all there is stays", round,
			[]string{"assistant Looking both up. [c1 c2]", "tool c1", "tool c2"}},
		{"plain turns keep the last two", talk, []string{"assistant hello", "user show and list"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowShapes(orch.applySlidingWindow(ctx, tc.msgs)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("kept rows:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
