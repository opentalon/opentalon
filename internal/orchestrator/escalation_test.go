package orchestrator

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// escPushRecorder captures ChannelSender invocations and signals on got so an
// async escalation test can wait for the reply push instead of racing the
// background goroutine.
type escPushRecorder struct {
	mu    sync.Mutex
	calls []titlePushCall
	got   chan struct{}
}

func newEscPushRecorder() *escPushRecorder {
	return &escPushRecorder{got: make(chan struct{}, 8)}
}

func (r *escPushRecorder) push(_ context.Context, sessionID string, msg pkgchannel.OutboundMessage) error {
	r.mu.Lock()
	r.calls = append(r.calls, titlePushCall{SessionID: sessionID, Msg: msg})
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
	return nil
}

func (r *escPushRecorder) snapshot() []titlePushCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]titlePushCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// stubLimitChecker is a UsageLimitChecker test double returning a fixed spend.
type stubLimitChecker struct {
	used int
	err  error
}

func (s stubLimitChecker) TotalTokensSince(_ context.Context, _ string, _ time.Time) (int, error) {
	return s.used, s.err
}

// escTestOrch builds an orchestrator with the escalation surface wired and a
// session seeded, returning the orchestrator, the escalation executor, the LLM
// stub and the push recorder.
func escTestOrch(t *testing.T, opts OrchestratorOpts) (*Orchestrator, *escalationExecutor, *fakeLLM, *escPushRecorder) {
	t.Helper()
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "ent1", "grp1", "")
	llm := &fakeLLM{responses: []string{"Investigated: two items at risk — reorder recommended."}}
	push := newEscPushRecorder()
	opts.ChannelSender = push.push
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, opts)
	return orch, &escalationExecutor{orch: orch}, llm, push
}

func decodeEscStatus(t *testing.T, res ToolResult) escalationResult {
	t.Helper()
	if res.Error != "" {
		t.Fatalf("unexpected executor error: %s", res.Error)
	}
	var got escalationResult
	if err := json.Unmarshal([]byte(res.Content), &got); err != nil {
		t.Fatalf("decode escalation status %q: %v", res.Content, err)
	}
	return got
}

func escCall(fromLLM bool) ToolCall {
	return ToolCall{
		ID:      "esc-1",
		Plugin:  escalatePluginName,
		Action:  escalateTurnAction,
		Args:    map[string]string{"session_id": "sess", "prompt": "A watcher tripped; investigate and advise the user."},
		FromLLM: fromLLM,
	}
}

func profileCtx(p profile.Profile) context.Context {
	return profile.WithProfile(context.Background(), &p)
}

func TestEscalation_RejectsLLMSourcedCall(t *testing.T) {
	_, exec, llm, _ := escTestOrch(t, OrchestratorOpts{Escalation: EscalationConfig{Enabled: true}})
	res := exec.Execute(profileCtx(profile.Profile{EntityID: "ent1"}), escCall(true))
	if res.Error == "" {
		t.Fatal("expected error for FromLLM call, got none")
	}
	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", llm.callCount)
	}
}

func TestEscalation_DisabledIsNoOp(t *testing.T) {
	// Escalation not enabled: startEscalation short-circuits without a turn.
	_, exec, llm, _ := escTestOrch(t, OrchestratorOpts{})
	got := decodeEscStatus(t, exec.Execute(profileCtx(profile.Profile{EntityID: "ent1"}), escCall(false)))
	if got.Escalated || got.Reason != "disabled" {
		t.Errorf("status = %+v, want {escalated:false reason:disabled}", got)
	}
	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", llm.callCount)
	}
}

func TestEscalation_NoProfileIsRefused(t *testing.T) {
	_, exec, llm, _ := escTestOrch(t, OrchestratorOpts{Escalation: EscalationConfig{Enabled: true}})
	got := decodeEscStatus(t, exec.Execute(context.Background(), escCall(false)))
	if got.Escalated || got.Reason != "no_profile" {
		t.Errorf("status = %+v, want {escalated:false reason:no_profile}", got)
	}
	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", llm.callCount)
	}
}

