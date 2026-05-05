package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func TestSanitizeHistory_RemovesHallucinatedTemplates(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many org units?"},
		{Role: provider.RoleAssistant, Content: "You have **{{plugin_output.pagination.total}}** org-units."},
		{Role: provider.RoleUser, Content: "that's wrong"},
	}
	got := sanitizeHistory(msgs)
	// The hallucinated assistant message AND its orphaned user message should be removed.
	if len(got) != 1 || got[0].Content != "that's wrong" {
		t.Errorf("expected 1 message, got %d: %v", len(got), got)
	}
}

func TestSanitizeHistory_RemovesNarratedIntent(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many items?"},
		{Role: provider.RoleAssistant, Content: "We'll fetch the total count of items."},
		{Role: provider.RoleUser, Content: "list my items"},
		{Role: provider.RoleAssistant, Content: "[tool_call]{\"tool\":\"timly.list-items\"}[/tool_call]"},
	}
	got := sanitizeHistory(msgs)
	// Narrated message + its user message removed; the real tool call survives.
	if len(got) != 2 {
		t.Errorf("expected 2 messages, got %d: %v", len(got), got)
	}
	if got[0].Content != "list my items" {
		t.Errorf("expected 'list my items', got %q", got[0].Content)
	}
}

func TestSanitizeHistory_RemovesClaimedResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many inactive items?"},
		{Role: provider.RoleAssistant, Content: "Here are the results showing inactive items."},
		{Role: provider.RoleUser, Content: "its no results!"},
	}
	got := sanitizeHistory(msgs)
	if len(got) != 1 || got[0].Content != "its no results!" {
		t.Errorf("expected 1 message, got %d: %v", len(got), got)
	}
}

func TestSanitizeHistory_KeepsLegitMessages(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "how many items?"},
		{Role: provider.RoleAssistant, Content: "[tool_call]{\"tool\":\"timly.list-items\"}[/tool_call]"},
		{Role: provider.RoleUser, Content: "[plugin_output]{\"pagination\":{\"total\":42}}[/plugin_output]"},
		{Role: provider.RoleAssistant, Content: "You have 42 items."},
	}
	got := sanitizeHistory(msgs)
	if len(got) != len(msgs) {
		t.Errorf("expected all %d messages preserved, got %d", len(msgs), len(got))
	}
}
