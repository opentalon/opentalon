package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Preparer-phase event-emission helpers (RFC #249 Phase 2). Pulled out
// of orchestrator.go so the hot-path preparer loop reads as
//
//	outcome := runSinglePreparer(...)
//	emitPreparerRetrievals(...)
//	accumulate(...)
//
// instead of inlining ~80 lines of candidate-to-hit conversion. The
// helpers themselves are pure functions over preparerResponse — no
// orchestrator state mutation, no plugin calls — so the same logic is
// reusable in Phase 3+ once `mode: full` starts driving real decisions.

// preparerAggregate accumulates candidate data across all preparers in
// one turn so emitPreparerDecision can publish the composite outcome
// once at the end of the loop.
type preparerAggregate struct {
	Knowledge []KnowledgeCandidate
	Glossary  []GlossaryCandidate
	Tools     []ToolCandidate

	// LegacyRelevantTools captures `pr.RelevantTools` for plugins that
	// haven't migrated to the structured ToolCandidates shape yet.
	// preparer_decision still surfaces them under tools.tier1_new so
	// instrumentation does not blank out on legacy plugins.
	LegacyRelevantTools []string

	// KnowledgeDedup, when non-nil, switches emitPreparerDecision into
	// the Phase-3 "full" mode: the injected/skipped/score-override
	// buckets reflect the actual dedup outcome rather than the
	// instrumentation_only pass-through. Nil means dedup didn't run
	// (config disabled, no store wired, or no knowledge candidates).
	KnowledgeDedup *knowledgeDedupDecision
}

// append pulls the candidate slices off one preparer's response into
// the aggregate. Safe to call with a zero-valued response (no-op).
func (a *preparerAggregate) append(pr preparerResponse) {
	a.Knowledge = append(a.Knowledge, pr.KnowledgeCandidates...)
	a.Glossary = append(a.Glossary, pr.GlossaryCandidates...)
	a.Tools = append(a.Tools, pr.ToolCandidates...)
	if len(pr.ToolCandidates) == 0 && len(pr.RelevantTools) > 0 {
		a.LegacyRelevantTools = append(a.LegacyRelevantTools, pr.RelevantTools...)
	}
}

// emitPreparerRetrievals fires the per-corpus *_retrieval events for
// one preparer pass. Each event is emitted via the existing emit
// helpers and inherits ctx's ParentID (which the caller has wrapped to
// point at user_message). Search-text dimensions come from the
// plugin's RetrievalMetrics if present, otherwise defaulted from the
// orchestrator's enrichment state.
func (o *Orchestrator) emitPreparerRetrievals(ctx context.Context, query string, defaultSource string, pr preparerResponse) {
	if o.eventSink == nil {
		return
	}

	km, gm, tm := splitCorpusMetrics(pr.RetrievalMetrics)

	if len(pr.KnowledgeCandidates) > 0 || km != nil {
		args := emit.KnowledgeRetrievalArgs{
			Query:            query,
			SearchTextSource: defaultSource,
			Hits:             knowledgeCandidatesToHits(pr.KnowledgeCandidates),
		}
		applyCorpusMetrics(km, &args.SearchTextSource, &args.TopK, &args.MinScore, &args.LatencyMS)
		emit.EmitKnowledgeRetrieval(ctx, o.eventSink, args)
	}

	if len(pr.GlossaryCandidates) > 0 || gm != nil {
		args := emit.GlossaryRetrievalArgs{
			Query:            query,
			SearchTextSource: defaultSource,
			Hits:             glossaryCandidatesToHits(pr.GlossaryCandidates),
		}
		applyCorpusMetrics(gm, &args.SearchTextSource, &args.TopK, &args.MinScore, &args.LatencyMS)
		emit.EmitGlossaryRetrieval(ctx, o.eventSink, args)
	}

	// Tools: emit when the plugin returned ToolCandidates, metrics, OR
	// the legacy relevant_tools list. The legacy path produces hits
	// with score=0 so the event still reflects "this many tools came
	// out of the retrieval", just without per-tool ranking.
	hasToolSignal := len(pr.ToolCandidates) > 0 || tm != nil || len(pr.RelevantTools) > 0
	if hasToolSignal {
		hits := toolCandidatesToHits(pr.ToolCandidates)
		if len(hits) == 0 && len(pr.RelevantTools) > 0 {
			hits = make([]events.ToolRetrievalHit, 0, len(pr.RelevantTools))
			for _, name := range pr.RelevantTools {
				hits = append(hits, events.ToolRetrievalHit{ToolName: name})
			}
		}
		args := emit.ToolRetrievalArgs{
			Query:            query,
			SearchTextSource: defaultSource,
			Hits:             hits,
		}
		applyCorpusMetrics(tm, &args.SearchTextSource, &args.TopK, &args.MinScore, &args.LatencyMS)
		emit.EmitToolRetrieval(ctx, o.eventSink, args)
	}
}

// emitPreparerDecision publishes the composite preparer-pass outcome
// once per user turn. The shape of the Knowledge block depends on
// whether agg.KnowledgeDedup is set:
//
//   - nil: Phase 2 / instrumentation_only mode. Every candidate is
//     reported under Knowledge.Injected with reason
//     "instrumentation_only"; the skipped / score-override buckets
//     stay empty.
//   - non-nil: Phase 3 / full mode. Injected / Skipped /
//     ScoreOverridesApplied reflect the dedup decision exactly; the
//     reason on each injected entry is "new" / "score_override" /
//     "top_k_force"; cap-exceeded entries land in Skipped.
//
// In both modes Tools.Tier1New surfaces every ToolCandidate plus any
// legacy relevant_tools fallback list so the event stays meaningful
// while Phase 4's tier logic is off.
func (o *Orchestrator) emitPreparerDecision(ctx context.Context, agg preparerAggregate) {
	if o.eventSink == nil {
		return
	}
	if len(agg.Knowledge) == 0 && len(agg.Glossary) == 0 && len(agg.Tools) == 0 && len(agg.LegacyRelevantTools) == 0 {
		// Nothing retrieved from any corpus → skip the event. Avoids
		// noisy preparer_decision rows on turns where no preparer ran
		// (toolCallSeeded path, sessions with empty o.preparers, …).
		return
	}

	var knowledgeBlock events.PreparerDecisionKnowledgeBlock
	mode := events.PreparerDecisionModeInstrumentationOnly
	if agg.KnowledgeDedup != nil {
		mode = events.PreparerDecisionModeFull
		knowledgeBlock = events.PreparerDecisionKnowledgeBlock{
			CandidateIDs:          knowledgeCandidateIDs(agg.Knowledge),
			Injected:              dedupInjectedItems(agg.KnowledgeDedup),
			SkippedKnown:          dedupSkippedItems(agg.KnowledgeDedup),
			ScoreOverridesApplied: agg.KnowledgeDedup.ScoreOverrides,
			InjectedBytes:         agg.KnowledgeDedup.InjectedBytes(),
		}
	} else {
		knowledgeBlock = events.PreparerDecisionKnowledgeBlock{
			CandidateIDs:  knowledgeCandidateIDs(agg.Knowledge),
			Injected:      knowledgeCandidatesToInjected(agg.Knowledge),
			InjectedBytes: knowledgeInjectedBytes(agg.Knowledge),
		}
	}

	toolsBlock := events.PreparerDecisionToolsBlock{
		Tier1New: toolCandidateNames(agg.Tools, agg.LegacyRelevantTools),
	}

	emit.EmitPreparerDecision(ctx, o.eventSink, emit.PreparerDecisionArgs{
		Mode:      mode,
		Knowledge: knowledgeBlock,
		Tools:     toolsBlock,
	})
}

// dedupInjectedItems converts the dedup decision's parallel
// Injected/InjectedReasons slices into the event payload shape.
func dedupInjectedItems(d *knowledgeDedupDecision) []events.PreparerDecisionInjectedItem {
	if len(d.Injected) == 0 {
		return nil
	}
	out := make([]events.PreparerDecisionInjectedItem, len(d.Injected))
	for i, c := range d.Injected {
		out[i] = events.PreparerDecisionInjectedItem{
			ArticleID:     c.ArticleID,
			ContentSHA256: c.ContentSHA256,
			Reason:        d.InjectedReasons[i],
		}
	}
	return out
}

// dedupSkippedItems converts the dedup decision's parallel
// Skipped/SkippedReasons slices into the event payload shape.
func dedupSkippedItems(d *knowledgeDedupDecision) []events.PreparerDecisionSkippedItem {
	if len(d.Skipped) == 0 {
		return nil
	}
	out := make([]events.PreparerDecisionSkippedItem, len(d.Skipped))
	for i, c := range d.Skipped {
		out[i] = events.PreparerDecisionSkippedItem{
			ArticleID: c.ArticleID,
			Reason:    d.SkippedReasons[i],
		}
	}
	return out
}

// ----- Pure conversion helpers (no orchestrator state) -----

func knowledgeCandidatesToHits(cands []KnowledgeCandidate) []events.KnowledgeRetrievalHit {
	if len(cands) == 0 {
		return nil
	}
	out := make([]events.KnowledgeRetrievalHit, len(cands))
	for i, c := range cands {
		out[i] = events.KnowledgeRetrievalHit{
			ArticleID:     c.ArticleID,
			Title:         c.Title,
			Score:         c.Score,
			ContentSHA256: c.ContentSHA256,
			Source:        c.Source,
		}
	}
	return out
}

func glossaryCandidatesToHits(cands []GlossaryCandidate) []events.GlossaryRetrievalHit {
	if len(cands) == 0 {
		return nil
	}
	out := make([]events.GlossaryRetrievalHit, len(cands))
	for i, c := range cands {
		out[i] = events.GlossaryRetrievalHit{
			Term:          c.Term,
			Score:         c.Score,
			ContentSHA256: c.ContentSHA256,
			Source:        c.Source,
		}
	}
	return out
}

func toolCandidatesToHits(cands []ToolCandidate) []events.ToolRetrievalHit {
	if len(cands) == 0 {
		return nil
	}
	out := make([]events.ToolRetrievalHit, len(cands))
	for i, c := range cands {
		out[i] = events.ToolRetrievalHit{
			ToolName: c.ToolName,
			Score:    c.Score,
		}
	}
	return out
}

func knowledgeCandidateIDs(cands []KnowledgeCandidate) []string {
	if len(cands) == 0 {
		return nil
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.ArticleID
	}
	return out
}

func knowledgeCandidatesToInjected(cands []KnowledgeCandidate) []events.PreparerDecisionInjectedItem {
	if len(cands) == 0 {
		return nil
	}
	out := make([]events.PreparerDecisionInjectedItem, len(cands))
	for i, c := range cands {
		out[i] = events.PreparerDecisionInjectedItem{
			ArticleID:     c.ArticleID,
			ContentSHA256: c.ContentSHA256,
			// Phase 2 has no dedup state, so every candidate is reported
			// as if newly injected. Phase 3+ switches to "new" /
			// "score_override" / "top_k_force" per the dedup decision
			// (see dedupInjectedItems).
			Reason: events.PreparerDecisionReasonInstrumentationOnly,
		}
	}
	return out
}

func knowledgeInjectedBytes(cands []KnowledgeCandidate) int {
	total := 0
	for _, c := range cands {
		total += len(c.Content)
	}
	return total
}

func toolCandidateNames(cands []ToolCandidate, legacy []string) []string {
	if len(cands) == 0 && len(legacy) == 0 {
		return nil
	}
	out := make([]string, 0, len(cands)+len(legacy))
	for _, c := range cands {
		out = append(out, c.ToolName)
	}
	out = append(out, legacy...)
	return out
}

// splitCorpusMetrics returns the three per-corpus metric pointers from
// the optional RetrievalMetrics envelope. Each can be nil
// independently, mirroring the plugin's "set only what you measured"
// contract.
func splitCorpusMetrics(rm *PreparerRetrievalMetrics) (k, g, t *PreparerCorpusMetrics) {
	if rm == nil {
		return nil, nil, nil
	}
	return rm.Knowledge, rm.Glossary, rm.Tools
}

// applyCorpusMetrics copies the populated fields from m into the
// retrieval-args pointers when m is non-nil. SearchTextSource only
// overrides the caller-provided default when the plugin set a
// non-empty value, so the orchestrator's enrichment-state default
// (user_input vs enriched) stays as fallback.
func applyCorpusMetrics(m *PreparerCorpusMetrics, searchTextSource *string, topK *int, minScore *float64, latencyMS *int64) {
	if m == nil {
		return
	}
	if m.SearchTextSource != "" {
		*searchTextSource = m.SearchTextSource
	}
	if m.TopK != 0 {
		*topK = m.TopK
	}
	if m.MinScore != 0 {
		*minScore = m.MinScore
	}
	if m.LatencyMS != 0 {
		*latencyMS = m.LatencyMS
	}
}
