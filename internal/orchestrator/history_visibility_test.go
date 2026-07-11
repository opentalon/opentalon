package orchestrator

import (
	"strings"
	"testing"

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
