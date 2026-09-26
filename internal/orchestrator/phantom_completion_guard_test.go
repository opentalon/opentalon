package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// The phantom-completion guard exists because prompt-side rules demonstrably
// do not stop the failure they target: the model loads a write tool, resolves
// every parameter through lookups, and then NARRATES the mutation instead of
// calling it ("The item has been assigned to John Doe" — no assign call, no
// confirmation, nothing persisted). Three separate never-claim-unexecuted-
// actions instructions were in context when production sessions did exactly
// that. These tests pin the structural detection instead: write tool loaded
// this turn + no write attempted + plain-text finish → exactly one nudge.

// phantomOrch wires a registry with one read-only lookup and one write action,
// a scripted text-mode LLM that records every request (so tests can see
// exactly what the nudge round was sent), and a parser that understands two
// markers:
// "LOAD"  → a _meta__load_tools call for the write tool
// "WRITE" → a direct call to the write tool
// Any other response parses to nil, i.e. a final plain-text answer.
func phantomOrch(t *testing.T, responses []string) (*Orchestrator, *capturingLLM, string) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "phantom-guard fixtures",
		Actions: []Action{
			{Name: "lookup", Description: "Read-only lookup.", ReadOnly: true},
			{Name: "write", Description: "A mutating action."},
		},
	}, &echoExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &capturingLLM{responses: responses}
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		switch {
		case strings.Contains(response, "LOAD"):
			return []ToolCall{{ID: "c-load", Plugin: metaPluginName, Action: metaLoadTools,
				Args: map[string]string{"names": "p__write"}}}
		case strings.Contains(response, "WRITE"):
			return []ToolCall{{ID: "c-write", Plugin: "p", Action: "write", Args: map[string]string{}}}
		default:
			return nil
		}
	}}
	orch := NewWithRules(llm, parser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	return orch, llm, "s1"
}

// assertNudgedAfter checks that LLM round `round` (1-based) was sent the
// rejected plain-text answer followed by exactly the phantom-completion nudge
// as its last two messages.
func assertNudgedAfter(t *testing.T, llm *capturingLLM, round int, answer string) {
	t.Helper()
	if len(llm.requests) < round {
		t.Fatalf("expected at least %d LLM requests, got %d", round, len(llm.requests))
	}
	msgs := llm.requests[round-1].Messages
	if len(msgs) < 2 {
		t.Fatalf("round %d: expected the rejected answer and the nudge, got %d messages", round, len(msgs))
	}
	prev, last := msgs[len(msgs)-2], msgs[len(msgs)-1]
	if prev.Role != provider.RoleAssistant || prev.Content != answer {
		t.Errorf("round %d: second-to-last message = %s %q, want the rejected assistant answer %q",
			round, prev.Role, prev.Content, answer)
	}
	if last.Role != provider.RoleUser || last.Content != phantomCompletionNudge {
		t.Errorf("round %d: last message = %s %q, want the phantom-completion nudge", round, last.Role, last.Content)
	}
}

// The phantom shape: load the write tool, then answer as if the write had
// happened. The guard must inject exactly one nudge (an extra LLM round) and
// accept the round-3 answer — by then the model either corrected itself or
// asked the user; both are legitimate, unlike the silent false "done".
func TestPhantomGuard_NudgesWhenWriteLoadedButNeverCalled(t *testing.T) {
	orch, llm, sessID := phantomOrch(t, []string{
		"LOAD",
		"The item has been assigned to John Doe.",
		"I have not executed the assignment yet — which schedule should it use?",
	})

	result, err := orch.Run(context.Background(), sessID, "assign the drill to John Doe")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 3 {
		t.Errorf("expected 3 LLM rounds (load, phantom answer, corrected answer), got %d", llm.callCount)
	}
	assertNudgedAfter(t, llm, 3, "The item has been assigned to John Doe.")
	if !strings.Contains(result.Response, "not executed") {
		t.Errorf("final answer must be the corrected round-3 response, got %q", result.Response)
	}
}

