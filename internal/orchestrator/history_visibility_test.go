package orchestrator

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// isVisibleUserMessage is the single predicate every "what did the user say"
// derivation routes through, so a hidden system-injected note (stored as
// role=user but model-only) can never be mistaken for the user's own words.
func TestIsVisibleUserMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  provider.Message
		want bool
	}{
		{"visible user", provider.Message{Role: provider.RoleUser, Content: "hi"}, true},
		{"hidden user (system note)", provider.Message{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[system] done"}, false},
		{"assistant", provider.Message{Role: provider.RoleAssistant, Content: "hello"}, false},
		{"tool result", provider.Message{Role: provider.RoleTool, Content: "result"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVisibleUserMessage(tc.msg); got != tc.want {
				t.Errorf("isVisibleUserMessage(%+v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// lastUserMessage must return the most recent VISIBLE user message, skipping a
// hidden system-injected note that sits after the real user turn — otherwise
// the RAG search enrichment and reply-language fallback inherit the note.
func TestLastUserMessage_SkipsHiddenNote(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "create a ticket for the broken drill"},
		{Role: provider.RoleAssistant, Content: "working on it"},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[system] job finished"},
	}
	if got := lastUserMessage(msgs); got != "create a ticket for the broken drill" {
		t.Errorf("lastUserMessage = %q, want the visible user turn (hidden note must be skipped)", got)
	}
}

// buildPlannerConversationContext must exclude hidden system-injected notes so
// the planner never treats a machine note as the user's intent — matching its
// own contract ("no ... system messages").
func TestBuildPlannerConversationContext_ExcludesHiddenNote(t *testing.T) {
	sess := &state.Session{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "list all defective devices"},
		{Role: provider.RoleAssistant, Content: "here they are"},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden,
			Content: "[system] SECRET job note that must not leak into planner context"},
	}}
	got := buildPlannerConversationContext(sess)
	if strings.Contains(got, "SECRET job note") {
		t.Errorf("planner context must exclude the hidden system note, got:\n%s", got)
	}
	if !strings.Contains(got, "User: list all defective devices") {
		t.Errorf("planner context should include the visible user turn, got:\n%s", got)
	}
}

// promptRecordingLLM is fakeLLM that keeps the last message of every request.
type promptRecordingLLM struct {
	fakeLLM
	prompts []string
}

func (l *promptRecordingLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.prompts = append(l.prompts, req.Messages[len(req.Messages)-1].Content)
	return l.fakeLLM.Complete(ctx, req)
}

// The session title describes what the user wrote: a hidden turn the assistant
// answered (the context a server opened the session with, a job note) leaves
// the session untitled, and the user's first visible message is the source.
func TestMaybeGenerateTitle_IgnoresHiddenTurns(t *testing.T) {
	opened := []provider.Message{
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[context] the setup created three samples"},
		{Role: provider.RoleAssistant, Content: "Your account is ready. Shall I show you the next tour?"},
	}
	replied := append(slices.Clone(opened),
		provider.Message{Role: provider.RoleUser, Content: "Yes, show me how tickets work"},
		provider.Message{Role: provider.RoleAssistant, Content: "Here is how tickets work."},
	)
	for _, tc := range []struct {
		name    string
		msgs    []provider.Message
		prompts []string
	}{
		{"hidden turn only", opened, nil},
		{"after the user's reply", replied, []string{"Yes, show me how tickets work"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "sess"})
			for _, m := range tc.msgs {
				_ = sessions.AddMessage("sess", m)
			}
			llm := &promptRecordingLLM{fakeLLM: fakeLLM{responses: []string{"Tickets tour"}}}
			orch := NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions,
				OrchestratorOpts{SessionTitlesEnabled: true})

			orch.maybeGenerateTitle(context.Background(), "sess")

			if !slices.Equal(llm.prompts, tc.prompts) {
				t.Errorf("title prompts = %q, want %q", llm.prompts, tc.prompts)
			}
		})
	}
}

// A keyed note's user message is stored with its key (WithNoteKey), which marks
// the key's reservation stored: a stale reservation can then never be taken
// again to run the note twice. An unkeyed turn leaves the reservation open.
func TestRun_StoresTheNoteKeyWithTheUserMessage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		key         string
		reclaimable bool
	}{
		{"keyed note", "chapter-2", false},
		{"unkeyed turn", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			orch := NewWithRules(&fakeLLM{responses: []string{"Noted."}},
				&fakeParser{parseFn: func(string) []ToolCall { return nil }},
				NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			now := time.Now()
			if ok, err := sessions.ReserveNote("s1", "chapter-2", now, time.Minute); err != nil || !ok {
				t.Fatalf("ReserveNote = %v, %v", ok, err)
			}

			ctx := WithNoteKey(actor.WithVisibility(context.Background(), provider.VisibilityHidden), tc.key)
			if _, err := orch.Run(ctx, "s1", "[context] chapter two was caught up"); err != nil {
				t.Fatalf("Run: %v", err)
			}

			again, err := sessions.ReserveNote("s1", "chapter-2", now.Add(time.Hour), time.Minute)
			if err != nil || again != tc.reclaimable {
				t.Errorf("stale reservation taken again = %v (%v), want %v", again, err, tc.reclaimable)
			}
		})
	}
}