func TestEscalation_OverBudgetIsRefused(t *testing.T) {
	_, exec, llm, _ := escTestOrch(t, OrchestratorOpts{
		Escalation:             EscalationConfig{Enabled: true},
		EscalationLimitChecker: stubLimitChecker{used: 1000},
	})
	p := profile.Profile{EntityID: "ent1", Limit: 500, LimitWindow: time.Hour}
	got := decodeEscStatus(t, exec.Execute(profileCtx(p), escCall(false)))
	if got.Escalated || got.Reason != "limit" {
		t.Errorf("status = %+v, want {escalated:false reason:limit}", got)
	}
	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", llm.callCount)
	}
}

func TestEscalation_InFlightIsDropped(t *testing.T) {
	orch, exec, llm, _ := escTestOrch(t, OrchestratorOpts{Escalation: EscalationConfig{Enabled: true}})
	// Simulate an escalation already running for this session by holding the
	// per-session guard, so no real goroutine/timing is involved.
	entry, ok := orch.escalationMuxes.tryLock("sess")
	if !ok {
		t.Fatal("could not acquire escalation guard for setup")
	}
	defer orch.escalationMuxes.unlock("sess", entry)

	got := decodeEscStatus(t, exec.Execute(profileCtx(profile.Profile{EntityID: "ent1"}), escCall(false)))
	if got.Escalated || got.Reason != "in_flight" {
		t.Errorf("status = %+v, want {escalated:false reason:in_flight}", got)
	}
	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0", llm.callCount)
	}
}

func TestEscalation_AbsentFromLLMToolCatalog(t *testing.T) {
	// UserOnly keeps _escalate out of the session-callable palette (the same
	// set that feeds the LLM tool catalog and the load_tools visibility gate),
	// so the model can never see or invoke it.
	orch, _, _, _ := escTestOrch(t, OrchestratorOpts{Escalation: EscalationConfig{Enabled: true}})
	set := allowedToolsSet(profileCtx(profile.Profile{EntityID: "ent1"}), orch)
	if _, ok := set[toolFQN(escalatePluginName, escalateTurnAction)]; ok {
		t.Errorf("%s present in session-callable tool set, want absent (UserOnly)", toolFQN(escalatePluginName, escalateTurnAction))
	}
}

func TestEscalation_HappyPath_RunsTurnAndPushesReply(t *testing.T) {
	orch, exec, _, push := escTestOrch(t, OrchestratorOpts{Escalation: EscalationConfig{Enabled: true}})

	// Limit 0 → no budget pre-check; a real background turn is spawned.
	got := decodeEscStatus(t, exec.Execute(profileCtx(profile.Profile{EntityID: "ent1", Group: "grp1"}), escCall(false)))
	if !got.Escalated || got.Reason != "" {
		t.Fatalf("status = %+v, want {escalated:true}", got)
	}

	select {
	case <-push.got:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for escalation reply push")
	}

	calls := push.snapshot()
	if len(calls) != 1 {
		t.Fatalf("push count = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.SessionID != "sess" {
		t.Errorf("push.SessionID = %q, want \"sess\"", c.SessionID)
	}
	if c.Msg.Content != "Investigated: two items at risk — reorder recommended." {
		t.Errorf("push.Content = %q", c.Msg.Content)
	}
	if c.Msg.Metadata["type"] != escalationMessageType {
		t.Errorf("push.Metadata[type] = %q, want %q", c.Msg.Metadata["type"], escalationMessageType)
	}
	if c.Msg.ConversationID != "" {
		t.Errorf("push.ConversationID = %q, want empty (adapter derives it)", c.Msg.ConversationID)
	}

	// The seed prompt must be recorded hidden (fed to the model, off the
	// user-facing transcript); the assistant reply is visible.
	sess, err := orch.sessions.Get("sess")
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	var foundHiddenSeed, foundVisibleReply bool
	for _, m := range sess.Messages {
		if m.Role == provider.RoleUser && m.Content == "A watcher tripped; investigate and advise the user." {
			if m.Visibility != provider.VisibilityHidden {
				t.Errorf("seed message Visibility = %q, want %q", m.Visibility, provider.VisibilityHidden)
			}
			foundHiddenSeed = true
		}
		if m.Role == provider.RoleAssistant && m.Visibility == "" {
			foundVisibleReply = true
		}
	}
	if !foundHiddenSeed {
		t.Error("hidden seed user message not found in session")
	}
	if !foundVisibleReply {
		t.Error("visible assistant reply not found in session")
	}
}
