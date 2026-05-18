package orchestrator

import (
	"reflect"
	"sort"
	"testing"

	"github.com/opentalon/opentalon/internal/state"
)

func defaultTierCfg() ToolTiersConfig {
	return ToolTiersConfig{
		Enabled:  true,
		Tier1Cap: 3,
		Tier2Cap: 2,
	}
}

func tc(name string, score float64) ToolCandidate {
	return ToolCandidate{ToolName: name, Score: score}
}

func TestApplyToolTierDecision_NewCandidatesFillTier1UnderCap(t *testing.T) {
	candidates := []ToolCandidate{tc("a.x", 0.9), tc("a.y", 0.7)}
	available := []string{"a.x", "a.y", "a.z"}
	got := applyToolTierDecision(candidates, available, nil, nil, state.InjectionState{}, defaultTierCfg(), 1)

	if !reflect.DeepEqual(got.Tier1, []string{"a.x", "a.y"}) {
		t.Errorf("Tier1 = %v, want [a.x a.y]", got.Tier1)
	}
	if !reflect.DeepEqual(got.Tier1New, []string{"a.x", "a.y"}) {
		t.Errorf("Tier1New = %v, want both freshly entering", got.Tier1New)
	}
	if len(got.Tier1Carried) != 0 {
		t.Errorf("Tier1Carried must be empty on first turn, got %v", got.Tier1Carried)
	}
	if !reflect.DeepEqual(got.Tier3, []string{"a.z"}) {
		t.Errorf("Tier3 = %v, want [a.z]", got.Tier3)
	}
	if got.Tier1SizeAfter != 2 || got.Tier1Cap != 3 {
		t.Errorf("Tier1SizeAfter/Tier1Cap = %d/%d, want 2/3", got.Tier1SizeAfter, got.Tier1Cap)
	}
}

func TestApplyToolTierDecision_AlwaysIncludeLandsInTier0(t *testing.T) {
	candidates := []ToolCandidate{tc("a.x", 0.9)}
	available := []string{"a.x", "meta.help"}
	always := []string{"meta.help"}
	got := applyToolTierDecision(candidates, available, always, nil, state.InjectionState{}, defaultTierCfg(), 1)

	if !reflect.DeepEqual(got.Tier0, []string{"meta.help"}) {
		t.Errorf("Tier0 = %v, want [meta.help]", got.Tier0)
	}
	if !reflect.DeepEqual(got.Tier1, []string{"a.x"}) {
		t.Errorf("Tier1 = %v, want [a.x] (always-include excluded)", got.Tier1)
	}
	if len(got.Tier3) != 0 {
		t.Errorf("Tier3 must be empty (everything accounted for), got %v", got.Tier3)
	}
}

func TestApplyToolTierDecision_LRUEvictsOldestWhenCapExceeded(t *testing.T) {
	// Prior turn left tier1=[a.x rank=1, a.y rank=2, a.z rank=3]. New
	// candidates push two newcomers in at rank=4. Cap=3 means the two
	// lowest-rank Tier-1 entries (a.x rank=1, a.y rank=2) get evicted.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.x", Tier: state.KnownToolTier1, LRURank: 1},
		{ToolName: "a.y", Tier: state.KnownToolTier1, LRURank: 2},
		{ToolName: "a.z", Tier: state.KnownToolTier1, LRURank: 3},
	}}
	candidates := []ToolCandidate{tc("a.new1", 0.9), tc("a.new2", 0.8)}
	available := []string{"a.x", "a.y", "a.z", "a.new1", "a.new2"}
	got := applyToolTierDecision(candidates, available, nil, nil, prior, defaultTierCfg(), 4)

	wantTier1 := []string{"a.new1", "a.new2", "a.z"}
	sort.Strings(wantTier1)
	if !reflect.DeepEqual(got.Tier1, wantTier1) {
		t.Errorf("Tier1 = %v, want %v", got.Tier1, wantTier1)
	}
	wantEvicted := []string{"a.x", "a.y"}
	if !reflect.DeepEqual(got.Tier1EvictedToTier3, wantEvicted) {
		t.Errorf("Tier1EvictedToTier3 = %v, want %v", got.Tier1EvictedToTier3, wantEvicted)
	}
	if !reflect.DeepEqual(got.Tier1Carried, []string{"a.z"}) {
		t.Errorf("Tier1Carried = %v, want [a.z]", got.Tier1Carried)
	}
	if !reflect.DeepEqual(got.Tier1New, []string{"a.new1", "a.new2"}) {
		t.Errorf("Tier1New = %v, want [a.new1 a.new2]", got.Tier1New)
	}
}

