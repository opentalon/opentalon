package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// ScoreComputedArgs is the rubric-driven session-quality score, written
// by the (separate) score worker at session-end. Reasoning is free text
// from the scoring LLM; numeric Score plus RubricVersion are the
// analytics columns.
type ScoreComputedArgs struct {
	Score         float64
	RubricVersion string
	Reasoning     string
}

// EmitScoreComputed writes one score_computed event.
func EmitScoreComputed(ctx context.Context, sink Sink, args ScoreComputedArgs) {
	send(ctx, sink, events.TypeScoreComputed, events.ScoreComputedPayload{
		Header:        events.Header{V: events.ScoreComputedVersion},
		Score:         args.Score,
		RubricVersion: args.RubricVersion,
		Reasoning:     events.SanitizeUTF8(args.Reasoning),
	}, 0)
}
