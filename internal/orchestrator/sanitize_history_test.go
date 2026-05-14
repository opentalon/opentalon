package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func TestSanitizeHistory_RemovesHallucinatedText(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many org units?"},
		{Role: provider.RoleAssistant, Content: "You have **{{plugin_output.pagination.total}}** org-units."},
		{Role: provider.RoleUser, Content: "that's wrong"},
	}
	got := sanitizeHistory(msgs)
	if len(got) != 1 || got[0].Content != "that's wrong" {
		t.Errorf("expected 1 message, got %d: %v", len(got), got)
	}
}

func TestSanitizeHistory_RemovesNarratedAndFabricatedAnswers(t *testing.T) {
	// Simulates a poisoned session where the LLM keeps answering without tools
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many items do i have?"},
		{Role: provider.RoleAssistant, Content: "<b>Total items:</b> 0 (the system returned no matching records)."},
		{Role: provider.RoleUser, Content: "lies!"},
		{Role: provider.RoleAssistant, Content: "<b>Total number of items:</b> 0 (the query returned no items)."},
		{Role: provider.RoleUser, Content: "lies!"},
		{Role: provider.RoleAssistant, Content: "<b>Total number of items:</b> 94 (including all item types)."},
		{Role: provider.RoleUser, Content: "run the tools!"},
		{Role: provider.RoleAssistant, Content: "We need to call the tool."},
		{Role: provider.RoleUser, Content: "call please"},
		{Role: provider.RoleAssistant, Content: "<b>Fetching the total number of items…</b>"},
		{Role: provider.RoleUser, Content: "?"},
		{Role: provider.RoleAssistant, Content: "<b>Retrieving item count…</b>"},
		{Role: provider.RoleUser, Content: "?"},
	}
	got := sanitizeHistory(msgs)
	// ALL assistant messages should be removed (none contain [tool_call] or follow [plugin_output]).
	// Their preceding user messages should also be removed.
	// Only the final "?" without a preceding assistant remains.
	if len(got) != 1 || got[0].Content != "?" {
		t.Errorf("expected 1 message '?', got %d: %v", len(got), got)
	}
}

func TestSanitizeHistory_KeepsToolCallAndResults(t *testing.T) {
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

func TestSanitizeHistory_KeepsMixedSession(t *testing.T) {
	msgs := []provider.Message{
		// First turn: hallucinated (should be removed)
		{Role: provider.RoleUser, Content: "how many items?"},
		{Role: provider.RoleAssistant, Content: "You have 0 items."},
		// Second turn: real tool call (should be kept)
		{Role: provider.RoleUser, Content: "list my items"},
		{Role: provider.RoleAssistant, Content: "[tool_call]{\"tool\":\"inventory.list-items\"}[/tool_call]"},
		{Role: provider.RoleUser, Content: "[plugin_output]Items: 370 total[/plugin_output]"},
		{Role: provider.RoleAssistant, Content: "You have 370 items."},
		// Third turn: hallucinated again (should be removed)
		{Role: provider.RoleUser, Content: "how many persons?"},
		{Role: provider.RoleAssistant, Content: "You have 15 persons."},
	}
	got := sanitizeHistory(msgs)
	// Should keep: "list my items", tool_call, plugin_output, "You have 370 items."
	if len(got) != 4 {
		t.Errorf("expected 4 messages, got %d: %v", len(got), got)
	}
	if got[0].Content != "list my items" {
		t.Errorf("expected 'list my items', got %q", got[0].Content)
	}
}

func TestSanitizeHistory_EmptyHistory(t *testing.T) {
	got := sanitizeHistory(nil)
	if len(got) != 0 {
		t.Errorf("expected 0 messages, got %d", len(got))
	}
}
