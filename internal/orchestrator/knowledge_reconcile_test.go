package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

func TestReconcileInjectionState_NoDriftReturnsNil(t *testing.T) {
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]body[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift != nil {
		t.Fatalf("no-drift case must return nil drift, got %+v", drift)
	}
	if len(corrected.KnownKnowledge) != 1 || corrected.KnownKnowledge[0].FirstInjectedTurn != 1 {
		t.Errorf("no-drift state must round-trip unchanged, got %+v", corrected)
	}
}

func TestReconcileInjectionState_MissingFromVisible(t *testing.T) {
	// State thinks kb_b is known, but no kb_b block appears in
	// messages — likely because a sliding-window cut dropped it.
	// Reconciliation must drop kb_b from state and emit drift.
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
			{ArticleID: "kb_b", ContentSHA256: "bbb", FirstInjectedTurn: 2},
		},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]still here[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift == nil {
		t.Fatal("expected drift, got nil")
	}
	if len(drift.MissingFromVisible) != 1 || drift.MissingFromVisible[0] != "bbb" {
		t.Errorf("MissingFromVisible mismatch: %+v", drift.MissingFromVisible)
	}
	if len(drift.ExtrasInVisible) != 0 {
		t.Errorf("ExtrasInVisible must be empty, got %+v", drift.ExtrasInVisible)
	}
	if len(corrected.KnownKnowledge) != 1 || corrected.KnownKnowledge[0].ContentSHA256 != "aaa" {
		t.Errorf("corrected state must keep only the visible SHA, got %+v", corrected.KnownKnowledge)
	}
	if corrected.KnownKnowledge[0].FirstInjectedTurn != 1 {
		t.Errorf("FirstInjectedTurn must survive reconciliation, got %d", corrected.KnownKnowledge[0].FirstInjectedTurn)
	}
}

func TestReconcileInjectionState_ExtrasInVisible(t *testing.T) {
	// A KC block reached the LLM that the state doesn't know about
	// — e.g. a previous turn wrote the message but the state-update
	// failed. Reconciliation must add it to state with
	// FirstInjectedTurn=0 (synthetic "discovered via reconciliation").
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]known[/knowledge_context]`},
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_b" sha="bbb"]new arrival[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift == nil {
		t.Fatal("expected drift, got nil")
	}
	if len(drift.ExtrasInVisible) != 1 || drift.ExtrasInVisible[0] != "bbb" {
		t.Errorf("ExtrasInVisible mismatch: %+v", drift.ExtrasInVisible)
	}
	if len(corrected.KnownKnowledge) != 2 {
		t.Fatalf("corrected state must include both SHAs, got %+v", corrected.KnownKnowledge)
	}
	// kb_b should land with FirstInjectedTurn=0 (synthetic marker).
	for _, e := range corrected.KnownKnowledge {
		if e.ContentSHA256 == "bbb" && e.FirstInjectedTurn != 0 {
			t.Errorf("extras-in-visible entry must have FirstInjectedTurn=0, got %d", e.FirstInjectedTurn)
		}
	}
}

func TestReconcileInjectionState_BothDirectionsAtOnce(t *testing.T) {
	// State thinks kb_a is known but it was cut; kb_b reached the LLM
	// without the state knowing. Both drift signals should fire in
	// one event.
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_b" sha="bbb"]new[/knowledge_context]`},
	}
	_, drift := reconcileInjectionState(msgs, persisted)
	if drift == nil {
		t.Fatal("expected drift, got nil")
	}
	if len(drift.MissingFromVisible) != 1 || drift.MissingFromVisible[0] != "aaa" {
		t.Errorf("MissingFromVisible: %+v", drift.MissingFromVisible)
	}
	if len(drift.ExtrasInVisible) != 1 || drift.ExtrasInVisible[0] != "bbb" {
		t.Errorf("ExtrasInVisible: %+v", drift.ExtrasInVisible)
	}
	if drift.ReconciliationAction != reconciliationActionRewriteFromVisible {
		t.Errorf("ReconciliationAction = %q, want %q", drift.ReconciliationAction, reconciliationActionRewriteFromVisible)
	}
}

func TestReconcileInjectionState_LegacyUntaggedBlocksIgnored(t *testing.T) {
	// A legacy bare [knowledge_context] block carries no id/sha so
	// reconciliation can't tie it back to state. It must be ignored
	// for drift purposes — neither counted as extras_in_visible nor
	// as authoritative-and-corrects-state.
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
		},
	}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "[knowledge_context]\nlegacy body, no attributes\n[/knowledge_context]"},
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]tagged[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift != nil {
		t.Fatalf("legacy block alongside matching tagged block must not trigger drift, got %+v", drift)
	}
	if len(corrected.KnownKnowledge) != 1 {
		t.Errorf("corrected state must reflect only the ID-tagged blocks, got %+v", corrected.KnownKnowledge)
	}
}