func TestApplyToolTierDecision_DemotedAreEvictionPreferred(t *testing.T) {
	// a.demoted has rank=10 (highest) but Demoted=true, so it should
	// lose Tier 1 to non-demoted tools even at lower rank.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.demoted", Tier: state.KnownToolTier1, LRURank: 10, Demoted: true},
		{ToolName: "a.normal", Tier: state.KnownToolTier1, LRURank: 1},
	}}
	candidates := []ToolCandidate{tc("a.new1", 0.9), tc("a.new2", 0.8)}
	available := []string{"a.demoted", "a.normal", "a.new1", "a.new2"}
	got := applyToolTierDecision(candidates, available, nil, nil, prior, defaultTierCfg(), 4)

	for _, name := range got.Tier1 {
		if name == "a.demoted" {
			t.Errorf("a.demoted should be evicted as preferred candidate, but stayed in Tier1: %v", got.Tier1)
		}
	}
}

func TestApplyToolTierDecision_Tier2FromCandidatesAboveCap(t *testing.T) {
	cfg := ToolTiersConfig{Enabled: true, Tier1Cap: 2, Tier2Cap: 2}
	candidates := []ToolCandidate{
		tc("a.1", 0.9), tc("a.2", 0.8), // Tier 1 (cap=2)
		tc("a.3", 0.7), tc("a.4", 0.6), // Tier 2 (cap=2)
		tc("a.5", 0.5), // overflow → Tier 3
	}
	available := []string{"a.1", "a.2", "a.3", "a.4", "a.5"}
	got := applyToolTierDecision(candidates, available, nil, nil, state.InjectionState{}, cfg, 1)

	wantT1 := []string{"a.1", "a.2"}
	if !reflect.DeepEqual(got.Tier1, wantT1) {
		t.Errorf("Tier1 = %v, want %v", got.Tier1, wantT1)
	}
	wantT2 := []string{"a.3", "a.4"}
	if !reflect.DeepEqual(got.Tier2, wantT2) {
		t.Errorf("Tier2 = %v, want %v", got.Tier2, wantT2)
	}
	if !reflect.DeepEqual(got.Tier3, []string{"a.5"}) {
		t.Errorf("Tier3 = %v, want [a.5]", got.Tier3)
	}
	if got.Tier2Cap != cfg.Tier2Cap {
		t.Errorf("Tier2Cap snapshot = %d, want %d (cfg.Tier2Cap)", got.Tier2Cap, cfg.Tier2Cap)
	}
}

func TestApplyToolTierDecision_PromotedJoinsTier1(t *testing.T) {
	// promoted tools enter the Tier-1 pool at currentTurn rank — should
	// land in Tier 1 ahead of older candidates.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.old", Tier: state.KnownToolTier1, LRURank: 1},
	}}
	candidates := []ToolCandidate{tc("a.cand", 0.9)}
	promoted := []string{"a.promo"}
	available := []string{"a.old", "a.cand", "a.promo"}
	got := applyToolTierDecision(candidates, available, nil, promoted, prior, defaultTierCfg(), 5)

	if !contains(got.Tier1, "a.promo") {
		t.Errorf("a.promo should be in Tier1, got %v", got.Tier1)
	}
	if !reflect.DeepEqual(got.PromotedViaGetToolDetails, []string{"a.promo"}) {
		t.Errorf("PromotedViaGetToolDetails = %v, want [a.promo]", got.PromotedViaGetToolDetails)
	}
}

func TestApplyToolTierDecision_Tier3IsResidual(t *testing.T) {
	candidates := []ToolCandidate{tc("a.1", 0.9)}
	available := []string{"a.1", "a.2", "a.3", "a.4"}
	got := applyToolTierDecision(candidates, available, nil, nil, state.InjectionState{}, defaultTierCfg(), 1)

	wantT3 := []string{"a.2", "a.3", "a.4"}
	if !reflect.DeepEqual(got.Tier3, wantT3) {
		t.Errorf("Tier3 = %v, want %v", got.Tier3, wantT3)
	}
}

func TestApplyToolTierDecision_UpdatedStatePreservesKnownKnowledge(t *testing.T) {
	prior := state.InjectionState{
		KnownKnowledge: []state.KnownKnowledgeEntry{
			{ArticleID: "kb_a", ContentSHA256: "abc", FirstInjectedTurn: 1},
		},
	}
	got := applyToolTierDecision(nil, []string{"a.x"}, nil, nil, prior, defaultTierCfg(), 1)
	if !reflect.DeepEqual(got.UpdatedState.KnownKnowledge, prior.KnownKnowledge) {
		t.Errorf("KnownKnowledge clobbered: %v vs %v", got.UpdatedState.KnownKnowledge, prior.KnownKnowledge)
	}
}

