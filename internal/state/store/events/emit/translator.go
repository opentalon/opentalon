package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// Preparer-phase pre-processing emit helper. Translator runs inside a
// plugin (today: weaviate-plugin's translateQuery), the plugin returns
// per-call metadata in its RPC response, and the orchestrator emits one
// translation event per entry — same pattern as the retrieval-event
// helpers in preparer.go.
//
// Schema lives in event_types.go (TranslationPayload + TranslationVersion);
// the wire-stable Outcome / Callsite vocabularies are the
// TranslationOutcome* / TranslationCallsite* constants in that file.

// TranslationArgs is the shape the orchestrator passes after unpacking
// one entry from a plugin's prepare-response `translator_events` slice
// (orchestrator.PreparerTranslatorEvent). Field names mirror
// events.TranslationPayload except for the InputText / OutputText pair,
// which the helper itself runs through Excerpt + SanitizeUTF8 to produce
// the InputExcerpt / OutputExcerpt fields that land in the payload.
type TranslationArgs struct {
	Callsite             string
	Outcome              string
	SourceLangDetected   string
	SourceLangConfidence float64
	TargetLang           string
	InputText            string
	OutputText           string
	DurationMS           int64
}

// EmitTranslation writes one translation event. Free-text fields are
// sanitised + capped; Truncated is set when either excerpt was clipped.
// The returned event id is the parent for any downstream events the
// caller chains via WithParent (none today, but consistent with the
// other Emit* helpers' contract).
func EmitTranslation(ctx context.Context, sink Sink, args TranslationArgs) string {
	inputExcerpt, inputTruncated := events.Excerpt(events.SanitizeUTF8(args.InputText))
	outputExcerpt, outputTruncated := events.Excerpt(events.SanitizeUTF8(args.OutputText))
	return send(ctx, sink, events.TypeTranslation, events.TranslationPayload{
		Header:               events.Header{V: events.TranslationVersion},
		Callsite:             args.Callsite,
		Outcome:              args.Outcome,
		SourceLangDetected:   args.SourceLangDetected,
		SourceLangConfidence: args.SourceLangConfidence,
		TargetLang:           args.TargetLang,
		InputExcerpt:         inputExcerpt,
		OutputExcerpt:        outputExcerpt,
		DurationMS:           args.DurationMS,
		Truncated:            inputTruncated || outputTruncated,
	}, args.DurationMS)
}
