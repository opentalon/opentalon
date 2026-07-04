package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

func newConfirmationTestOrchestrator(t *testing.T) (*Orchestrator, string) {
	t.Helper()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"unused"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		NewToolRegistry(), memory, sessions, OrchestratorOpts{})
	return orch, "s1"
}

// TestConfirmationFrameMetadata_KeyContract pins the exact metadata keys and
// values. Downstream consumers — the api-plugin transcript reader and the Timly
// chat widget's history rebuild — branch on these literal strings, so a rename
// here is a silent cross-repo break.
func TestConfirmationFrameMetadata_KeyContract(t *testing.T) {
	tool := toolConfirmationFrameMetadata("call_1")
	if tool["type"] != "confirmation" || tool["prompt_type"] != "tool_confirmation" ||
		tool["options"] != "approve,reject" || tool["tool_call_id"] != "call_1" {
		t.Errorf("tool confirmation metadata contract drift: %+v", tool)
	}
	// tool_call_id is omitted when empty (never an empty-string key).
	if _, ok := toolConfirmationFrameMetadata("")["tool_call_id"]; ok {
		t.Error("empty tool_call_id must be omitted, not set to \"\"")
	}

	pipe := pipelineConfirmationFrameMetadata("pipeline-7")
	if pipe["type"] != "confirmation" || pipe["prompt_type"] != "confirmation" ||
		pipe["options"] != "approve,reject" || pipe["pipeline_id"] != "pipeline-7" {
		t.Errorf("pipeline confirmation metadata contract drift: %+v", pipe)
	}

	reply := confirmationReplyMetadata("approve")
	if reply["prompt_type"] != "confirmation_response" || reply["action"] != "approve" {
		t.Errorf("confirmation reply metadata contract drift: %+v", reply)
	}
}

func TestPendingConfirmationFrame_ToolCallInMemory(t *testing.T) {
	orch, sid := newConfirmationTestOrchestrator(t)
	orch.pendingMu.Lock()
	orch.pendingToolCalls[sid] = &ToolCall{ID: "call_9", Plugin: "timly", Action: "delete-item"}
	orch.pendingConfirmationPrompts[sid] = "Proceed with deleting 3 items?"
	orch.pendingMu.Unlock()

	content, meta, ok := orch.PendingConfirmationFrame(sid)
	if !ok {
		t.Fatal("ok = false, want true for a pending tool call")
	}
	if content != "Proceed with deleting 3 items?" {
		t.Errorf("content = %q, want the stored prompt", content)
	}
	if meta["prompt_type"] != "tool_confirmation" || meta["tool_call_id"] != "call_9" {
		t.Errorf("metadata mismatch: %+v", meta)
	}

	// Read-only: the pending state must survive the lookup so Run's Block A2
	// can still consume it when the decision arrives.
	orch.pendingMu.Lock()
	stillPending := orch.pendingToolCalls[sid] != nil
	orch.pendingMu.Unlock()
	if !stillPending {
		t.Error("PendingConfirmationFrame consumed the pending state; it must be read-only")
	}
}

func TestPendingConfirmationFrame_ToolCallFromPersistedBlob(t *testing.T) {
	orch, sid := newConfirmationTestOrchestrator(t)
	// No in-memory state (simulating a pod restart) — only the persisted blob.
	savePendingToolCall(orch.sessions, sid, &ToolCall{ID: "call_restored", Plugin: "timly", Action: "delete-item"}, "", "Proceed after restart?")

	content, meta, ok := orch.PendingConfirmationFrame(sid)
	if !ok {
		t.Fatal("ok = false, want true (persisted blob fallback)")
	}
	if content != "Proceed after restart?" {
		t.Errorf("content = %q, want the persisted prompt", content)
	}
	if meta["tool_call_id"] != "call_restored" {
		t.Errorf("tool_call_id = %q, want call_restored", meta["tool_call_id"])
	}
}

func TestPendingConfirmationFrame_NothingPending(t *testing.T) {
	orch, sid := newConfirmationTestOrchestrator(t)
	if content, meta, ok := orch.PendingConfirmationFrame(sid); ok {
		t.Errorf("ok = true with nothing pending (content=%q meta=%+v)", content, meta)
	}
}

func TestRun_ConfirmationDecisionWithNothingPending_ReturnsExpiredFrame(t *testing.T) {
	orch, sid := newConfirmationTestOrchestrator(t)

	// A deterministic button decision arrives but nothing is pending (pod
	// restarted / already resolved). The literal "approve" must NOT be handled
	// as a normal chat turn — the client gets a typed expired frame instead.
	ctx := actor.WithConfirmationDecision(context.Background(), "approve")
	res, err := orch.Run(ctx, sid, "approve")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil || res.Metadata["action"] != "confirmation_expired" {
		t.Fatalf("action = %q, want confirmation_expired (res=%+v)", res.Metadata["action"], res)
	}
	// Mirrors the reject rule: an expired reply adds no rows to the transcript.
	sess, _ := orch.sessions.Get(sid)
	if len(sess.Messages) != 0 {
		t.Errorf("expired path must add no messages, got %d: %+v", len(sess.Messages), sess.Messages)
	}
}

func TestRun_FreeTextWithNothingPending_IsNotExpired(t *testing.T) {
	orch, sid := newConfirmationTestOrchestrator(t)

	// No confirmation decision on the context (ordinary free text). Nothing
	// pending must NOT produce an expired frame — only a deterministic button
	// click with nothing pending does.
	res, err := orch.Run(context.Background(), sid, "hello there")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res != nil && res.Metadata["action"] == "confirmation_expired" {
		t.Error("free text with nothing pending must not be treated as an expired confirmation")
	}
}
