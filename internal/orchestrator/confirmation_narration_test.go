package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// TestNarrateConfirmation_FeedsRecentContextAndReturnsLLMText pins the
// confirmation narration: it hands BOTH the recent conversation (where a list
// call's count + example records live) AND the tool call to the LLM — so the LLM
// can resolve an opaque scope_token to a human description in the user's
// language — and returns the LLM's text verbatim.
func TestNarrateConfirmation_FeedsRecentContextAndReturnsLLMText(t *testing.T) {
	llm := &capturingLLM{responses: []string{"Sie möchten 2 Abräumwagen löschen — fortfahren?"}}
	orch := NewWithRules(llm, &fakeParser{}, NewToolRegistry(),
		state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{})

	recent := []provider.Message{
		{Role: provider.RoleUser, Content: "lösche die Abräumwagen"},
		{Role: provider.RoleUser, Content: "Items: 2 total • Abräumwagen [id: 2134281] • Abräumwagen [id: 2134285]"},
	}
	call := ToolCall{Action: "timly.delete-item", Args: map[string]string{"scope_token": "scope_xyz"}}

	got := orch.narrateConfirmation(context.Background(), recent, call, "lösche die Abräumwagen")

	if got != "Sie möchten 2 Abräumwagen löschen — fortfahren?" {
		t.Errorf("must return the LLM narration verbatim, got %q", got)
	}
	if len(llm.requests) != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", len(llm.requests))
	}
	var sys, usr string
	for _, m := range llm.requests[0].Messages {
		switch m.Role {
		case provider.RoleSystem:
			sys = m.Content
		case provider.RoleUser:
			usr = m.Content
		}
	}
	if !strings.Contains(strings.ToLower(sys), "language") {
		t.Errorf("system prompt must steer the user's language, got: %q", sys)
	}
	// The preceding list result (count + an example id) must reach the LLM so it
	// can describe the opaque scope_token meaningfully.
	if !strings.Contains(usr, "2134281") {
		t.Errorf("recent tool result (example id) missing from LLM input, got: %q", usr)
	}
	if !strings.Contains(usr, "scope_xyz") {
		t.Errorf("tool args should be in the LLM input, got: %q", usr)
	}
}

// TestNarrateConfirmation_NilLLMReturnsEmpty pins the fallback hook: with no LLM
// the method returns "" so the caller uses the static template.
func TestNarrateConfirmation_NilLLMReturnsEmpty(t *testing.T) {
	orch := &Orchestrator{}
	if got := orch.narrateConfirmation(context.Background(), nil, ToolCall{Action: "x"}, ""); got != "" {
		t.Errorf("nil LLM must return empty string, got %q", got)
	}
}