func TestReconcileInjectionState_DuplicateVisibleSHAs_DedupedInCorrectedState(t *testing.T) {
	// The same article re-appears in two user messages (e.g. score-
	// override re-injected it in turn 5 after turn 1). The corrected
	// state must contain a single entry for that SHA.
	persisted := state.InjectionState{}
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]copy 1[/knowledge_context]`},
		{Role: provider.RoleUser, Content: `[knowledge_context id="kb_a" sha="aaa"]copy 2[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift == nil {
		t.Fatal("expected drift (extra in visible)")
	}
	if len(corrected.KnownKnowledge) != 1 || corrected.KnownKnowledge[0].ContentSHA256 != "aaa" {
		t.Errorf("dup-SHA must collapse to one entry, got %+v", corrected.KnownKnowledge)
	}
}

func TestReconcileInjectionState_NonUserMessagesIgnored(t *testing.T) {
	// Only user-role messages carry [knowledge_context] blocks by
	// construction. A tagged block accidentally appearing in an
	// assistant or tool message must not affect reconciliation.
	persisted := state.InjectionState{}
	msgs := []provider.Message{
		{Role: provider.RoleAssistant, Content: `[knowledge_context id="kb_a" sha="aaa"]wrong-role[/knowledge_context]`},
		{Role: provider.RoleTool, Content: `[knowledge_context id="kb_b" sha="bbb"]wrong-role[/knowledge_context]`},
	}
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift != nil {
		t.Errorf("non-user-role blocks must not trigger drift, got %+v", drift)
	}
	if len(corrected.KnownKnowledge) != 0 {
		t.Errorf("non-user-role blocks must not appear in corrected state, got %+v", corrected.KnownKnowledge)
	}
}

func TestReconcileInjectionState_PreservesPhase4KnownTools(t *testing.T) {
	// Phase 4's KnownTools entries must survive reconciliation
	// regardless of knowledge-side drift.
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_drop", ContentSHA256: "drop", FirstInjectedTurn: 1},
		},
		KnownTools: []state.KnownToolEntry{
			{ToolName: "timly__keep", Tier: "tier1", LRURank: 7},
		},
	}
	msgs := []provider.Message{} // empty → kb_drop is missing
	corrected, drift := reconcileInjectionState(msgs, persisted)
	if drift == nil {
		t.Fatal("expected drift on missing kb_drop")
	}
	if len(corrected.KnownTools) != 1 || corrected.KnownTools[0].ToolName != "timly__keep" {
		t.Errorf("KnownTools must survive reconciliation, got %+v", corrected.KnownTools)
	}
}

func TestReconcileAndEmitDrift_EmitsEventWithMissingAndExtras(t *testing.T) {
	// End-to-end: a session with persisted state but a visible-message
	// scan that disagrees. The orchestrator method must emit one
	// drift_detected event with the correct buckets.
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {
				KnownKnowledge: []state.KnownKnowledgeEntry{
					{ArticleID: "kb_gone", ContentSHA256: "gone", FirstInjectedTurn: 1},
				},
			},
		},
	}
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	_ = sessions.AddMessage("s1", provider.Message{
		Role:    provider.RoleUser,
		Content: `[knowledge_context id="kb_new" sha="new"]surprise body[/knowledge_context]`,
	})
	orch := &Orchestrator{
		sessions:            sessions,
		eventSink:           sink,
		injectionStateStore: dedupStore,
	}
	corrected := orch.reconcileAndEmitDrift(context.Background(), "s1")
	if len(corrected.KnownKnowledge) != 1 || corrected.KnownKnowledge[0].ContentSHA256 != "new" {
		t.Errorf("corrected state must reflect visible SHA, got %+v", corrected.KnownKnowledge)
	}
	evs := sink.snapshot()
	drift := findEventByType(evs, events.TypeDriftDetected)
	if drift == nil {
		t.Fatal("drift_detected event missing")
	}
	var p events.DriftDetectedPayload
	if err := json.Unmarshal(drift.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.MissingFromVisible) != 1 || p.MissingFromVisible[0] != "gone" {
		t.Errorf("MissingFromVisible = %v", p.MissingFromVisible)
	}
	if len(p.ExtrasInVisible) != 1 || p.ExtrasInVisible[0] != "new" {
		t.Errorf("ExtrasInVisible = %v", p.ExtrasInVisible)
	}
	if p.ReconciliationAction == "" {
		t.Errorf("ReconciliationAction must be set")
	}
}

