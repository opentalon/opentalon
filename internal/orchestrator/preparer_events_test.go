package orchestrator

import (
	"context"
	"encoding/json"
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
// The tier counts are always zero now that tool discovery is the
// registry-sourced catalog rather than a per-turn tier decision.
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

// TestEmitPreparerTranslations covers the three contract clauses of the
// translation-event fan-out (opentalon/opentalon#256):
//   - one event per slice entry, in slice order
//   - no event when the slice is empty/nil
//   - field-for-field pass-through of every PreparerTranslatorEvent into
//     the emitted TranslationPayload
func TestEmitPreparerTranslations(t *testing.T) {
	t.Run("no events when slice is empty", func(t *testing.T) {
		sink := &recordingEventSink{}
		o := &Orchestrator{eventSink: sink}
		o.emitPreparerTranslations(context.Background(), preparerResponse{})
		if got := len(sink.snapshot()); got != 0 {
			t.Fatalf("emitted %d events for empty slice, want 0", got)
		}
	})

	t.Run("one event per slice entry, field pass-through", func(t *testing.T) {
		sink := &recordingEventSink{}
		o := &Orchestrator{eventSink: sink}
		o.emitPreparerTranslations(context.Background(), preparerResponse{
			TranslatorEvents: []PreparerTranslatorEvent{
				{
					Callsite:             events.TranslationCallsitePrepare,
					Outcome:              events.TranslationOutcomeTranslated,
					SourceLangDetected:   "de",
					SourceLangConfidence: 0.93,
					TargetLang:           "en",
					InputText:            "wieviele Items habe ich",
					OutputText:           "how many items do I have",
					DurationMS:           42,
				},
				{
					Callsite:   events.TranslationCallsiteSearch,
					Outcome:    events.TranslationOutcomeSkippedTargetLang,
					TargetLang: "en",
					InputText:  "list available tools",
					OutputText: "list available tools",
					DurationMS: 5,
				},
			},
		})

		evts := sink.snapshot()
		if len(evts) != 2 {
			t.Fatalf("got %d events, want 2", len(evts))
		}
		for _, e := range evts {
			if e.EventType != events.TypeTranslation {
				t.Errorf("EventType = %q, want %q", e.EventType, events.TypeTranslation)
			}
		}

		var first events.TranslationPayload
		if err := json.Unmarshal(evts[0].Payload, &first); err != nil {
			t.Fatalf("unmarshal first: %v", err)
		}
		if first.Callsite != events.TranslationCallsitePrepare ||
			first.Outcome != events.TranslationOutcomeTranslated ||
			first.SourceLangDetected != "de" ||
			first.SourceLangConfidence != 0.93 ||
			first.TargetLang != "en" ||
			first.DurationMS != 42 {
			t.Errorf("first payload mismatch: %+v", first)
		}
		if first.InputExcerpt != "wieviele Items habe ich" || first.OutputExcerpt != "how many items do I have" {
			t.Errorf("first excerpts mismatch: in=%q out=%q", first.InputExcerpt, first.OutputExcerpt)
		}

		var second events.TranslationPayload
		if err := json.Unmarshal(evts[1].Payload, &second); err != nil {
			t.Fatalf("unmarshal second: %v", err)
		}
		if second.Outcome != events.TranslationOutcomeSkippedTargetLang {
			t.Errorf("second.Outcome = %q", second.Outcome)
		}
		if second.InputExcerpt != second.OutputExcerpt {
			t.Errorf("skipped_target_lang: in/out should be equal; in=%q out=%q",
				second.InputExcerpt, second.OutputExcerpt)
		}
	})

	t.Run("nil eventSink is a no-op", func(t *testing.T) {
		o := &Orchestrator{eventSink: nil}
		// Should not panic.
		o.emitPreparerTranslations(context.Background(), preparerResponse{
			TranslatorEvents: []PreparerTranslatorEvent{{Callsite: "x"}},
		})
	})
}
