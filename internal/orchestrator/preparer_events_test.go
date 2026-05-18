package orchestrator

import (
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TestTurnStartRefsFromAggregate covers the four input modes the
// preparer aggregate can be in by the time turn_start needs its
// Pillar-C back-references:
//
//   - full dedup mode (KnowledgeDedup set) → InjectedKnowledge from the decision's Injected slice
//   - instrumentation_only mode (no dedup, no legacy plugin) → every retrieved candidate
//   - legacy_fallback mode (any legacy plugin returned a pre-rendered block) → empty (no structured handle)
//   - no preparer ran (aggregate empty) → nothing
//
// Plus the tier-count derivation: zero when ToolTier is nil (Phase 4
// off), len(Tier1) / len(Tier3) when set.
func TestTurnStartRefsFromAggregate(t *testing.T) {
	candA := KnowledgeCandidate{ArticleID: "kb_a", ContentSHA256: "sha_a"}
	candB := KnowledgeCandidate{ArticleID: "kb_b", ContentSHA256: "sha_b"}
	candC := KnowledgeCandidate{ArticleID: "kb_c", ContentSHA256: "sha_c"}

	tests := []struct {
		name           string
		agg            preparerAggregate
		wantRefs       []events.KnowledgeRef
		wantTier1Count int
		wantTier3Count int
	}{
		{
			name: "full dedup mode — refs from KnowledgeDedup.Injected",
			agg: preparerAggregate{
				Knowledge: []KnowledgeCandidate{candA, candB, candC}, // retrieved set
				KnowledgeDedup: &knowledgeDedupDecision{
					Injected: []KnowledgeCandidate{candA, candC}, // deduped: B was already known
				},
			},
			wantRefs: []events.KnowledgeRef{
				{ArticleID: "kb_a", ContentSHA256: "sha_a"},
				{ArticleID: "kb_c", ContentSHA256: "sha_c"},
			},
		},
		{
			name: "instrumentation_only mode — refs from every retrieved candidate",
			agg: preparerAggregate{
				Knowledge: []KnowledgeCandidate{candA, candB},
				// KnowledgeDedup nil, LegacyKnowledgePlugins empty → Phase 2
			},
			wantRefs: []events.KnowledgeRef{
				{ArticleID: "kb_a", ContentSHA256: "sha_a"},
				{ArticleID: "kb_b", ContentSHA256: "sha_b"},
			},
		},
		{
			name: "legacy_fallback mode — refs empty (no structured handle for the rendered block)",
			agg: preparerAggregate{
				Knowledge:              []KnowledgeCandidate{candA},
				LegacyKnowledgePlugins: []string{"legacy_plugin"},
			},
			wantRefs: nil,
		},
		{
			name: "no preparer ran — empty aggregate yields no refs",
			agg:  preparerAggregate{},
		},
		{
			name: "tier counts surface when ToolTier is set",
			agg: preparerAggregate{
				ToolTier: &toolTierDecision{
					Tier1: []string{"t1", "t2", "t3"},
					Tier3: []string{"t4", "t5"},
				},
			},
			wantTier1Count: 3,
			wantTier3Count: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs, tier1, tier3 := turnStartRefsFromAggregate(tc.agg)
			if !reflect.DeepEqual(refs, tc.wantRefs) {
				t.Errorf("refs = %+v, want %+v", refs, tc.wantRefs)
			}
			if tier1 != tc.wantTier1Count {
				t.Errorf("tier1Count = %d, want %d", tier1, tc.wantTier1Count)
			}
			if tier3 != tc.wantTier3Count {
				t.Errorf("tier3Count = %d, want %d", tier3, tc.wantTier3Count)
			}
		})
	}
}
