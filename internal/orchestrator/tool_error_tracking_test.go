package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func newOrchForErrorTrackingTests(t *testing.T, store *fakeInjectionStateStore) *Orchestrator {
	t.Helper()
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	opts := OrchestratorOpts{
		ToolTiers: ToolTiersConfig{Enabled: true},
		ToolErrorHandling: ToolErrorHandlingConfig{
			LoopCapPerTurn:          2,
			StickyDemotionThreshold: 3,
		},
	}
	if store != nil {
		opts.InjectionStateStore = store
	}
	return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, opts)
}

func errorResult() ToolResult { return ToolResult{Error: "boom"} }
func okResult() ToolResult    { return ToolResult{Content: "fine"} }
func sampleCall() ToolCall    { return ToolCall{Plugin: "p", Action: "a"} }

func TestRecordToolOutcome_FirstErrorBelowLoopCapEmitsNoWarning(t *testing.T) {
	orch := newOrchForErrorTrackingTests(t, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")
	warning := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	if warning != nil {
		t.Errorf("first error (count=1, cap=2) must NOT emit warning, got %+v", warning)
	}
}

func TestRecordToolOutcome_AtLoopCapEmitsWarning(t *testing.T) {
	orch := newOrchForErrorTrackingTests(t, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult()) // count=1
	warning := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	if warning == nil {
		t.Fatal("second error (count=2, cap=2) must emit a warning")
	}
	if !strings.Contains(warning.Content, "p.a") || !strings.Contains(warning.Content, "failed 2 times") {
		t.Errorf("warning content lacks tool name or count, got %q", warning.Content)
	}
	if warning.Role == "" {
		t.Errorf("warning must carry a Role, got empty")
	}
}

func TestRecordToolOutcome_AdditionalErrorsAboveCapKeepEmittingWithUpdatedCount(t *testing.T) {
	// The RFC's "N times" wording is dynamic — every error past the
	// cap re-injects with the current count, so the LLM sees the
	// growing failure trail and doesn't miss the nudge.
	orch := newOrchForErrorTrackingTests(t, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	w := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	if w == nil || !strings.Contains(w.Content, "failed 3 times") {
		t.Errorf("third error must re-warn with count=3, got %+v", w)
	}
}

func TestRecordToolOutcome_SuccessResetsCounters(t *testing.T) {
	orch := newOrchForErrorTrackingTests(t, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), okResult())
	// Counter reset — next error must NOT immediately re-trip the warning.
	w := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	if w != nil {
		t.Errorf("post-success error (count=1) must NOT re-trip warning, got %+v", w)
	}
}

func TestRecordToolOutcome_DifferentToolsCountIndependently(t *testing.T) {
	orch := newOrchForErrorTrackingTests(t, nil)
	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = orch.recordToolOutcome(ctx, "s1", ToolCall{Plugin: "p", Action: "a"}, errorResult())
	w := orch.recordToolOutcome(ctx, "s1", ToolCall{Plugin: "p", Action: "b"}, errorResult())
	if w != nil {
		t.Errorf("p.b first-error must NOT re-trip warning (independent counter), got %+v", w)
	}
}

func TestRecordToolOutcome_TierLogicOffShortCircuits(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ToolTiers: ToolTiersConfig{Enabled: false},
	})
	ctx := actor.WithSessionID(context.Background(), "s1")
	for i := 0; i < 5; i++ {
		w := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
		if w != nil {
			t.Errorf("tier-off must short-circuit tracking, got warning iter %d: %+v", i, w)
		}
	}
}

func TestRecordToolOutcome_StickyDemotionFlipsDemotedFlag(t *testing.T) {
	store := &fakeInjectionStateStore{}
	orch := newOrchForErrorTrackingTests(t, store)
	ctx := actor.WithSessionID(context.Background(), "s1")

	for i := 0; i < 3; i++ { // threshold=3
		_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	}
	// After 3rd error: sticky-demotion threshold trips, write should fire.
	if store.updateCalls == 0 {
		t.Fatal("sticky-demotion must persist via UpdateInjectionState")
	}
	found := false
	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "p.a" && kt.Demoted {
			found = true
		}
	}
	if !found {
		t.Errorf("p.a must be Demoted=true after threshold, got %+v", store.lastWritten.KnownTools)
	}
}