func TestApplyToolTierDecision_UpdatedStatePersistsDemotedFlag(t *testing.T) {
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.x", Tier: state.KnownToolTier1, LRURank: 1, Demoted: true},
	}}
	candidates := []ToolCandidate{tc("a.x", 0.9)}
	got := applyToolTierDecision(candidates, []string{"a.x"}, nil, nil, prior, defaultTierCfg(), 5)
	found := false
	for _, kt := range got.UpdatedState.KnownTools {
		if kt.ToolName == "a.x" {
			found = true
			if !kt.Demoted {
				t.Errorf("a.x must remain Demoted=true after RAG match")
			}
		}
	}
	if !found {
		t.Errorf("a.x missing from updated KnownTools")
	}
}

func TestApplyToolTierDecision_UpdatedStateDropsRemovedTools(t *testing.T) {
	// A tool that used to be in KnownTools but is no longer in
	// availableTools (plugin removed, profile changed) should drop out.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.gone", Tier: state.KnownToolTier1, LRURank: 5},
		{ToolName: "a.here", Tier: state.KnownToolTier3, LRURank: 3},
	}}
	got := applyToolTierDecision(nil, []string{"a.here"}, nil, nil, prior, defaultTierCfg(), 6)
	for _, kt := range got.UpdatedState.KnownTools {
		if kt.ToolName == "a.gone" {
			t.Errorf("a.gone should drop out of KnownTools, got %v", got.UpdatedState.KnownTools)
		}
	}
}

func TestApplyToolTierDecision_AlwaysIncludeBeatsRAGScore(t *testing.T) {
	// A tool marked always_include should land in Tier 0, NEVER in
	// Tier 1, even when also a high-score RAG candidate.
	candidates := []ToolCandidate{tc("meta.help", 0.95), tc("a.x", 0.4)}
	available := []string{"a.x", "meta.help"}
	always := []string{"meta.help"}
	got := applyToolTierDecision(candidates, available, always, nil, state.InjectionState{}, defaultTierCfg(), 1)

	if !contains(got.Tier0, "meta.help") {
		t.Errorf("meta.help must be in Tier0, got Tier0=%v", got.Tier0)
	}
	if contains(got.Tier1, "meta.help") {
		t.Errorf("meta.help must NOT be in Tier1, got Tier1=%v", got.Tier1)
	}
}

func TestApplyToolTierDecision_BumpsLRURankOnRAGMatch(t *testing.T) {
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.x", Tier: state.KnownToolTier1, LRURank: 2},
	}}
	candidates := []ToolCandidate{tc("a.x", 0.9)}
	got := applyToolTierDecision(candidates, []string{"a.x"}, nil, nil, prior, defaultTierCfg(), 7)
	for _, kt := range got.UpdatedState.KnownTools {
		if kt.ToolName == "a.x" && kt.LRURank != 7 {
			t.Errorf("a.x LRURank = %d, want 7 after RAG match in turn 7", kt.LRURank)
		}
	}
}

func TestApplyToolTierDecision_EmptyCandidatesAndAvailable(t *testing.T) {
	got := applyToolTierDecision(nil, nil, nil, nil, state.InjectionState{}, defaultTierCfg(), 1)
	if len(got.Tier0) != 0 || len(got.Tier1) != 0 || len(got.Tier2) != 0 || len(got.Tier3) != 0 {
		t.Errorf("empty inputs must yield empty tiers, got %+v", got)
	}
	if got.Tier1Cap != 3 {
		t.Errorf("Tier1Cap snapshot = %d, want 3", got.Tier1Cap)
	}
}

func TestApplyToolTierDecision_LegacyEvictionOnlyWhenWasTier1(t *testing.T) {
	// A pool overflow that was NOT tier="tier1" in prior state should
	// not appear in Tier1EvictedToTier3 (it never lost Tier 1 status).
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.was_t3", Tier: state.KnownToolTier3, LRURank: 1},
	}}
	// a.was_t3 is NOT a current candidate, NOT promoted, so it never
	// even enters the pool. It belongs to Tier 3 directly.
	candidates := []ToolCandidate{tc("a.1", 0.9), tc("a.2", 0.8), tc("a.3", 0.7), tc("a.4", 0.6)}
	available := []string{"a.1", "a.2", "a.3", "a.4", "a.was_t3"}
	got := applyToolTierDecision(candidates, available, nil, nil, prior, defaultTierCfg(), 1)
	// Tier1Cap=3, so a.4 overflows. But a.4 was never tier="tier1",
	// so it's not in Tier1EvictedToTier3.
	if contains(got.Tier1EvictedToTier3, "a.4") {
		t.Errorf("a.4 should not be in Tier1EvictedToTier3 (never was Tier 1)")
	}
}

