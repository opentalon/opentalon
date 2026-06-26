package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/state"
)

func TestToolTiersConfig_NormalizesZeroValuesToRFCDefaults(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		// ToolTiers + ToolErrorHandling intentionally left zero-valued.
	})
	got := orch.toolTiers
	if got.Enabled {
		t.Error("zero-value ToolTiers must keep Enabled=false")
	}
	if got.Tier1Cap == nil || *got.Tier1Cap != 10 {
		t.Errorf("Tier1Cap default = %v, want 10", got.Tier1Cap)
	}
	if got.Tier2Cap == nil || *got.Tier2Cap != 15 {
		t.Errorf("Tier2Cap default = %v, want 15", got.Tier2Cap)
	}
	if got.EnableGetToolDetails {
		t.Error("EnableGetToolDetails must default to false")
	}
	errGot := orch.toolErrorHandling
	if errGot.LoopCapPerTurn != 2 {
		t.Errorf("LoopCapPerTurn default = %d, want 2", errGot.LoopCapPerTurn)
	}
	if errGot.StickyDemotionThreshold != 3 {
		t.Errorf("StickyDemotionThreshold default = %d, want 3", errGot.StickyDemotionThreshold)
	}
}

func TestToolTiersConfig_PreservesExplicitValues(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ToolTiers: ToolTiersConfig{
			Enabled:              true,
			Tier1Cap:             intPtr(12),
			Tier2Cap:             intPtr(18),
			EnableGetToolDetails: true,
		},
		ToolErrorHandling: ToolErrorHandlingConfig{
			LoopCapPerTurn:          4,
			StickyDemotionThreshold: 6,
		},
	})
	got := orch.toolTiers
	if !got.Enabled || got.Tier1Cap == nil || *got.Tier1Cap != 12 ||
		got.Tier2Cap == nil || *got.Tier2Cap != 18 || !got.EnableGetToolDetails {
		t.Errorf("explicit ToolTiers values clobbered: %+v", got)
	}
	errGot := orch.toolErrorHandling
	if errGot.LoopCapPerTurn != 4 || errGot.StickyDemotionThreshold != 6 {
		t.Errorf("explicit ToolErrorHandling values clobbered: %+v", errGot)
	}
}

func TestToolTiersConfig_EnableGetToolDetailsImpliesEnabled(t *testing.T) {
	// Enabling the meta-tool without the master switch would register
	// a Tier-0 tool that no one can use. The normalization step
	// upgrades Enabled→true in that case so the YAML "set just the
	// meta-tool flag" shorthand stays operator-friendly.
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ToolTiers: ToolTiersConfig{
			Enabled:              false,
			EnableGetToolDetails: true,
		},
	})
	if !orch.toolTiers.Enabled {
		t.Error("EnableGetToolDetails=true must upgrade Enabled to true")
	}
	if !orch.toolTiers.EnableGetToolDetails {
		t.Error("EnableGetToolDetails must remain true after normalization")
	}
}

func TestToolTiersConfig_DisabledStaysDisabled(t *testing.T) {
	// Inverse of the upgrade rule: with both flags false (the safe
	// default) Enabled must stay false even after normalization, so
	// the orchestrator falls through to the pre-Phase-4 single-tier
	// behaviour. Caps are still normalized so any later toggle of
	// Enabled gets sane numbers without restart.
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ToolTiers: ToolTiersConfig{
			Enabled:              false,
			EnableGetToolDetails: false,
		},
	})
	if orch.toolTiers.Enabled {
		t.Error("Enabled=false + EnableGetToolDetails=false must stay disabled")
	}
	if orch.toolTiers.Tier1Cap == nil || *orch.toolTiers.Tier1Cap != 10 ||
		orch.toolTiers.Tier2Cap == nil || *orch.toolTiers.Tier2Cap != 15 {
		t.Errorf("caps must still normalize even when disabled, got %+v", orch.toolTiers)
	}
}
