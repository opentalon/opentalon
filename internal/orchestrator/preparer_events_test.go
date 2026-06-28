package orchestrator

import (
	"reflect"
	"testing"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TestTurnStartRefsFromAggregate pins the pull-only contract: knowledge
// is never auto-injected, so turn_start's Pillar-C injected-knowledge
// back-references are always empty and the tier counts always zero —
// regardless of what the aggregate retrieved (the retrieved candidates
// surface on preparer_decision instead).
func TestTurnStartRefsFromAggregate(t *testing.T) {
	candA := KnowledgeCandidate{ArticleID: "kb_a", ContentSHA256: "sha_a"}
	candB := KnowledgeCandidate{ArticleID: "kb_b", ContentSHA256: "sha_b"}

	tests := []struct {
		name string
		agg  preparerAggregate
	}{
		{
			name: "retrieved candidates — still no injected refs (pull-only)",
			agg:  preparerAggregate{Knowledge: []KnowledgeCandidate{candA, candB}},
		},
		{
			name: "no preparer ran — empty aggregate yields no refs",
			agg:  preparerAggregate{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs, tier1, tier3 := turnStartRefsFromAggregate(tc.agg)
			if !reflect.DeepEqual(refs, []events.KnowledgeRef(nil)) {
				t.Errorf("refs = %+v, want nil", refs)
			}
			if tier1 != 0 {
				t.Errorf("tier1Count = %d, want 0", tier1)
			}
			if tier3 != 0 {
				t.Errorf("tier3Count = %d, want 0", tier3)
			}
		})
	}
}
