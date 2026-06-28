package orchestrator

import (
	"context"
	"encoding/json"
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
