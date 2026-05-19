package emit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TestEmitTranslation_RoundTripTranslatedOutcome exercises the happy
// path: a DE→EN translation produces a payload with both excerpts, a
// non-zero confidence, and a non-zero duration.
func TestEmitTranslation_RoundTripTranslatedOutcome(t *testing.T) {
	sink := &fakeSink{}
	EmitTranslation(context.Background(), sink, TranslationArgs{
		Callsite:             events.TranslationCallsiteSearch,
		Outcome:              events.TranslationOutcomeTranslated,
		SourceLangDetected:   "de",
		SourceLangConfidence: 0.93,
		TargetLang:           "en",
		InputText:            "wie viele Items habe ich",
		OutputText:           "how many items do I have",
		DurationMS:           42,
	})

	if len(sink.events) != 1 {
		t.Fatalf("got %d events, want 1", len(sink.events))
	}
	evt := sink.events[0]
	if evt.EventType != events.TypeTranslation {
		t.Errorf("EventType = %q, want %q", evt.EventType, events.TypeTranslation)
	}

	var p events.TranslationPayload
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.V != events.TranslationVersion {
		t.Errorf("V = %d, want %d", p.V, events.TranslationVersion)
	}
	if p.Callsite != events.TranslationCallsiteSearch {
		t.Errorf("Callsite = %q", p.Callsite)
	}
	if p.Outcome != events.TranslationOutcomeTranslated {
		t.Errorf("Outcome = %q", p.Outcome)
	}
	if p.SourceLangDetected != "de" || p.SourceLangConfidence != 0.93 {
		t.Errorf("source lang/confidence = %q/%v", p.SourceLangDetected, p.SourceLangConfidence)
	}
	if p.TargetLang != "en" {
		t.Errorf("TargetLang = %q", p.TargetLang)
	}
	if p.InputExcerpt != "wie viele Items habe ich" {
		t.Errorf("InputExcerpt = %q", p.InputExcerpt)
	}
	if p.OutputExcerpt != "how many items do I have" {
		t.Errorf("OutputExcerpt = %q", p.OutputExcerpt)
	}
	if p.DurationMS != 42 {
		t.Errorf("DurationMS = %d", p.DurationMS)
	}
	if p.Truncated {
		t.Errorf("Truncated = true, want false (inputs were short)")
	}
}

// TestEmitTranslation_SkippedTargetLangOmitsOutput covers the "input is
// already in target language" path: outcome is skipped_target_lang, the
// OutputText typically equals InputText so the JSON carries equal
// excerpts. Confidence reflects the detector's reading of the input.
func TestEmitTranslation_SkippedTargetLangOmitsOutput(t *testing.T) {
	sink := &fakeSink{}
	EmitTranslation(context.Background(), sink, TranslationArgs{
		Callsite:             events.TranslationCallsiteSearch,
		Outcome:              events.TranslationOutcomeSkippedTargetLang,
		SourceLangDetected:   "en",
		SourceLangConfidence: 0.98,
		TargetLang:           "en",
		InputText:            "how many items",
		OutputText:           "how many items",
		DurationMS:           5,
	})

	var p events.TranslationPayload
	if err := json.Unmarshal(sink.events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Outcome != events.TranslationOutcomeSkippedTargetLang {
		t.Errorf("Outcome = %q", p.Outcome)
	}
	if p.InputExcerpt != p.OutputExcerpt {
		t.Errorf("input/output excerpts differ for skipped_target_lang: %q vs %q", p.InputExcerpt, p.OutputExcerpt)
	}
}

// TestEmitTranslation_FailedOutcomeZeroConfidence covers the
// translator-failed path: detect may have run (and recorded a confidence)
// or may have failed outright (confidence stays 0). OutputText is empty.
func TestEmitTranslation_FailedOutcomeZeroConfidence(t *testing.T) {
	sink := &fakeSink{}
	EmitTranslation(context.Background(), sink, TranslationArgs{
		Callsite:             events.TranslationCallsitePrepare,
		Outcome:              events.TranslationOutcomeFailed,
		SourceLangDetected:   "",
		SourceLangConfidence: 0,
		TargetLang:           "en",
		InputText:            "some text the translator could not process",
		OutputText:           "",
		DurationMS:           3000,
	})

	var p events.TranslationPayload
	if err := json.Unmarshal(sink.events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Outcome != events.TranslationOutcomeFailed {
		t.Errorf("Outcome = %q", p.Outcome)
	}
	if p.OutputExcerpt != "" {
		t.Errorf("OutputExcerpt = %q, want empty for failed outcome", p.OutputExcerpt)
	}
	if p.SourceLangConfidence != 0 {
		t.Errorf("SourceLangConfidence = %v, want 0 for failed-detect", p.SourceLangConfidence)
	}
}

// TestEmitTranslation_ExcerptCap exercises the 4 KB clip: an 8 KB input
// must be clipped, and Truncated must be set. The helper also clips the
// output side, so we exercise both in one assertion.
func TestEmitTranslation_ExcerptCap(t *testing.T) {
	big := strings.Repeat("a", 8*1024) // 8 KB ASCII

	sink := &fakeSink{}
	EmitTranslation(context.Background(), sink, TranslationArgs{
		Callsite:   events.TranslationCallsiteSearch,
		Outcome:    events.TranslationOutcomeTranslated,
		TargetLang: "en",
		InputText:  big,
		OutputText: big,
	})

	var p events.TranslationPayload
	if err := json.Unmarshal(sink.events[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(p.InputExcerpt) != events.ExcerptCap {
		t.Errorf("InputExcerpt len = %d, want %d (capped)", len(p.InputExcerpt), events.ExcerptCap)
	}
	if len(p.OutputExcerpt) != events.ExcerptCap {
		t.Errorf("OutputExcerpt len = %d, want %d (capped)", len(p.OutputExcerpt), events.ExcerptCap)
	}
	if !p.Truncated {
		t.Errorf("Truncated = false, want true (both fields were clipped)")
	}
}