func TestApplyToolTierDecision_Tier1NewIncludesPromotionsFromTier2(t *testing.T) {
	// A tool that was Tier 2 last turn (no tier="tier1" entry in
	// known_tools) and is a top-ranked candidate this turn lands in
	// Tier 1. It should appear as Tier1New, not Tier1Carried.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.moved_up", Tier: state.KnownToolTier2, LRURank: 1},
	}}
	candidates := []ToolCandidate{tc("a.moved_up", 0.99)}
	got := applyToolTierDecision(candidates, []string{"a.moved_up"}, nil, nil, prior, defaultTierCfg(), 5)
	if !contains(got.Tier1, "a.moved_up") {
		t.Errorf("a.moved_up should be in Tier1, got %v", got.Tier1)
	}
	if !contains(got.Tier1New, "a.moved_up") {
		t.Errorf("a.moved_up should be Tier1New (was Tier 2 prior), got Tier1New=%v Tier1Carried=%v",
			got.Tier1New, got.Tier1Carried)
	}
}

func TestBuildToolsBlock_FallbackWhenNoTierDecision(t *testing.T) {
	agg := preparerAggregate{
		Tools: []ToolCandidate{tc("a.x", 0.9), tc("a.y", 0.8)},
	}
	block := buildToolsBlock(agg)
	want := []string{"a.x", "a.y"}
	if !reflect.DeepEqual(block.Tier1New, want) {
		t.Errorf("fallback Tier1New = %v, want %v", block.Tier1New, want)
	}
	if block.Tier1SizeAfter != 0 || block.Tier1Cap != 0 || block.Tier3TotalVisible != 0 {
		t.Errorf("fallback must leave tier-aware fields zero, got %+v", block)
	}
}

func TestBuildToolsBlock_FromTierDecision(t *testing.T) {
	d := toolTierDecision{
		Tier0:                     []string{"meta.help"},
		Tier1New:                  []string{"a.x"},
		Tier1Carried:              []string{"a.y"},
		Tier1EvictedToTier3:       []string{"a.z"},
		Tier1SizeAfter:            2,
		Tier1Cap:                  10,
		Tier2:                     []string{"a.t2a", "a.t2b"},
		Tier2Cap:                  15,
		Tier3:                     []string{"a.lots", "a.more"},
		PromotedViaGetToolDetails: []string{"a.promo"},
	}
	agg := preparerAggregate{ToolTier: &d}
	block := buildToolsBlock(agg)

	if block.Tier0Count != 1 {
		t.Errorf("Tier0Count = %d, want 1", block.Tier0Count)
	}
	if !reflect.DeepEqual(block.Tier1New, d.Tier1New) || !reflect.DeepEqual(block.Tier1Carried, d.Tier1Carried) {
		t.Errorf("tier1 subset slices not threaded through: %+v", block)
	}
	// Tier 2 fields close the prior observability gap: without them,
	// consumers had no way to tell from the event log whether Tier 2
	// was being populated, even though Tier 2 actually drives the
	// "name + 1-line summary" block in the rendered system prompt.
	if !reflect.DeepEqual(block.Tier2Tools, []string{"a.t2a", "a.t2b"}) {
		t.Errorf("Tier2Tools = %v, want [a.t2a a.t2b]", block.Tier2Tools)
	}
	if block.Tier2SizeAfter != 2 {
		t.Errorf("Tier2SizeAfter = %d, want 2", block.Tier2SizeAfter)
	}
	if block.Tier2Cap != 15 {
		t.Errorf("Tier2Cap = %d, want 15", block.Tier2Cap)
	}
	if block.Tier3TotalVisible != 2 {
		t.Errorf("Tier3TotalVisible = %d, want 2", block.Tier3TotalVisible)
	}
	if !reflect.DeepEqual(block.PromotedViaGetToolDetails, []string{"a.promo"}) {
		t.Errorf("PromotedViaGetToolDetails not threaded through, got %v", block.PromotedViaGetToolDetails)
	}
}

