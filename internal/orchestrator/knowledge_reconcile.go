package orchestrator

import (
	"context"
	"log/slog"
	"sort"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// RFC #249 Phase 3 lazy reconciliation: scan the visible message
// stream for ID-tagged [knowledge_context] blocks, derive the actually
// injected SHA set, and reconcile against the persisted InjectionState.
//
// Why this exists:
//   - The dedup decision (knowledge_dedup.go) trusts persisted state to
//     answer "is this article already in the LLM's context?". Anything
//     that mutates the message stream outside the orchestrator's
//     awareness — summarization, sliding-window truncation, future
//     redaction — invalidates that trust.
//   - Reconciliation runs once per preparer pass, BEFORE the dedup
//     decision. The corrected state feeds applyKnowledgeDedup.
//   - When state and visible disagree, a drift_detected event records
//     what was rewritten so the audit trail stays complete.
//
// Reconciliation is intentionally authoritative: the visible-message
// scan wins. If state says "kb_a known" but no kb_a block is visible,
// kb_a is dropped from state (LLM doesn't "know" it anymore, future
// re-retrieval will re-inject). Symmetrically, an extra visible block
// not in state gets added — without this the orchestrator would
// double-inject content the LLM already has.

// reconciliationAction is the fixed-vocabulary value the drift_detected
// event records when reconciliation rewrote state. Phase 3 has only
// one corrective strategy (rewrite-from-visible) so a single constant
// suffices; future strategies (e.g. "partial_merge_with_state") will
// add siblings rather than reformatting this one.
const reconciliationActionRewriteFromVisible = "state_rewritten_from_visible_scan"

// driftReport captures the discrepancy between persisted and visible
// state. Empty fields mean no drift in that direction; the whole
// struct is nil when state and visible agree. Used by both the
// drift_detected event payload and the unit tests.
type driftReport struct {
	StateBelievedKnown   []string // SHAs persisted state thought were known
	ActuallyVisible      []string // SHAs the message scan actually found
	MissingFromVisible   []string // in state, not in visible — LLM forgot
	ExtrasInVisible      []string // in visible, not in state — state lost track
	ReconciliationAction string
}

// reconcileInjectionState runs the lazy-reconciliation algorithm.
// Returns the corrected state (always — even when there's no drift,
// callers can use the result directly) and an optional drift report
// (nil when state and visible agree). Pure function — no orchestrator
// state mutation, no I/O.
//
// Only user-role messages are scanned: [knowledge_context] blocks only
// appear in user messages by construction (the orchestrator prepends
// them when it builds the current-turn content).
//
// When drift is detected, the corrected state's KnownKnowledge is
// derived from the visible blocks; FirstInjectedTurn is preserved
// from the persisted entry when the SHA was already known
// (continuity for diagnostics), or set to 0 for extras_in_visible
// entries (lost-track entries get a synthetic "unknown turn" marker).
// KnownTools is copied through unchanged — Phase 4 territory.
func reconcileInjectionState(messages []provider.Message, persisted state.InjectionState) (state.InjectionState, *driftReport) {
	visible := scanVisibleKnowledgeBlocks(messages)

	visibleSHAs := make(map[string]parsedKCBlock, len(visible))
	for _, blk := range visible {
		if blk.ContentSHA256 == "" {
			continue // legacy untagged block — nothing to reconcile against
		}
		visibleSHAs[blk.ContentSHA256] = blk
	}

	persistedSHAs := make(map[string]state.KnownKnowledgeEntry, len(persisted.KnownKnowledge))
	for _, k := range persisted.KnownKnowledge {
		persistedSHAs[k.ContentSHA256] = k
	}

	var missing, extras []string
	for sha := range persistedSHAs {
		if _, ok := visibleSHAs[sha]; !ok {
			missing = append(missing, sha)
		}
	}
	for sha := range visibleSHAs {
		if _, ok := persistedSHAs[sha]; !ok {
			extras = append(extras, sha)
		}
	}

	if len(missing) == 0 && len(extras) == 0 {
		// State and visible agree → return persisted state unchanged
		// so the caller can keep its slice header. No drift event.
		return persisted, nil
	}

	corrected := state.InjectionState{
		KnownKnowledge: rebuildKnownKnowledge(visible, persistedSHAs),
		KnownTools:     persisted.KnownTools,
	}
	report := &driftReport{
		StateBelievedKnown:   sortedKeys(persistedSHAs),
		ActuallyVisible:      sortedKeys(visibleSHAs),
		MissingFromVisible:   sortedStrings(missing),
		ExtrasInVisible:      sortedStrings(extras),
		ReconciliationAction: reconciliationActionRewriteFromVisible,
	}
	return corrected, report
}

// scanVisibleKnowledgeBlocks walks the user-role messages and returns
// every parsed [knowledge_context] block. Blocks without an `id` and
// `sha` attribute (legacy untagged form) are still returned so callers
// can count them, but they're filtered out by the dedup-state
// reconciliation since their identity is unknown.
func scanVisibleKnowledgeBlocks(messages []provider.Message) []parsedKCBlock {
	var out []parsedKCBlock
	for _, m := range messages {
		if m.Role != provider.RoleUser || m.Content == "" {
			continue
		}
		out = append(out, parseKnowledgeContextBlocks(m.Content)...)
	}
	return out
}

// rebuildKnownKnowledge constructs the corrected KnownKnowledge slice
// from the visible blocks. Persisted FirstInjectedTurn is preserved
// when the SHA was already known — that diagnostic value should
// survive reconciliation. Extras (visible but unpersisted) get
// FirstInjectedTurn=0 to mark them as "discovered via reconciliation".
func rebuildKnownKnowledge(visible []parsedKCBlock, persistedSHAs map[string]state.KnownKnowledgeEntry) []state.KnownKnowledgeEntry {
	out := make([]state.KnownKnowledgeEntry, 0, len(visible))
	seen := make(map[string]bool, len(visible))
	for _, blk := range visible {
		if blk.ContentSHA256 == "" || seen[blk.ContentSHA256] {
			continue
		}
		seen[blk.ContentSHA256] = true
		entry := state.KnownKnowledgeEntry{
			ArticleID:     blk.ArticleID,
			ContentSHA256: blk.ContentSHA256,
		}
		if prior, ok := persistedSHAs[blk.ContentSHA256]; ok {
			entry.FirstInjectedTurn = prior.FirstInjectedTurn
		}
		out = append(out, entry)
	}
	return out
}

// sortedKeys returns the map keys in a deterministic order so the
// drift_detected event payload doesn't churn on every emit.
func sortedKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedStrings returns a sorted copy of in. Convenience wrapper so
// the call sites in reconcileInjectionState stay one-liners.
func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// reconcileAndEmitDrift is the orchestrator's reconciliation entry point.
// Loads persisted state, runs reconciliation against the session's
// visible messages, emits a drift_detected event when drift is found,
// and returns the corrected state for the dedup decision to consume.
//
// Failure-mode contract — both error paths fall back to empty state
// rather than persisted state. The visible-message scan is the
// authoritative source per RFC #249; if we can't run it (either we
// can't read state, or we can't load the message stream) we err
// toward "treat as first turn" so a later turn's full re-injection
// is the worst case, instead of carrying stale state forward.
//
// The drift_detected event inherits ctx's ParentID (which the caller
// has scoped to user_message, matching the other preparer-phase
// events).
func (o *Orchestrator) reconcileAndEmitDrift(ctx context.Context, sessionID string) state.InjectionState {
	if o.injectionStateStore == nil {
		return state.InjectionState{}
	}
	persisted, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "knowledge_dedup: read state failed during reconciliation, starting fresh",
			"component", "orchestrator", "session", sessionID, "error", err)
		return state.InjectionState{}
	}
	sess, err := o.sessions.Get(sessionID)
	if err != nil || sess == nil {
		slog.WarnContext(ctx, "knowledge_dedup: session lookup failed during reconciliation, starting fresh",
			"component", "orchestrator", "session", sessionID, "error", err)
		return state.InjectionState{}
	}
	corrected, drift := reconcileInjectionState(sess.Messages, persisted)
	if drift == nil {
		return corrected
	}
	emit.EmitDriftDetected(ctx, o.eventSink, emit.DriftDetectedArgs{
		StateBelievedKnown:   drift.StateBelievedKnown,
		ActuallyVisible:      drift.ActuallyVisible,
		MissingFromVisible:   drift.MissingFromVisible,
		ExtrasInVisible:      drift.ExtrasInVisible,
		ReconciliationAction: drift.ReconciliationAction,
	})
	return corrected
}
