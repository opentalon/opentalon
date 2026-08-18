package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// Contract: stripApprovedConfirmationExchanges removes ONLY resolved tool-call
// confirmation exchanges — a narrated prompt row (prompt_type=tool_confirmation)
// followed directly by its button reply row (prompt_type=confirmation_response) —
// from the history fed to the model. Everything else survives untouched: prompts
// still pending or amended (no reply row follows), pipeline plan confirmations
// (prompt_type=confirmation), and every ordinary message. The regression this
// guards: replayed prompt+reply pairs teach the model to imitate the gate and
// ask "Shall I go ahead?" in plain text instead of calling the tool, stranding
// multi-write turns (ai_eval multi/container_creation_keeps_context — identical
// on gpt-oss, GLM-4.7 and Kimi K3).

// TestStrip_ResolvedExchangesDropped mirrors the observed Box A/B/C turn: two
// approved creates, each with its narrated prompt and "y" reply. Both exchanges
// must vanish; the tool calls, their results, and the real user messages stay.
func TestStrip_ResolvedExchangesDropped(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Please create three: 'Box A', 'Box B' and 'Box C'."},
		{Role: provider.RoleAssistant, Content: "This will create Box A. Shall I go ahead?", PromptType: provider.PromptTypeToolConfirmation},
		{Role: provider.RoleUser, Content: "y", PromptType: provider.PromptTypeConfirmationResponse},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "timly__create-container"}}},
		{Role: provider.RoleTool, Content: "Created container: Box A", ToolCallID: "c1"},
		{Role: provider.RoleAssistant, Content: "This will create Box B. Shall I go ahead?", PromptType: provider.PromptTypeToolConfirmation},
		{Role: provider.RoleUser, Content: "y", PromptType: provider.PromptTypeConfirmationResponse},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c2", Name: "timly__create-container"}}},
		{Role: provider.RoleTool, Content: "Created container: Box B", ToolCallID: "c2"},
	}

	got := stripApprovedConfirmationExchanges(msgs)

	if len(got) != 5 {
		t.Fatalf("want 5 surviving messages, got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.PromptType == provider.PromptTypeToolConfirmation || m.PromptType == provider.PromptTypeConfirmationResponse {
			t.Fatalf("confirmation exchange row leaked into history: %+v", m)
		}
	}
	if got[0].Content != "Please create three: 'Box A', 'Box B' and 'Box C'." {
		t.Fatalf("real user message must survive, got %q", got[0].Content)
	}
	if len(got[1].ToolCalls) == 0 || got[2].ToolCallID != "c1" {
		t.Fatalf("tool call + result pair must survive adjacent, got %+v / %+v", got[1], got[2])
	}
}

// TestStrip_UnresolvedPromptKept: a prompt with no reply row after it (pending,
// rejected, or amended — the correction arrives as a plain user message) must
// stay, because re-planning needs the proposed action for context.
func TestStrip_UnresolvedPromptKept(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Delete the old drill"},
		{Role: provider.RoleAssistant, Content: "This will delete 'Drill 5'. Proceed?", PromptType: provider.PromptTypeToolConfirmation},
		{Role: provider.RoleUser, Content: "no — I meant Drill 7, the defective one"},
	}

	got := stripApprovedConfirmationExchanges(msgs)

	if len(got) != 3 {
		t.Fatalf("nothing may be dropped without a reply row, got %d: %+v", len(got), got)
	}
	if got[1].PromptType != provider.PromptTypeToolConfirmation {
		t.Fatalf("the unresolved prompt must survive, got %+v", got[1])
	}
}

// TestStrip_PipelineExchangeKept: multi-step plan confirmations persist their
// prompt with prompt_type=confirmation (not tool_confirmation), so the pair —
// plan text AND its reply — stays in model history.
func TestStrip_PipelineExchangeKept(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Set up the new branch"},
		{Role: provider.RoleAssistant, Content: "Plan: 1) create org-unit 2) create rooms. OK?", PromptType: "confirmation"},
		{Role: provider.RoleUser, Content: "y", PromptType: provider.PromptTypeConfirmationResponse},
	}

	got := stripApprovedConfirmationExchanges(msgs)

	if len(got) != 3 {
		t.Fatalf("pipeline exchanges must stay intact, got %d: %+v", len(got), got)
	}
}

// TestStrip_PlainConversationUntouched: ordinary turns carry no PromptType and
// must pass through byte-identically, clarifying questions included.
func TestStrip_PlainConversationUntouched(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "Create a test item"},
		{Role: provider.RoleAssistant, Content: "Which category should it go into?"},
		{Role: provider.RoleUser, Content: "Laptops"},
	}

	got := stripApprovedConfirmationExchanges(msgs)

	if len(got) != len(msgs) {
		t.Fatalf("plain conversation must be untouched, got %d of %d", len(got), len(msgs))
	}
	for i := range msgs {
		if got[i].Content != msgs[i].Content || got[i].Role != msgs[i].Role {
			t.Fatalf("message %d changed: %+v -> %+v", i, msgs[i], got[i])
		}
	}
}