func TestApplyToolTierDecision_PromotedAlreadyTier1StaysTier1Carried(t *testing.T) {
	// A tool that was tier="tier1" in prior state AND is promoted via
	// get_tool_details this turn should: (a) end up in Tier 1, and
	// (b) classify as Tier1Carried (was tier="tier1" before, still is)
	// rather than Tier1New. Otherwise a re-promoted carry-over would
	// look like a freshly-promoted tool in the event payload.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "a.kept", Tier: state.KnownToolTier1, LRURank: 3},
	}}
	promoted := []string{"a.kept"}
	got := applyToolTierDecision(nil, []string{"a.kept"}, nil, promoted, prior, defaultTierCfg(), 7)

	if !contains(got.Tier1, "a.kept") {
		t.Errorf("a.kept must stay in Tier1 after promote, got %v", got.Tier1)
	}
	if contains(got.Tier1New, "a.kept") {
		t.Errorf("a.kept must NOT be Tier1New (was tier1 before), got %v", got.Tier1New)
	}
	if !contains(got.Tier1Carried, "a.kept") {
		t.Errorf("a.kept must be Tier1Carried, got Tier1Carried=%v Tier1New=%v",
			got.Tier1Carried, got.Tier1New)
	}
}

func TestApplyToolTierDecision_SortStableOnEqualRank(t *testing.T) {
	// Two non-demoted tools with the same LRURank must sort by name
	// (alphabetical) so the event payload is reproducible across
	// orchestrator restarts and the tier diff stays auditable.
	prior := state.InjectionState{KnownTools: []state.KnownToolEntry{
		{ToolName: "z.last", Tier: state.KnownToolTier1, LRURank: 5},
		{ToolName: "a.first", Tier: state.KnownToolTier1, LRURank: 5},
		{ToolName: "m.middle", Tier: state.KnownToolTier1, LRURank: 5},
	}}
	available := []string{"a.first", "m.middle", "z.last"}
	got := applyToolTierDecision(nil, available, nil, nil, prior, defaultTierCfg(), 6)
	wantTier1 := []string{"a.first", "m.middle", "z.last"}
	if !reflect.DeepEqual(got.Tier1, wantTier1) {
		t.Errorf("Tier1 = %v, want alphabetical %v", got.Tier1, wantTier1)
	}
}

func TestApplyToolTierDecision_Tier1CapLargerThanAvailable(t *testing.T) {
	// cfg.Tier1Cap=100, only 2 available — no panic, Tier 1 holds
	// what's there. Guards against accidental over-allocation in the
	// pool-slicing path.
	cfg := ToolTiersConfig{Enabled: true, Tier1Cap: 100, Tier2Cap: 5}
	candidates := []ToolCandidate{tc("a.x", 0.9), tc("a.y", 0.8)}
	got := applyToolTierDecision(candidates, []string{"a.x", "a.y"}, nil, nil, state.InjectionState{}, cfg, 1)
	if len(got.Tier1) != 2 {
		t.Errorf("Tier1 len = %d, want 2 (cap > pool size)", len(got.Tier1))
	}
	if got.Tier1Cap != 100 {
		t.Errorf("Tier1Cap snapshot = %d, want 100", got.Tier1Cap)
	}
}

func TestApplyToolTierDecision_Tier2CapZeroSendsAllToTier3(t *testing.T) {
	// cfg.Tier2Cap=0 means no Tier 2 entries — every below-Tier-1
	// candidate falls straight through to Tier 3. Legal edge case
	// since an operator might want to suppress the system-prompt
	// "name + 1-liner" block entirely without disabling tier logic.
	cfg := ToolTiersConfig{Enabled: true, Tier1Cap: 1, Tier2Cap: 0}
	candidates := []ToolCandidate{tc("a.1", 0.9), tc("a.2", 0.8), tc("a.3", 0.7)}
	got := applyToolTierDecision(candidates, []string{"a.1", "a.2", "a.3"}, nil, nil, state.InjectionState{}, cfg, 1)
	if len(got.Tier2) != 0 {
		t.Errorf("Tier2Cap=0 must produce empty Tier2, got %v", got.Tier2)
	}
	wantT3 := []string{"a.2", "a.3"}
	if !reflect.DeepEqual(got.Tier3, wantT3) {
		t.Errorf("Tier3 = %v, want %v", got.Tier3, wantT3)
	}
}

// contains is a tiny test helper. The orchestrator package already
// has `slices.Contains` available via Go 1.21+; the local helper is
// kept for readability of test-only assertions.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