func TestRecordToolOutcome_SuccessAfterDemotionSelfHeals(t *testing.T) {
	store := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownTools: []state.KnownToolEntry{
				{ToolName: "p.a", Tier: state.KnownToolTier3, Demoted: true},
			}},
		},
	}
	orch := newOrchForErrorTrackingTests(t, store)
	ctx := actor.WithSessionID(context.Background(), "s1")
	// Prime an error so the in-memory session counter is non-zero
	// (recordSuccess returns wasFailing=true only when the session
	// counter had ticked at least once — i.e. the tracker remembers
	// the failure that demoted it before the process restart). For
	// this test the simulated demotion came from a prior session
	// state load + an error this turn.
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), okResult())

	// The clear write should have fired and reset Demoted.
	if store.updateCalls < 1 {
		t.Fatal("self-heal must write the cleared state")
	}
	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "p.a" && kt.Demoted {
			t.Errorf("self-heal must clear Demoted, got Demoted=true")
		}
	}
}

func TestRecordToolOutcome_NewTurnResetsTurnCounter(t *testing.T) {
	// A new turn (different turn number) must reset the per-turn
	// counter so a single failure in the previous turn doesn't
	// short-circuit the cap calculation now.
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ToolTiers: ToolTiersConfig{Enabled: true},
		ToolErrorHandling: ToolErrorHandlingConfig{
			LoopCapPerTurn:          2,
			StickyDemotionThreshold: 99,
		},
	})

	// Turn 1: one error
	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())

	// Simulate next turn by adding a user message — turnNumberForDedup
	// counts user messages + 1, so this bumps the orchestrator's view
	// of "current turn" without going through the full agent loop.
	_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "next"})

	// Turn 2 (now): first error — turn counter reset, so no warning yet.
	w := orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	if w != nil {
		t.Errorf("first error of a new turn must NOT warn (turn counter reset), got %+v", w)
	}
}

func TestRecordToolOutcome_StoreWriteFailureDoesNotBubble(t *testing.T) {
	// A transient store-write failure must not propagate back to the
	// agent loop — it's logged and dropped. The error tracker's
	// in-memory state advances either way; the missing persistence
	// will resync the next time the threshold trips OR the tool
	// self-heals.
	store := &fakeInjectionStateStore{failUpdateErr: errors.New("simulated write failure")}
	orch := newOrchForErrorTrackingTests(t, store)
	ctx := actor.WithSessionID(context.Background(), "s1")
	for i := 0; i < 3; i++ { // hit sticky threshold
		_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), errorResult())
	}
	// No panic, no crash. Test passes if we got here.
}

func TestRecordToolOutcome_FirstCallSuccessNoClearWrite(t *testing.T) {
	// First-ever recordToolOutcome call on a session is a success
	// (no prior failures): wasFailing=false → no clear-write fires.
	// Guards against accidentally writing on every success when the
	// tool was never demoted.
	store := &fakeInjectionStateStore{}
	orch := newOrchForErrorTrackingTests(t, store)
	ctx := actor.WithSessionID(context.Background(), "s1")

	_ = orch.recordToolOutcome(ctx, "s1", sampleCall(), okResult())

	if store.updateCalls != 0 {
		t.Errorf("first-call success on a clean session must not write state, got %d update calls", store.updateCalls)
	}
}

func TestRecordToolOutcome_NoSessionIDShortCircuits(t *testing.T) {
	// An empty session id means we can't address a tracker. Skip
	// quietly rather than synthesizing an "anonymous" bucket.
	orch := newOrchForErrorTrackingTests(t, nil)
	w := orch.recordToolOutcome(context.Background(), "", sampleCall(), errorResult())
	if w != nil {
		t.Errorf("empty sessionID must short-circuit, got %+v", w)
	}
}
