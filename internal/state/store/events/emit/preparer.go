package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// Preparer-phase emit helpers (RFC #249). One file groups the six events
// that fire from (or describe) the orchestrator's preparer pass:
//
//   - knowledge_retrieval / glossary_retrieval / tool_retrieval — one
//     per RAG corpus searched on the current user turn, parented to the
//     user_message event so a session timeline reads as a clean tree.
//   - preparer_decision — the composite outcome (which candidates
//     injected, which tools tier-promoted, etc.). Always emitted exactly
//     once per turn; the Mode field discriminates Phase-2 instrumentation
//     from Phase-3+ dedup behaviour.
//   - drift_detected — emitted at the start of a preparer pass when the
//     session's known-knowledge state and the actual visible message
//     stream disagree (Phase 3+ only; helper exists now so the wiring
//     site can land alongside the dedup logic without a schema bump).
//   - messages_truncated — emitted when the sliding-window cutter drops
//     messages from the LLM-bound message slice.
//
// EmitToolRetrieval used to live in planner.go but moves here because
// tool retrieval is preparer-phase scope, not planner scope. Callers
// keep the same function signature on this side of the move; the helper
// gains search_text_source / min_score / latency_ms args to match the
// dimensions the new knowledge / glossary retrievals carry.

// KnowledgeRetrievalArgs carries the result of a knowledge-base RAG
// search. Hits stay in returned-rank order so consumers can compute
// position-based metrics without re-sorting.
type KnowledgeRetrievalArgs struct {
	Query            string
	SearchTextSource string // events.SearchTextSource* — empty when unspecified
	TopK             int
	MinScore         float64
	LatencyMS        int64
	Hits             []events.KnowledgeRetrievalHit
}

// EmitKnowledgeRetrieval writes one knowledge_retrieval event. Hits are
// sanitized per-element on the free-text fields (Title, Source) since
// RAG-plugin output is upstream of Postgres' UTF-8 constraint and we
// can't trust the corpus byte-for-byte.
func EmitKnowledgeRetrieval(ctx context.Context, sink Sink, args KnowledgeRetrievalArgs) string {
	return send(ctx, sink, events.TypeKnowledgeRetrieval, events.KnowledgeRetrievalPayload{
		Header:           events.Header{V: events.KnowledgeRetrievalVersion},
		Query:            events.SanitizeUTF8(args.Query),
		SearchTextSource: args.SearchTextSource,
		TopK:             args.TopK,
		MinScore:         args.MinScore,
		LatencyMS:        args.LatencyMS,
		Hits:             sanitizeKnowledgeHits(args.Hits),
	}, args.LatencyMS)
}

// sanitizeKnowledgeHits returns a copy of hits with free-text fields
// scrubbed of invalid UTF-8. The input slice is not mutated.
func sanitizeKnowledgeHits(hits []events.KnowledgeRetrievalHit) []events.KnowledgeRetrievalHit {
	if len(hits) == 0 {
		return hits
	}
	out := make([]events.KnowledgeRetrievalHit, len(hits))
	for i, h := range hits {
		h.Title = events.SanitizeUTF8(h.Title)
		h.Source = events.SanitizeUTF8(h.Source)
		out[i] = h
	}
	return out
}

// GlossaryRetrievalArgs mirrors KnowledgeRetrievalArgs; the only
// difference is the per-hit "term" instead of "title".
type GlossaryRetrievalArgs struct {
	Query            string
	SearchTextSource string
	TopK             int
	MinScore         float64
	LatencyMS        int64
	Hits             []events.GlossaryRetrievalHit
}

// EmitGlossaryRetrieval writes one glossary_retrieval event. Hits are
// sanitized per-element analogous to EmitKnowledgeRetrieval.
func EmitGlossaryRetrieval(ctx context.Context, sink Sink, args GlossaryRetrievalArgs) string {
	return send(ctx, sink, events.TypeGlossaryRetrieval, events.GlossaryRetrievalPayload{
		Header:           events.Header{V: events.GlossaryRetrievalVersion},
		Query:            events.SanitizeUTF8(args.Query),
		SearchTextSource: args.SearchTextSource,
		TopK:             args.TopK,
		MinScore:         args.MinScore,
		LatencyMS:        args.LatencyMS,
		Hits:             sanitizeGlossaryHits(args.Hits),
	}, args.LatencyMS)
}

func sanitizeGlossaryHits(hits []events.GlossaryRetrievalHit) []events.GlossaryRetrievalHit {
	if len(hits) == 0 {
		return hits
	}
	out := make([]events.GlossaryRetrievalHit, len(hits))
	for i, h := range hits {
		h.Term = events.SanitizeUTF8(h.Term)
		h.Source = events.SanitizeUTF8(h.Source)
		out[i] = h
	}
	return out
}

// ToolRetrievalArgs carries the result of a tool RAG search.
type ToolRetrievalArgs struct {
	Query            string
	SearchTextSource string
	TopK             int
	MinScore         float64
	LatencyMS        int64
	Hits             []events.ToolRetrievalHit
}

