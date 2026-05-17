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

	// ToolTier, when non-nil, drives the Phase-4 tier-aware Tools
	// block of preparer_decision. Nil means tier logic didn't run
	// (config disabled or no store wired) — emitPreparerDecision then
	// falls back to the Phase-2 pass-through (every candidate name in
	// tools.tier1_new). The decision also feeds the system-prompt
	// renderer (D3) and persists KnownTools via the dedup persist when
	// both ran in the same turn.
	ToolTier *toolTierDecision

	// LegacyKnowledgePlugins lists plugin names whose response carried
	// a legacy [knowledge_context] injection (pr.Message contains the
	// envelope) without the structured KnowledgeCandidates that Phase 3
	// dedup needs. When non-empty AND dedup is enabled, the orchestrator
	// switches the whole turn to mode=legacy_fallback — dedup is
	// skipped and the plugin's pr.Message passes through verbatim.
	LegacyKnowledgePlugins []string
}

// append pulls the candidate slices off one preparer's response into
// the aggregate. Safe to call with a zero-valued response (no-op).
// pluginName identifies the source plugin so legacy-injection
// fallback can name the affected plugin in the deprecation warning.
func (a *preparerAggregate) append(pluginName string, pr preparerResponse) {
	a.Knowledge = append(a.Knowledge, pr.KnowledgeCandidates...)
	a.Glossary = append(a.Glossary, pr.GlossaryCandidates...)
	a.Tools = append(a.Tools, pr.ToolCandidates...)
	if len(pr.ToolCandidates) == 0 && len(pr.RelevantTools) > 0 {
		a.LegacyRelevantTools = append(a.LegacyRelevantTools, pr.RelevantTools...)
	}
	if responseUsesLegacyKnowledgeInjection(pr) {
		a.LegacyKnowledgePlugins = append(a.LegacyKnowledgePlugins, pluginName)
	}
}

// responseUsesLegacyKnowledgeInjection returns true when the plugin
// returned a [knowledge_context] block in pr.Message without the
// structured KnowledgeCandidates slice that Phase 3 dedup needs.
// Detection is content-based (parser recognizes both legacy bare
// and tagged forms) so a plugin that started emitting tagged blocks
// without populating candidates still triggers the fallback path —
// dedup can only run when the per-candidate identity is in the
// structured field.
func responseUsesLegacyKnowledgeInjection(pr preparerResponse) bool {
	if len(pr.KnowledgeCandidates) > 0 {
		return false
	}
	return len(parseKnowledgeContextBlocks(pr.Message)) > 0
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
	hasSignal := len(agg.Knowledge) > 0 ||
		len(agg.Glossary) > 0 ||
		len(agg.Tools) > 0 ||
		len(agg.LegacyRelevantTools) > 0 ||
		len(agg.LegacyKnowledgePlugins) > 0
	if !hasSignal {
		// Nothing retrieved from any corpus and no legacy-fallback
		// trigger → skip the event. Avoids noisy preparer_decision
		// rows on turns where no preparer ran (toolCallSeeded path,
		// sessions with empty o.preparers, …). Legacy-fallback is
		// counted as signal because the audit trail benefits from
		// recording the fallback even if no candidates surfaced.
		return
	}

	var knowledgeBlock events.PreparerDecisionKnowledgeBlock
	var mode string
	switch {
	case o.knowledgeDedup.Enabled && len(agg.LegacyKnowledgePlugins) > 0:
		// A plugin still uses the legacy [knowledge_context]-in-Message
		// shape, so dedup can't decide on a per-candidate basis. Report
		// fallback mode; Injected/Skipped stay empty since dedup didn't
		// run. CandidateIDs are surfaced for any non-legacy plugin that
		// did return structured candidates — the consumer can still
		// see what was retrieved even though the decision was forced
		// to pass-through.
		mode = events.PreparerDecisionModeLegacyFallback
		knowledgeBlock = events.PreparerDecisionKnowledgeBlock{
			CandidateIDs: knowledgeCandidateIDs(agg.Knowledge),
		}
	case agg.KnowledgeDedup != nil:
		mode = events.PreparerDecisionModeFull
		knowledgeBlock = events.PreparerDecisionKnowledgeBlock{
			CandidateIDs:          knowledgeCandidateIDs(agg.Knowledge),
			Injected:              dedupInjectedItems(agg.KnowledgeDedup),
			SkippedKnown:          dedupSkippedItems(agg.KnowledgeDedup),
			ScoreOverridesApplied: agg.KnowledgeDedup.ScoreOverrides,
			InjectedBytes:         agg.KnowledgeDedup.InjectedBytes(),
		}
	default:
		mode = events.PreparerDecisionModeInstrumentationOnly
		knowledgeBlock = events.PreparerDecisionKnowledgeBlock{
			CandidateIDs:  knowledgeCandidateIDs(agg.Knowledge),
			Injected:      knowledgeCandidatesToInjected(agg.Knowledge),
			InjectedBytes: knowledgeInjectedBytes(agg.Knowledge),
		}
	}

	toolsBlock := buildToolsBlock(agg)

	emit.EmitPreparerDecision(ctx, o.eventSink, emit.PreparerDecisionArgs{
		Mode:      mode,
		Knowledge: knowledgeBlock,
		Tools:     toolsBlock,
	})
}

// buildToolsBlock returns the preparer_decision.tools payload for the
// current aggregate. With ToolTier set (Phase 4 enabled + ran), each
// tier bucket is reported separately + the LRU/eviction telemetry
// + the get_tool_details promotion list. Without ToolTier (Phase 2 /
// Phase 3 fall-through), every candidate appears in tier1_new — the
// instrumentation_only behaviour Phase 2 introduced.
func buildToolsBlock(agg preparerAggregate) events.PreparerDecisionToolsBlock {
	if agg.ToolTier == nil {
		return events.PreparerDecisionToolsBlock{
			Tier1New: toolCandidateNames(agg.Tools, agg.LegacyRelevantTools),
		}
	}
	d := agg.ToolTier
	return events.PreparerDecisionToolsBlock{
		Tier0Count:                len(d.Tier0),
		Tier1New:                  d.Tier1New,
		Tier1Carried:              d.Tier1Carried,
		Tier1EvictedToTier3:       d.Tier1EvictedToTier3,
		Tier1DemotedSticky:        d.Tier1DemotedSticky,
		Tier1SizeAfter:            d.Tier1SizeAfter,
		Tier1Cap:                  d.Tier1Cap,
		Tier2Tools:                d.Tier2,
		Tier2SizeAfter:            len(d.Tier2),
		Tier2Cap:                  d.Tier2Cap,
		Tier3TotalVisible:         len(d.Tier3),
		PromotedViaGetToolDetails: d.PromotedViaGetToolDetails,
	}
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
