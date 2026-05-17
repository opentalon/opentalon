package orchestrator

import (
	"context"
	"log/slog"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// RFC #249 Phase 3 dedup decision logic.
//
// Pure functions over the candidate list + persisted state + config —
// no orchestrator pointer receivers, no plugin calls, no I/O. The
// preparer loop calls applyKnowledgeDedup once per user turn with the
// aggregate candidate list from all preparers; the resulting decision
// (a) drives the [knowledge_context] block the LLM sees, (b) feeds
// the preparer_decision event payload, and (c) carries the updated
// InjectionState the caller persists back to the session row.
//
// Keeping the logic separate from the I/O makes the test surface
// trivial: every dedup path (new / score_override / top_k_force /
// already_known / cap_exceeded) is exercised against a static
// candidate slice without a session store mock. The reason vocabulary
// itself lives next to the event payload types in
// internal/state/store/events.event_types.go as PreparerDecisionReason*
// constants so consumers (api-plugin, Timly review UI) share one
// source of truth with the producer side.

// knowledgeDedupDecision is the result of one dedup pass. The slices
// preserve candidate order so the rendered [knowledge_context] block
// matches the plugin's ranking, and so the preparer_decision event
// faithfully reflects what the LLM actually saw.
type knowledgeDedupDecision struct {
	// Injected is the subset of candidates that should be spliced into
	// the current turn's [knowledge_context] block, in input order.
	Injected []KnowledgeCandidate
	// InjectedReasons is parallel to Injected: one of dedupReasonNew /
	// dedupReasonScoreOverride / dedupReasonTopKForce per entry.
	InjectedReasons []string
	// Skipped lists candidates the orchestrator chose NOT to inject;
	// SkippedReasons parallels it with the explanation.
	Skipped        []KnowledgeCandidate
	SkippedReasons []string
	// ScoreOverrides records the (article, score, threshold) tuples
	// for candidates that re-injected via the score-override path.
	// Used for preparer_decision payload diagnostics.
	ScoreOverrides []events.PreparerDecisionScoreOverride
	// UpdatedState is the post-decision InjectionState — the caller
	// persists this back via SessionStore.UpdateInjectionState.
	// known_knowledge is the union of the pre-call state and every
	// candidate the preparer surfaced (whether injected or skipped),
	// matching the RFC's "the LLM knows about rejected-because-known
	// ones too" rule.
	UpdatedState state.InjectionState
}

// InjectedBytes is the sum of Content lengths across the Injected set
// — feeds preparer_decision.knowledge.injected_bytes.
func (d *knowledgeDedupDecision) InjectedBytes() int {
	total := 0
	for _, c := range d.Injected {
		total += len(c.Content)
	}
	return total
}

// applyKnowledgeDedup runs the RFC #249 dedup algorithm over the
// preparer-aggregate candidate list. The algorithm:
//
//  1. A candidate is selected for injection when its ContentSHA256
//     is not yet in existing.KnownKnowledge ("new"), OR its current
//     score exceeds cfg.ReinjectScoreThreshold ("score_override"),
//     OR its slice index is below cfg.ReinjectTopKForce ("top_k_force").
//  2. The selected set is capped at cfg.CapPerTurn; overflow is moved
//     to Skipped with reason "cap_exceeded".
//  3. Every candidate (injected or skipped) is added to UpdatedState's
//     known_knowledge, deduplicating on ContentSHA256 so a plugin
//     returning the same SHA twice within a turn only produces one
//     entry.
//
// The slice index drives the top-K-force decision instead of the
// candidate's PositionInResults field: this matches the plugin
// contract that the candidate slice is already ranked by score
// (which is how RAG retrieval orders its output), and avoids the
// 0-vs-1-indexed ambiguity that the omitempty PositionInResults
// field would otherwise introduce.
func applyKnowledgeDedup(
	candidates []KnowledgeCandidate,
	existing state.InjectionState,
	cfg KnowledgeDedupConfig,
	currentTurn int,
) knowledgeDedupDecision {
	known := make(map[string]bool, len(existing.KnownKnowledge))
	for _, k := range existing.KnownKnowledge {
		known[k.ContentSHA256] = true
	}

	out := knowledgeDedupDecision{
		UpdatedState: state.InjectionState{
			KnownKnowledge: append([]state.KnownKnowledgeEntry(nil), existing.KnownKnowledge...),
			KnownTools:     existing.KnownTools, // Phase 4 territory; preserve forward-compat
		},
	}
	injectCount := 0
	for i, c := range candidates {
		seen := known[c.ContentSHA256]

		// Pick the inject reason — first match wins. Falls through to
		// "already known + below thresholds" → goes to Skipped.
		var injectReason string
		switch {
		case !seen:
			injectReason = events.PreparerDecisionReasonNew
		case c.Score > cfg.ReinjectScoreThreshold:
			injectReason = events.PreparerDecisionReasonScoreOverride
		case i < cfg.ReinjectTopKForce:
			injectReason = events.PreparerDecisionReasonTopKForce
		}

		switch {
		case injectReason == "":
			out.Skipped = append(out.Skipped, c)
			out.SkippedReasons = append(out.SkippedReasons, events.PreparerDecisionReasonAlreadyKnown)
		case injectCount >= cfg.CapPerTurn:
			// The candidate WOULD have injected but the per-turn cap
			// kicked in. Record under Skipped so the event consumer
			// sees the budget-pressure signal.
			out.Skipped = append(out.Skipped, c)
			out.SkippedReasons = append(out.SkippedReasons, events.PreparerDecisionReasonCapExceeded)
		default:
			out.Injected = append(out.Injected, c)
			out.InjectedReasons = append(out.InjectedReasons, injectReason)
			injectCount++
			if injectReason == events.PreparerDecisionReasonScoreOverride {
				out.ScoreOverrides = append(out.ScoreOverrides, events.PreparerDecisionScoreOverride{
					ArticleID:    c.ArticleID,
					CurrentScore: c.Score,
					Threshold:    cfg.ReinjectScoreThreshold,
				})
			}
		}

		// State update: every fresh SHA gets a known_knowledge entry
		// regardless of whether it injected or was skipped — the LLM
		// will "see" it in the rendered KC block (if injected) or
		// "know about" it because the cap or threshold pushed it out
		// (then the next turn shouldn't re-surface it as new).
		if !seen {
			out.UpdatedState.KnownKnowledge = append(out.UpdatedState.KnownKnowledge, state.KnownKnowledgeEntry{
				ArticleID:         c.ArticleID,
				ContentSHA256:     c.ContentSHA256,
				FirstInjectedTurn: currentTurn,
			})
			known[c.ContentSHA256] = true
		}
	}
	return out
}

// knowledgeCandidatesToInjections converts the dedup decision's
// Injected slice into the input shape renderKnowledgeContextBlock
// expects. Pure — no state, no allocation when the input is empty.
func knowledgeCandidatesToInjections(cands []KnowledgeCandidate) []kcInjection {
	if len(cands) == 0 {
		return nil
	}
	out := make([]kcInjection, len(cands))
	for i, c := range cands {
		out[i] = kcInjection{
			ArticleID:     c.ArticleID,
			ContentSHA256: c.ContentSHA256,
			Body:          c.Content,
		}
	}
	return out
}

// applyDedupToContent rebuilds the current-turn user message: strip
// every plugin-emitted [knowledge_context] block, prepend the
// dedup-decision's rendered block (when non-empty), and join with a
// single blank line so the LLM sees the KC envelope cleanly preceded
// by the user's actual text.
//
// When the decision injects nothing, the stripped user text is
// returned as-is (no empty `[knowledge_context]` shell). When the
// decision injects something but the stripped user text is empty
// (rare — only when the plugin returned a KC-only Message), the KC
// block becomes the entire content; the LLM treats the KC as its
// instruction context, which mirrors what the legacy per-turn
// re-inject behaviour already does.
func applyDedupToContent(content string, injections []kcInjection) string {
	stripped := stripKnowledgeContext(content)
	rendered := renderKnowledgeContextBlock(injections)
	switch {
	case rendered == "":
		return stripped
	case stripped == "":
		return rendered
	default:
		return rendered + "\n\n" + stripped
	}
}

// prepareDedupDecision is the orchestrator's Phase-3 entry point for
// READ + COMPUTE: read the persisted InjectionState, run the dedup
// decision, rewrite the user-turn content with the deduped
// [knowledge_context] block, and stash the decision on agg so
// emitPreparerDecision picks up the full-mode payload. The actual
// state PERSIST happens in persistDedupDecision after the event is
// emitted — RFC #249 invariant: "Event emission precedes state writes
// within a turn. If a state write fails after an event was emitted,
// the next preparer pass detects drift and reconciles. This
// guarantees the event log is the durable record even on partial
// failure."
//
// Robustness contract:
//   - If reading the state fails, log and start fresh (RFC: "service
//     availability ahead of dedup correctness"). The orchestrator
//     proceeds with an empty existing state — every candidate then
//     appears as "new" — which gives the user this turn full context
//     at the cost of one extra LLM token spend.
//
// The method assumes the caller has already verified that dedup is
// enabled, a store is wired, and there are knowledge candidates to
// decide on — see the preparer-loop callsite.
func (o *Orchestrator) prepareDedupDecision(ctx context.Context, sessionID, content string, agg *preparerAggregate) string {
	// Reconciliation step: load persisted state, scan the visible
	// message stream, emit drift_detected when they disagree, and
	// use the corrected state as the dedup input. RFC #249: the
	// visible-message scan is authoritative.
	existing := o.reconcileAndEmitDrift(ctx, sessionID)

	turn := o.turnNumberForDedup(sessionID)
	decision := applyKnowledgeDedup(agg.Knowledge, existing, o.knowledgeDedup, turn)
	agg.KnowledgeDedup = &decision

	return applyDedupToContent(content, knowledgeCandidatesToInjections(decision.Injected))
}

// persistDedupDecision writes the decision's UpdatedState back to the
// store. Logs and swallows errors so a transient store failure
// doesn't abort the user turn — the next preparer pass's lazy
// reconciliation step (Phase 3 C5) catches the resulting drift and
// self-corrects. Caller is expected to have already emitted the
// preparer_decision event so the audit trail survives partial
// failure (RFC #249 invariant).
func (o *Orchestrator) persistDedupDecision(ctx context.Context, sessionID string, agg *preparerAggregate) {
	if agg.KnowledgeDedup == nil {
		return
	}
	if err := o.injectionStateStore.UpdateInjectionState(ctx, sessionID, agg.KnowledgeDedup.UpdatedState); err != nil {
		slog.WarnContext(ctx, "knowledge_dedup: write state failed, will reconcile next turn",
			"component", "orchestrator", "session", sessionID, "error", err)
	}
}

// legacyWarningKeySeparator joins sessionID and pluginName into the
// sync.Map key. Pulled into a constant so callers and tests share one
// source of truth — drifting the delimiter would silently double-warn.
const legacyWarningKeySeparator = "|"

// warnLegacyKnowledgePluginsOnce logs a deprecation warning the first
// time a (sessionID, pluginName) pair hits the legacy_fallback path.
// Subsequent legacy detections for the same pair stay silent so a
// session that keeps using a legacy preparer doesn't flood the log
// with the same message every turn. The de-dup map is in-process,
// not persisted — a process restart re-warns once per session, which
// is acceptable for what is fundamentally observability data.
func (o *Orchestrator) warnLegacyKnowledgePluginsOnce(ctx context.Context, sessionID string, plugins []string) {
	for _, plugin := range plugins {
		key := sessionID + legacyWarningKeySeparator + plugin
		if _, alreadyWarned := o.legacyKnowledgeWarnings.LoadOrStore(key, struct{}{}); alreadyWarned {
			continue
		}
		slog.WarnContext(ctx,
			"knowledge_dedup: plugin returned legacy [knowledge_context] in pr.Message without structured knowledge_candidates — falling back to mode=legacy_fallback for this turn. Please migrate the plugin to return KnowledgeCandidates so dedup can run.",
			"component", "orchestrator",
			"session", sessionID,
			"plugin", plugin)
	}
}

// turnNumberForDedup returns a stable monotonically-increasing turn
// counter for the session — used as KnownKnowledgeEntry.FirstInjectedTurn
// so a downstream event consumer can tell "this article was first
// surfaced 4 turns ago" without scanning the full event log.
//
// Implemented as the count of user-role messages in the session plus
// one, on the theory that the upcoming user message will become the
// next entry. Imperfect (assistant-led turns aren't counted, store
// errors silently fall back to turn=1) but sufficient for diagnostic
// value; an explicit turn-counter column would be the proper Phase-4
// fix if/when analytics needs it.
func (o *Orchestrator) turnNumberForDedup(sessionID string) int {
	sess, err := o.sessions.Get(sessionID)
	if err != nil || sess == nil {
		return 1
	}
	turn := 1
	for _, m := range sess.Messages {
		if m.Role == provider.RoleUser {
			turn++
		}
	}
	return turn
}