// EmitToolRetrieval writes one tool_retrieval event.
//
// The type exists since migration 009 but was never emitted before; per
// the RFC #249 Phase 2 rollout the helper formally lives here from now
// on (the trivial reachable-only-via-planner.go version is gone).
func EmitToolRetrieval(ctx context.Context, sink Sink, args ToolRetrievalArgs) string {
	return send(ctx, sink, events.TypeToolRetrieval, events.ToolRetrievalPayload{
		Header:           events.Header{V: events.ToolRetrievalVersion},
		Query:            events.SanitizeUTF8(args.Query),
		SearchTextSource: args.SearchTextSource,
		TopK:             args.TopK,
		MinScore:         args.MinScore,
		LatencyMS:        args.LatencyMS,
		Hits:             args.Hits,
	}, args.LatencyMS)
}

// PreparerDecisionArgs is the composite outcome the orchestrator landed
// on for one preparer pass. Mode is one of the
// events.PreparerDecisionMode* constants; callers in Phase 2 always pass
// PreparerDecisionModeInstrumentationOnly, with Knowledge.Injected
// listing all candidates and the dedup-state-related buckets empty.
//
// Phase 3+ callers flip Mode to "full" and start populating
// Knowledge.SkippedKnown / ScoreOverridesApplied etc.; Phase 4 wires up
// the Tools block. The helper has no opinion about the mode — it just
// forwards what the caller assembles.
type PreparerDecisionArgs struct {
	Mode      string
	Knowledge events.PreparerDecisionKnowledgeBlock
	Tools     events.PreparerDecisionToolsBlock
}

// EmitPreparerDecision writes one preparer_decision event.
func EmitPreparerDecision(ctx context.Context, sink Sink, args PreparerDecisionArgs) string {
	return send(ctx, sink, events.TypePreparerDecision, events.PreparerDecisionPayload{
		Header:    events.Header{V: events.PreparerDecisionVersion},
		Mode:      args.Mode,
		Knowledge: args.Knowledge,
		Tools:     args.Tools,
	}, 0)
}

// DriftDetectedArgs describes one drift between the in-state known-
// knowledge set and the actual visible-message scan. ReconciliationAction
// is free-form so future paths can record what they did without a schema
// bump — see RFC #249 §Pillar A for the vocabulary today.
//
// Helper is defined now so Phase 3's reconciliation code can land
// without a follow-up schema change; Phase 2 has no caller.
type DriftDetectedArgs struct {
	StateBelievedKnown   []string
	ActuallyVisible      []string
	MissingFromVisible   []string
	ExtrasInVisible      []string
	ReconciliationAction string
}

// EmitDriftDetected writes one drift_detected event.
func EmitDriftDetected(ctx context.Context, sink Sink, args DriftDetectedArgs) string {
	return send(ctx, sink, events.TypeDriftDetected, events.DriftDetectedPayload{
		Header:               events.Header{V: events.DriftDetectedVersion},
		StateBelievedKnown:   args.StateBelievedKnown,
		ActuallyVisible:      args.ActuallyVisible,
		MissingFromVisible:   args.MissingFromVisible,
		ExtrasInVisible:      args.ExtrasInVisible,
		ReconciliationAction: events.SanitizeUTF8(args.ReconciliationAction),
	}, 0)
}

// MessagesTruncatedArgs describes one sliding-window cutter pass.
// DroppedSeqRange is [from, to] inclusive indexed into sess.Messages
// (position-based, since the in-memory message slice does not carry a
// seq column). ReleasedKnowledgeIDs / RemainingKnownKnowledgeCount stay
// empty/zero in Phase 2 — the dedup state that would populate them
// arrives with Phase 3.
type MessagesTruncatedArgs struct {
	DroppedSeqFrom               int
	DroppedSeqTo                 int
	DroppedCount                 int
	ReleasedKnowledgeIDs         []string
	RemainingKnownKnowledgeCount int
}

// EmitMessagesTruncated writes one messages_truncated event. Callers
// should only emit when DroppedCount > 0; the helper does not guard
// against a no-op emit so the orchestrator's logic stays explicit.
func EmitMessagesTruncated(ctx context.Context, sink Sink, args MessagesTruncatedArgs) string {
	var seqRange []int
	if args.DroppedCount > 0 {
		seqRange = []int{args.DroppedSeqFrom, args.DroppedSeqTo}
	}
	return send(ctx, sink, events.TypeMessagesTruncated, events.MessagesTruncatedPayload{
		Header:                       events.Header{V: events.MessagesTruncatedVersion},
		DroppedSeqRange:              seqRange,
		DroppedCount:                 args.DroppedCount,
		ReleasedKnowledgeIDs:         args.ReleasedKnowledgeIDs,
		RemainingKnownKnowledgeCount: args.RemainingKnownKnowledgeCount,
	}, 0)
}
