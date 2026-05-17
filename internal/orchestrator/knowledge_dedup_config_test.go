package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/state"
)

// fakeInjectionStateStore is a minimal InjectionStateStore implementation
// used by the wiring tests. It captures the last write so the test can
// assert the orchestrator's plumbing reaches the dependency.
type fakeInjectionStateStore struct {
	getCalls    int
	updateCalls int
	lastWritten state.InjectionState
}

func (f *fakeInjectionStateStore) GetInjectionState(_ context.Context, _ string) (state.InjectionState, error) {
	f.getCalls++
	return state.InjectionState{}, nil
}

func (f *fakeInjectionStateStore) UpdateInjectionState(_ context.Context, _ string, st state.InjectionState) error {
	f.updateCalls++
	f.lastWritten = st
	return nil
}

func TestKnowledgeDedupConfig_NormalizesZeroValuesToRFCDefaults(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		// KnowledgeDedup intentionally left zero-valued.
	})
	got := orch.knowledgeDedup
	if got.Enabled {
		t.Error("zero-value KnowledgeDedup must keep Enabled=false")
	}
	if got.ReinjectScoreThreshold != 0.85 {
		t.Errorf("ReinjectScoreThreshold default = %v, want 0.85", got.ReinjectScoreThreshold)
	}
	if got.ReinjectTopKForce != 3 {
		t.Errorf("ReinjectTopKForce default = %d, want 3", got.ReinjectTopKForce)
	}
	if got.CapPerTurn != 5 {
		t.Errorf("CapPerTurn default = %d, want 5", got.CapPerTurn)
	}
}

func TestKnowledgeDedupConfig_PreservesExplicitValues(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		KnowledgeDedup: KnowledgeDedupConfig{
			Enabled:                true,
			ReinjectScoreThreshold: 0.95,
			ReinjectTopKForce:      7,
			CapPerTurn:             12,
		},
	})
	got := orch.knowledgeDedup
	if !got.Enabled {
		t.Error("Enabled=true must round-trip")
	}
	if got.ReinjectScoreThreshold != 0.95 || got.ReinjectTopKForce != 7 || got.CapPerTurn != 12 {
		t.Errorf("explicit values clobbered: %+v", got)
	}
}

func TestKnowledgeDedup_NilStoreStaysNilForDisabledFlag(t *testing.T) {
	// Disabled dedup with no store wired is the safe default state
	// every test using the in-memory state.SessionStore lands in. The
	// orchestrator must not synthesize a store on its own.
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{})
	if orch.injectionStateStore != nil {
		t.Errorf("injectionStateStore should default to nil, got %#v", orch.injectionStateStore)
	}
}

func TestKnowledgeDedup_StoreInterfaceIsCarriedThrough(t *testing.T) {
	// When an InjectionStateStore is injected via OrchestratorOpts it
	// must reach the Orchestrator unchanged. Phase 3's decision logic
	// (a later commit) will reach for it via this field.
	store := &fakeInjectionStateStore{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
		InjectionStateStore: store,
	})
	if orch.injectionStateStore != InjectionStateStore(store) {
		t.Errorf("injectionStateStore must reach the orchestrator, got %#v", orch.injectionStateStore)
	}
}