// Announcing a write instead of calling it is the same failure as claiming it
// is done: the plain-text answer ends the turn, so "I will now run the
// deletion" promises something that never happens. The nudge must treat an
// announcement like a completion claim (call the tool now or ask the user)
// and must not offer the "send it again unchanged" exit for it — that exit is
// only for answers that neither claim nor announce an action.
func TestPhantomGuard_NudgesAnnouncedAction(t *testing.T) {
	announcement := "Since you already confirmed, I will now run the final deletion."
	orch, llm, sessID := phantomOrch(t, []string{
		"LOAD",
		announcement,
		"WRITE",
		"Done — the item has been deleted.",
	})

	result, err := orch.Run(context.Background(), sessID, "delete the drill")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 4 {
		t.Errorf("expected 4 LLM rounds (load, announcement, write, summary), got %d", llm.callCount)
	}
	assertNudgedAfter(t, llm, 3, announcement)
	if !strings.Contains(result.Response, "Done") {
		t.Errorf("final answer must be the post-write summary, got %q", result.Response)
	}

	for _, want := range []string{
		"says it is being done or is about to be done", // announcements count like completion claims
		"Your answer ends the turn",                    // why an announcement is false
		"only if it neither claims nor announces an action",
	} {
		if !strings.Contains(phantomCompletionNudge, want) {
			t.Errorf("nudge must contain %q, got %q", want, phantomCompletionNudge)
		}
	}
}

// The nudge is strictly one-shot: if the model repeats the phantom claim after
// being nudged, the answer passes through rather than looping the turn forever.
func TestPhantomGuard_NudgeIsOneShot(t *testing.T) {
	orch, llm, sessID := phantomOrch(t, []string{
		"LOAD",
		"The item has been assigned.",
		"The item has been assigned.", // model insists — must not loop again
	})

	result, err := orch.Run(context.Background(), sessID, "assign the drill")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 3 {
		t.Errorf("expected exactly one nudge round, got %d LLM rounds", llm.callCount)
	}
	if result.Response == "" {
		t.Error("the repeated answer must still be delivered after the one-shot nudge")
	}
}

// An actually-executed write disarms the guard: load, write, summarize — three
// rounds, no nudge. A guard that failed to record the write attempt would burn
// a fourth round here and trip fakeLLM's out-of-responses error.
func TestPhantomGuard_QuietWhenWriteExecuted(t *testing.T) {
	orch, llm, sessID := phantomOrch(t, []string{
		"LOAD",
		"WRITE",
		"Done — the item has been assigned.",
	})

	result, err := orch.Run(context.Background(), sessID, "assign the drill")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 3 {
		t.Errorf("expected 3 LLM rounds with no nudge, got %d", llm.callCount)
	}
	if !strings.Contains(result.Response, "Done") {
		t.Errorf("expected the summary answer, got %q", result.Response)
	}
}

// Ordering contract: an EMPTY response while the guard is armed must take the
// empty-retry path first — the nudge may never wrap an empty assistant message
// (some providers reject those). The guard then still fires on the next
// non-empty plain-text answer: load, empty, honest answer (nudged), final.
func TestPhantomGuard_EmptyResponseHandledBeforeNudge(t *testing.T) {
	orch, llm, sessID := phantomOrch(t, []string{
		"LOAD",
		"",
		"I could not complete this.",
		"I have not executed the assignment — which person do you mean?",
	})

	result, err := orch.Run(context.Background(), sessID, "assign the drill")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 4 {
		t.Errorf("expected 4 rounds (load, empty-retry, nudged answer, final), got %d", llm.callCount)
	}
	if !strings.Contains(result.Response, "not executed") {
		t.Errorf("final answer must be the post-nudge response, got %q", result.Response)
	}
}

// Read-only traffic can never pay for the guard: no write tool is loaded, so a
// plain one-round answer stays a one-round answer.
func TestPhantomGuard_QuietOnReadOnlyTurn(t *testing.T) {
	orch, llm, sessID := phantomOrch(t, []string{
		"You have 16 items in stock.",
	})

	result, err := orch.Run(context.Background(), sessID, "how many items do we have?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if llm.callCount != 1 {
		t.Errorf("expected a single LLM round, got %d", llm.callCount)
	}
	if !strings.Contains(result.Response, "16 items") {
		t.Errorf("unexpected answer: %q", result.Response)
	}
}
