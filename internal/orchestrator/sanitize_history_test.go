package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// Essential requirement (the contract these tests pin down):
//
// A session's conversation history must reach the model intact. sanitizeHistory
// may strip ONLY genuinely poisoned assistant turns — a malformed [tool_call]
// block, or a fabricated {{template}} result placeholder. It must NEVER drop a
// user message, must keep legitimate text answers and clarifying questions, and
// must keep assistant turns that carry a native tool call. Context-window
// trimming is a separate, oldest-first concern (trimToContextWindow); nothing
// else may delete history. Losing a user's earlier request is what breaks
// multi-turn tasks — see the #162 regression that this file guards against.

func userContents(msgs []provider.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			out = append(out, m.Content)
		}
	}
	return out
}

// TestSanitizeHistory_KeepsClarificationDialogue is the direct regression guard:
// a create-item task carried across several clarifying turns. None of it is
// poison, so every message — the user's requests AND the assistant's questions —
// must survive unchanged.
func TestSanitizeHistory_KeepsClarificationDialogue(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Erstell mal schnell ein neues test item"},
		{Role: provider.RoleAssistant, Content: "Sure! To create the test item I'll need a category."},
		{Role: provider.RoleUser, Content: "egal"},
		{Role: provider.RoleAssistant, Content: "Got it. Which name should it have?"},
		{Role: provider.RoleUser, Content: "Nimm irgendwelche die wir im account haben"},
		{Role: provider.RoleAssistant, Content: "I can use a default name like Test-Item."},
		{Role: provider.RoleUser, Content: "egal irgendwas"},
	}
	got := sanitizeHistory(msgs)
	if len(got) != len(msgs) {
		t.Fatalf("clarification dialogue must survive intact: got %d of %d messages", len(got), len(msgs))
	}
	for i := range msgs {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content {
			t.Errorf("message %d changed: got [%s] %q, want [%s] %q",
				i, got[i].Role, got[i].Content, msgs[i].Role, msgs[i].Content)
		}
	}
}

// TestSanitizeHistory_NeverDropsUserMessages: even when poison is present and
// stripped, every user message survives in order.
func TestSanitizeHistory_NeverDropsUserMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many org units?"},
		{Role: provider.RoleAssistant, Content: "You have **{{plugin_output.pagination.total}}** org-units."}, // template poison
		{Role: provider.RoleUser, Content: "that's wrong"},
		{Role: provider.RoleAssistant, Content: "[tool_call] ."}, // broken tool-call poison
		{Role: provider.RoleUser, Content: "call the tool please"},
	}
	got := sanitizeHistory(msgs)
	want := []string{"how many org units?", "that's wrong", "call the tool please"}
	gotUsers := userContents(got)
	if len(gotUsers) != len(want) {
		t.Fatalf("user messages must never be dropped: got %v, want %v", gotUsers, want)
	}
	for i := range want {
		if gotUsers[i] != want[i] {
			t.Errorf("user message %d = %q, want %q", i, gotUsers[i], want[i])
		}
	}
}

// TestSanitizeHistory_StripsHallucinatedTemplate: the {{template}} assistant turn
// is removed, but BOTH surrounding user messages are kept.
func TestSanitizeHistory_StripsHallucinatedTemplate(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many org units?"},
		{Role: provider.RoleAssistant, Content: "You have **{{plugin_output.pagination.total}}** org-units."},
		{Role: provider.RoleUser, Content: "that's wrong"},
	}
	got := sanitizeHistory(msgs)
	want := []string{"how many org units?", "that's wrong"}
	if len(got) != 2 || got[0].Content != want[0] || got[1].Content != want[1] {
		t.Errorf("template assistant must be stripped, both users kept: got %v", got)
	}
}

// TestSanitizeHistory_StripsBrokenToolCall: a malformed [tool_call] turn is
// removed; the user messages around it are kept.
func TestSanitizeHistory_StripsBrokenToolCall(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "list items"},
		{Role: provider.RoleAssistant, Content: "[tool_call] ."},
		{Role: provider.RoleUser, Content: "?"},
	}
	got := sanitizeHistory(msgs)
	if len(got) != 2 || got[0].Content != "list items" || got[1].Content != "?" {
		t.Errorf("broken tool-call must be stripped, users kept: got %v", got)
	}
}

// TestSanitizeHistory_KeepsFabricatedTextAnswer: a plain (even if factually
// wrong) text answer with NO template/broken-tool-call poison is kept now —
// conversation integrity beats over-eager scrubbing. Native tool calling plus
// generation-time template detection handle the real fabrication risk; erasing
// the turn (and the user's question) is the worse failure.
func TestSanitizeHistory_KeepsFabricatedTextAnswer(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many items?"},
		{Role: provider.RoleAssistant, Content: "You have 0 items."},
		{Role: provider.RoleUser, Content: "lies!"},
	}
	got := sanitizeHistory(msgs)
	if len(got) != 3 {
		t.Errorf("plain text answers (even if wrong) are kept: got %d of 3: %v", len(got), got)
	}
}

// TestSanitizeHistory_KeepsNativeToolCallTurn: an assistant turn with a native
// tool call (ToolCalls set, empty content) must always survive — dropping it
// would orphan the following tool result and break native function-calling.
func TestSanitizeHistory_KeepsNativeToolCallTurn(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "delete item 5"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "inventory.delete-item"}}},
		{Role: provider.RoleTool, Content: "Deleted item 5.", ToolCallID: "c1"},
		{Role: provider.RoleAssistant, Content: "Done — item 5 deleted."},
	}
	got := sanitizeHistory(msgs)
	if len(got) != len(msgs) {
		t.Errorf("native tool-call turn + result must be preserved: got %d of %d", len(got), len(msgs))
	}
}

// TestSanitizeHistory_KeepsValidToolCallAndResults: text-format tool call + its
// plugin_output + the summary all survive.
func TestSanitizeHistory_KeepsValidToolCallAndResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many items?"},
		{Role: provider.RoleAssistant, Content: "[tool_call]{\"tool\":\"inventory.list-items\"}[/tool_call]"},
		{Role: provider.RoleUser, Content: "[plugin_output]{\"pagination\":{\"total\":370}}[/plugin_output]"},
		{Role: provider.RoleAssistant, Content: "You have 370 items."},
	}
	got := sanitizeHistory(msgs)
	if len(got) != len(msgs) {
		t.Errorf("expected all %d messages preserved, got %d", len(msgs), len(got))
	}
}

func TestSanitizeHistory_EmptyHistory(t *testing.T) {
	if got := sanitizeHistory(nil); len(got) != 0 {
		t.Errorf("expected 0 messages, got %d", len(got))
	}
}