func TestReconcileAndEmitDrift_NoStoreReturnsEmptyState(t *testing.T) {
	// Defensive guard: when no InjectionStateStore is wired the
	// reconciliation step still returns a usable (zero-valued) state
	// so the dedup decision can proceed as if first-turn.
	orch := &Orchestrator{
		sessions:  state.NewSessionStore(""),
		eventSink: &recordingEventSink{},
	}
	got := orch.reconcileAndEmitDrift(context.Background(), "s-no-store")
	if len(got.KnownKnowledge) != 0 {
		t.Errorf("nil-store path must return empty state, got %+v", got)
	}
}

func TestReconcileInjectionState_EmptyBothReturnsNilDrift(t *testing.T) {
	// Pinning the no-input case so a future refactor that adds a "drift
	// on first-turn" misfeature gets caught: empty state + empty
	// messages must produce no drift event.
	corrected, drift := reconcileInjectionState(nil, state.InjectionState{})
	if drift != nil {
		t.Errorf("empty inputs must yield nil drift, got %+v", drift)
	}
	if len(corrected.KnownKnowledge) != 0 {
		t.Errorf("empty inputs must yield empty corrected state, got %+v", corrected)
	}
}

func TestReconcileInjectionState_EmptyMessagesDropsAllKnownKnowledge(t *testing.T) {
	// State carries entries but the message stream is empty (e.g. a
	// summarization just collapsed history). Reconciliation must wipe
	// KnownKnowledge to match the visible truth.
	persisted := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
			{ArticleID: "kb_b", ContentSHA256: "bbb", FirstInjectedTurn: 1},
		},
	}
	corrected, drift := reconcileInjectionState(nil, persisted)
	if drift == nil {
		t.Fatal("expected drift, got nil")
	}
	if len(corrected.KnownKnowledge) != 0 {
		t.Errorf("empty messages must drop all KnownKnowledge, got %+v", corrected.KnownKnowledge)
	}
	if len(drift.MissingFromVisible) != 2 {
		t.Errorf("both SHAs must appear as missing, got %v", drift.MissingFromVisible)
	}
}

func TestReconcileAndEmitDrift_GetStateErrorReturnsEmptyAndNoEvent(t *testing.T) {
	// Robustness contract: when GetInjectionState fails the
	// reconciliation step returns empty state (so the dedup decision
	// treats every candidate as new) and does NOT emit drift_detected
	// (there's nothing to reconcile against).
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{failGetErr: errors.New("simulated read failure")}
	orch := &Orchestrator{
		sessions:            state.NewSessionStore(""),
		eventSink:           sink,
		injectionStateStore: dedupStore,
	}
	got := orch.reconcileAndEmitDrift(context.Background(), "s1")
	if len(got.KnownKnowledge) != 0 {
		t.Errorf("read failure must yield empty state, got %+v", got)
	}
	if findEventByType(sink.snapshot(), events.TypeDriftDetected) != nil {
		t.Errorf("read failure must not emit drift_detected")
	}
}

func TestReconcileAndEmitDrift_SessionLookupErrorReturnsEmpty(t *testing.T) {
	// Symmetric robustness path: when the session can't be loaded
	// (e.g. evicted between turns) the visible-scan can't run, so the
	// authoritative-visible rule defaults to empty state — caller
	// treats every candidate as new rather than trusting stale state.
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownKnowledge: []state.KnownKnowledgeEntry{
				{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
			}},
		},
	}
	orch := &Orchestrator{
		sessions:            state.NewSessionStore(""), // empty store — session "s1" doesn't exist
		eventSink:           sink,
		injectionStateStore: dedupStore,
	}
	got := orch.reconcileAndEmitDrift(context.Background(), "s1")
	if len(got.KnownKnowledge) != 0 {
		t.Errorf("session-lookup failure must yield empty state, got %+v", got)
	}
	if findEventByType(sink.snapshot(), events.TypeDriftDetected) != nil {
		t.Errorf("session-lookup failure must not emit drift_detected")
	}
}

func TestReconcileAndEmitDrift_NoDriftEmitsNothing(t *testing.T) {
	// Regression guard: drift_detected must NOT fire on aligned state.
	sink := &recordingEventSink{}
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	_ = sessions.AddMessage("s1", provider.Message{
		Role:    provider.RoleUser,
		Content: `[knowledge_context id="kb_a" sha="aaa"]body[/knowledge_context]`,
	})
	dedupStore := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownKnowledge: []state.KnownKnowledgeEntry{
				{ArticleID: "kb_a", ContentSHA256: "aaa", FirstInjectedTurn: 1},
			}},
		},
	}
	orch := &Orchestrator{
		sessions:            sessions,
		eventSink:           sink,
		injectionStateStore: dedupStore,
	}
	orch.reconcileAndEmitDrift(context.Background(), "s1")
	if findEventByType(sink.snapshot(), events.TypeDriftDetected) != nil {
		t.Error("no-drift path must not emit drift_detected")
	}
}
