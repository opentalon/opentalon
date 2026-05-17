package orchestrator

import (
	"context"
	"log/slog"
	"sort"

	"github.com/opentalon/opentalon/internal/state"
)

// RFC #249 Phase 4 three-tier tool visibility decision logic.
//
// Pure functions over the per-turn candidate list, the persisted
// per-session known_tools state, and the available-tools universe —
// mirroring the Phase-3 knowledge_dedup pattern. The decision drives
// (a) which tool definitions land in the LLM-visible `tools` array
// with full schemas (Tier 0 + Tier 1), (b) which surface in the
// system prompt with a 1-line summary (Tier 2, D3 renders), (c)
// which surface as names-only grouped by capability (Tier 3, D3
// renders), and (d) the preparer_decision.tools event payload.
//
// Tier assignments per RFC:
//   - Tier 0: action.AlwaysInclude=true (manifest pin) + the
//     get_tool_details meta-tool (D4 registers it). Always present
//     in the LLM tools array.
//   - Tier 1: the RAG-top-K + LRU survivors + this-turn-promoted set,
//     capped at ToolTiersConfig.Tier1Cap. Full schemas in tools array.
//   - Tier 2: the next slice of RAG candidates (those not already in
//     Tier 0/1), capped at ToolTiersConfig.Tier2Cap. Surfaces as name
//     + 1-liner in the system prompt.
//   - Tier 3: every remaining available action, names-only in the
//     system prompt.
//
// LRU contract for Tier 1 (RFC §"Three-tier tool visibility"):
// `lru_rank` is the turn number of the tool's most recent reference,
// where "reference" = RAG-match above threshold this turn OR an LLM
// invocation OR a get_tool_details promotion. D2 implements the RAG-
// match + promotion path; D5 (in-loop invocation bump) and D4
// (promotion plumbing) wire the other two via the same KnownToolEntry
// fields.
//
// State persistence is shared with knowledge_dedup via
// state.InjectionState — KnownKnowledge stays the dedup decision's
// concern, KnownTools is owned here. The prepare/persist split
// honours RFC #249's EMIT→PERSIST ordering invariant.

// toolTierDecision is the per-turn tier outcome. Slices preserve a
// stable order (sorted alphabetically except where the RAG ranking
// is the semantically meaningful order — currently nowhere; Tier 1
// and Tier 2 are sorted by name to keep the event payload diff-able
// across turns). The event payload pulls Tier1New / Tier1Carried /
// Tier1EvictedToTier3 / Tier1SizeAfter / Tier1Cap + Tier2 / Tier2Cap
// (the latter for visibility into D3 promotion); the system-prompt
// renderer (D3) pulls Tier 2 and Tier 3.
type toolTierDecision struct {
	// Tier0 is the always_include set ∩ available tools. Always present
	// in the LLM tools array regardless of RAG score.
	Tier0 []string
	// Tier1 is the LRU-managed top tier — full schemas in tools array,
	// capped at Tier1Cap. Sorted alphabetically.
	Tier1 []string
	// Tier2 is the next slice of RAG candidates (≤ Tier2Cap), rendered
	// as name + 1-liner in the system prompt (D3).
	Tier2 []string
	// Tier3 is every remaining available action, rendered as names-only
	// grouped by capability in the system prompt (D3).
	Tier3 []string

	// Tier1New is the subset of Tier1 that wasn't tier="tier1" in the
	// prior known_tools. Promotion paths (Tier 2/3 → Tier 1) land here
	// alongside genuinely-new tools.
	Tier1New []string
	// Tier1Carried is the subset of Tier1 that survived from the prior
	// turn (was tier="tier1" before and still is). RFC's "tools that
	// did not need to move" bucket — useful for diff displays.
	Tier1Carried []string
	// Tier1EvictedToTier3 lists tools that were tier="tier1" in the
	// prior state but got pushed out by LRU cap pressure this turn.
	// Distinct from Tier1DemotedSticky, which is reserved for the D5
	// error-threshold demotion path (always empty in D2).
	Tier1EvictedToTier3 []string
	// Tier1DemotedSticky is the D5 sticky-demotion eviction bucket —
	// always empty here; D5 fills it when the error threshold trips.
	Tier1DemotedSticky []string
	// Tier1SizeAfter snapshots len(Tier1) for the event payload (saves
	// the consumer re-counting).
	Tier1SizeAfter int
	// Tier1Cap snapshots the config value at decision time so the event
	// stays interpretable even if the operator flips the cap between
	// turns.
	Tier1Cap int
	// Tier2Cap mirrors Tier1Cap for the Tier 2 bucket — emitted into the
	// preparer_decision event so consumers can verify Tier 2 was sized
	// as configured.
	Tier2Cap int
	// PromotedViaGetToolDetails is the D4 input slice — tools the LLM
	// pulled into Tier 1 via the meta-tool this turn. Echoed into the
	// event payload so consumers see the promotion provenance.
	PromotedViaGetToolDetails []string

	// UpdatedState is the post-decision InjectionState the caller
	// persists. KnownKnowledge is carried through unchanged from the
	// input (knowledge_dedup owns it); KnownTools is the new bookkeeping.
	UpdatedState state.InjectionState
}

// toolTierPoolEntry is one row in the Tier-1 selection pool: a tool
// name plus the (rank, demoted) snapshot the LRU sort consumes.
// Defined at file scope so the helpers below can pass slices of it
// around without anonymous-struct gymnastics.
type toolTierPoolEntry struct {
	Name    string
	LRURank int
	Demoted bool
}

// applyToolTierDecision runs the RFC #249 Phase 4 three-tier
// classification over the per-turn candidate set + LRU state.
//
// Algorithm:
//
//  1. Tier 0 = alwaysInclude ∩ availableTools (and never demoted by
//     RAG score; manifest authors are trusted).
//  2. Build the Tier-1 pool: RAG candidates this turn (rank=currentTurn)
//     ∪ promoted tools (rank=currentTurn) ∪ prior tier="tier1" entries
//     (rank=prior.LRURank). Tools also in Tier 0 are excluded.
//  3. Sort the pool by (Demoted asc, LRURank desc, ToolName asc) so
//     non-demoted high-rank tools win; demoted tools are eviction
//     candidates per RFC's "LRU treats demoted as preferred eviction".
//  4. Top Tier1Cap entries → Tier 1; the rest overflow. Overflow that
//     was tier="tier1" in prior state → Tier1EvictedToTier3.
//  5. Tier 2 = the next ≤ Tier2Cap RAG candidates not already in Tier 0
//     or Tier 1. Order preserves the input candidate ranking so
//     consumers see the same score-ordered slice the plugin returned.
//  6. Tier 3 = available - Tier 0 - Tier 1 - Tier 2, sorted by name.
//
// The pure-function contract means everything the caller needs to
// emit the preparer_decision event + render the system prompt is in
// the returned struct; no further reads are required.
func applyToolTierDecision(
	candidates []ToolCandidate,
	availableTools []string,
	alwaysInclude []string,
	promoted []string,
	prior state.InjectionState,
	cfg ToolTiersConfig,
	currentTurn int,
) toolTierDecision {
	availSet := stringSet(availableTools)
	alwaysSet := stringSet(alwaysInclude)
	promotedSet := stringSet(promoted)

	priorByName := make(map[string]state.KnownToolEntry, len(prior.KnownTools))
	for _, kt := range prior.KnownTools {
		priorByName[kt.ToolName] = kt
	}

	// Tier 0: always-include set intersected with availability.
	tier0 := sortedIntersection(alwaysSet, availSet)
	tier0Set := stringSet(tier0)

	// Tier 1 pool: candidates + promoted + prior tier="tier1".
	pool := make([]toolTierPoolEntry, 0, len(candidates)+len(promoted)+len(prior.KnownTools))
	inPool := make(map[string]bool, len(candidates)+len(promoted))
	addToPool := func(name string, freshRank int, useFreshRank bool) {
		if !availSet[name] || tier0Set[name] || inPool[name] {
			return
		}
		rank, demoted := 0, false
		if existing, ok := priorByName[name]; ok {
			rank, demoted = existing.LRURank, existing.Demoted
		}
		if useFreshRank && freshRank > rank {
			rank = freshRank
		}
		pool = append(pool, toolTierPoolEntry{Name: name, LRURank: rank, Demoted: demoted})
		inPool[name] = true
	}
	for _, c := range candidates {
		addToPool(c.ToolName, currentTurn, true)
	}
	for name := range promotedSet {
		addToPool(name, currentTurn, true)
	}
	for _, kt := range prior.KnownTools {
		if kt.Tier == "tier1" {
			addToPool(kt.ToolName, 0, false)
		}
	}

	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Demoted != pool[j].Demoted {
			return !pool[i].Demoted
		}
		if pool[i].LRURank != pool[j].LRURank {
			return pool[i].LRURank > pool[j].LRURank
		}
		return pool[i].Name < pool[j].Name
	})

	tier1 := make([]string, 0, cfg.Tier1Cap)
	tier1Set := make(map[string]bool, cfg.Tier1Cap)
	overflow := make([]toolTierPoolEntry, 0)
	for i, p := range pool {
		if i < cfg.Tier1Cap {
			tier1 = append(tier1, p.Name)
			tier1Set[p.Name] = true
			continue
		}
		overflow = append(overflow, p)
	}
	sort.Strings(tier1)

	// Tier1EvictedToTier3: pool overflow that was tier="tier1" before.
	var evicted []string
	for _, p := range overflow {
		if existing, ok := priorByName[p.Name]; ok && existing.Tier == "tier1" {
			evicted = append(evicted, p.Name)
		}
	}
	sort.Strings(evicted)

	// Tier1New / Tier1Carried split (operates on the SORTED tier1 slice
	// so payload buckets remain stable across turns).
	var tier1New, tier1Carried []string
	for _, name := range tier1 {
		if existing, ok := priorByName[name]; ok && existing.Tier == "tier1" {
			tier1Carried = append(tier1Carried, name)
		} else {
			tier1New = append(tier1New, name)
		}
	}

	// Tier 2: candidates not in Tier 0/1, in input (= score) order,
	// capped at Tier2Cap.
	tier2 := make([]string, 0, cfg.Tier2Cap)
	tier2Set := make(map[string]bool, cfg.Tier2Cap)
	for _, c := range candidates {
		if tier0Set[c.ToolName] || tier1Set[c.ToolName] || tier2Set[c.ToolName] {
			continue
		}
		if !availSet[c.ToolName] {
			continue
		}
		if len(tier2) >= cfg.Tier2Cap {
			break
		}
		tier2 = append(tier2, c.ToolName)
		tier2Set[c.ToolName] = true
	}

	// Tier 3: available - Tier 0/1/2.
	tier3 := make([]string, 0)
	for _, name := range availableTools {
		if tier0Set[name] || tier1Set[name] || tier2Set[name] {
			continue
		}
		tier3 = append(tier3, name)
	}
	sort.Strings(tier3)

	// Build updated known_tools. Every available tool that's been
	// "ranked" (Tier 0/1/2) plus every prior entry whose tool is still
	// available (preserves LRURank + Demoted across turns even if the
	// tool fell to Tier 3 this turn). Tools whose underlying action is
	// no longer available are dropped — cleanup for plugin
	// removal / profile change.
	updated := buildUpdatedKnownTools(
		tier0, tier1, tier2, availSet, priorByName, pool, currentTurn,
	)

	return toolTierDecision{
		Tier0:                     tier0,
		Tier1:                     tier1,
		Tier2:                     tier2,
		Tier3:                     tier3,
		Tier1New:                  tier1New,
		Tier1Carried:              tier1Carried,
		Tier1EvictedToTier3:       evicted,
		Tier1SizeAfter:            len(tier1),
		Tier1Cap:                  cfg.Tier1Cap,
		Tier2Cap:                  cfg.Tier2Cap,
		PromotedViaGetToolDetails: appendIfPresent(nil, promoted, availSet, tier0Set),
		UpdatedState: state.InjectionState{
			KnownKnowledge: append([]state.KnownKnowledgeEntry(nil), prior.KnownKnowledge...),
			KnownTools:     updated,
		},
	}
}

// buildUpdatedKnownTools assembles the new KnownTools slice from the
// per-tier results + the Tier-1 pool's accumulated lru_rank/demoted
// info. Pulled into a helper so the main algorithm reads top-down.
func buildUpdatedKnownTools(
	tier0, tier1, tier2 []string,
	availSet map[string]bool,
	priorByName map[string]state.KnownToolEntry,
	pool []toolTierPoolEntry,
	currentTurn int,
) []state.KnownToolEntry {
	entry := make(map[string]state.KnownToolEntry, len(pool)+len(tier0)+len(tier2))

	upsertWithRank := func(name, tier string, rank int, demoted bool) {
		cur, ok := entry[name]
		if !ok {
			cur = priorByName[name]
		}
		cur.ToolName = name
		cur.Tier = tier
		if rank > cur.LRURank {
			cur.LRURank = rank
		}
		if demoted {
			cur.Demoted = true
		}
		entry[name] = cur
	}

	for _, name := range tier0 {
		// Tier 0 is "always referenced" — bump to currentTurn so
		// downstream LRU sees them as freshest.
		upsertWithRank(name, "tier0", currentTurn, false)
	}
	tier1Set := stringSet(tier1)
	for _, p := range pool {
		t := "tier3"
		if tier1Set[p.Name] {
			t = "tier1"
		}
		upsertWithRank(p.Name, t, p.LRURank, p.Demoted)
	}
	for _, name := range tier2 {
		// Tier 2 is also a "reference" per RFC ("RAG-match above
		// threshold") so bump rank.
		upsertWithRank(name, "tier2", currentTurn, false)
	}
	// Carry forward prior entries whose tool is still available but
	// didn't land in any tier above (i.e. they're effectively Tier 3
	// this turn). Preserves their LRURank + Demoted for future LRU
	// rounds.
	for name, kt := range priorByName {
		if _, already := entry[name]; already {
			continue
		}
		if !availSet[name] {
			continue
		}
		kt.Tier = "tier3"
		entry[name] = kt
	}

	out := make([]state.KnownToolEntry, 0, len(entry))
	for _, kt := range entry {
		out = append(out, kt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ToolName < out[j].ToolName })
	return out
}

// stringSet builds a presence-only set from a slice. Same shape we
// already use in knowledge_dedup, but kept package-local because the
// underlying map[string]bool pattern is idiomatic enough that lifting
// it would only add indirection.
func stringSet(items []string) map[string]bool {
	if len(items) == 0 {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// sortedIntersection returns the intersection of two presence sets,
// sorted alphabetically for stable rendering.
func sortedIntersection(a, b map[string]bool) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	var out []string
	for k := range a {
		if b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// appendIfPresent appends names from src into dst when they're in
// availSet and NOT in excludeSet. Used to filter the promoted list
// down to "actually promotable" entries before echoing into the
// preparer_decision payload.
func appendIfPresent(dst, src []string, availSet, excludeSet map[string]bool) []string {
	if len(src) == 0 {
		return dst
	}
	for _, name := range src {
		if !availSet[name] || excludeSet[name] {
			continue
		}
		dst = append(dst, name)
	}
	sort.Strings(dst)
	return dst
}

// prepareToolTierDecision is the orchestrator's Phase-4 entry point
// for READ + COMPUTE of the tier decision. Mirrors the Phase-3
// prepareDedupDecision split: this method reads state, builds the
// decision, and stashes it on prepAgg.ToolTier for the emit step;
// persistToolTierDecision handles the write after the event has been
// emitted (EMIT→PERSIST invariant).
//
// When knowledge_dedup also ran this turn the prior-state-as-of-read
// comes from agg.KnowledgeDedup.UpdatedState so both decisions see a
// coherent base; the final persist piggy-backs on the dedup write
// (avoids double round-trip). When dedup didn't run we read state
// fresh — same robustness contract as Phase 3 (read error → start
// from zero so service stays available).
func (o *Orchestrator) prepareToolTierDecision(ctx context.Context, sessionID string, agg *preparerAggregate) {
	var prior state.InjectionState
	switch {
	case agg.KnowledgeDedup != nil:
		// Dedup already loaded + updated state; preserve its KnownKnowledge.
		prior = agg.KnowledgeDedup.UpdatedState
	default:
		var err error
		prior, err = o.injectionStateStore.GetInjectionState(ctx, sessionID)
		if err != nil {
			slog.WarnContext(ctx, "tool_tiers: read state failed, starting fresh",
				"component", "orchestrator", "session", sessionID, "error", err)
			prior = state.InjectionState{}
		}
	}

	available := o.availableToolNames(ctx)
	alwaysInclude := o.alwaysIncludeToolNames(ctx)
	promoted := o.promotedToolsThisTurn(ctx, sessionID) // D4 wires; nil here
	turn := o.turnNumberForDedup(sessionID)

	decision := applyToolTierDecision(
		agg.Tools, available, alwaysInclude, promoted, prior, o.toolTiers, turn,
	)

	// When dedup also ran, merge tier's KnownTools delta into the
	// dedup decision's UpdatedState so persistDedupDecision picks up
	// both. KnownKnowledge stays as the dedup decision left it.
	if agg.KnowledgeDedup != nil {
		merged := agg.KnowledgeDedup.UpdatedState
		merged.KnownTools = decision.UpdatedState.KnownTools
		agg.KnowledgeDedup.UpdatedState = merged
	}
	agg.ToolTier = &decision
}

// persistToolTierDecision writes the tier decision back to the
// session store ONLY when knowledge_dedup didn't run — otherwise the
// dedup persist already includes the tier's KnownTools delta (merged
// during prepareToolTierDecision). Same warn-and-continue contract as
// persistDedupDecision: a transient write failure is reconciled the
// next turn via the visible-message scan.
func (o *Orchestrator) persistToolTierDecision(ctx context.Context, sessionID string, agg *preparerAggregate) {
	if agg.ToolTier == nil || o.injectionStateStore == nil {
		return
	}
	if agg.KnowledgeDedup != nil {
		// Dedup persist already carries the merged tool state.
		return
	}
	if err := o.injectionStateStore.UpdateInjectionState(ctx, sessionID, agg.ToolTier.UpdatedState); err != nil {
		slog.WarnContext(ctx, "tool_tiers: write state failed, will reconcile next turn",
			"component", "orchestrator", "session", sessionID, "error", err)
	}
}

// availableToolNames returns the fully-qualified action names the
// current request can invoke — i.e. profile-allowed, non-preparer,
// non-guard, non-user-only. Mirrors the filter chain that
// buildToolDefinitions already runs, hoisted here so the tier
// decision and the system-prompt renderer (D3) share one source of
// truth. Guards are excluded for the same reason preparers are: they
// run pre-LLM-call to sanitise messages and never produce a
// user-callable surface.
func (o *Orchestrator) availableToolNames(ctx context.Context) []string {
	allowedPlugins, _ := ctx.Value(allowedPluginsKey{}).(cachedAllowedPlugins)
	preparerAction := preparerActionSet(o.preparers, o.guards)

	var names []string
	for _, cap := range o.registry.ListCapabilities() {
		if !o.pluginAllowed(cap, allowedPlugins) {
			continue
		}
		for _, action := range cap.Actions {
			fqn := cap.Name + "." + action.Name
			if preparerAction[fqn] || action.UserOnly {
				continue
			}
			names = append(names, fqn)
		}
	}
	sort.Strings(names)
	return names
}

// alwaysIncludeToolNames returns the subset of available tool names
// the plugin manifest pinned to Tier 0 via action.AlwaysInclude=true.
// The get_tool_details meta-tool (D4 registers it) lands here via the
// same path.
func (o *Orchestrator) alwaysIncludeToolNames(ctx context.Context) []string {
	allowedPlugins, _ := ctx.Value(allowedPluginsKey{}).(cachedAllowedPlugins)
	preparerAction := preparerActionSet(o.preparers, o.guards)

	var names []string
	for _, cap := range o.registry.ListCapabilities() {
		if !o.pluginAllowed(cap, allowedPlugins) {
			continue
		}
		for _, action := range cap.Actions {
			if !action.AlwaysInclude {
				continue
			}
			fqn := cap.Name + "." + action.Name
			if preparerAction[fqn] || action.UserOnly {
				continue
			}
			names = append(names, fqn)
		}
	}
	sort.Strings(names)
	return names
}

// preparerActionSet pre-computes the FQNs the preparer pipeline
// itself owns (both pre-LLM preparers and guard preparers) so
// they're excluded from LLM-visible tier buckets. Used by both
// availableToolNames and alwaysIncludeToolNames so the filter chain
// stays consistent with buildToolDefinitions / buildSystemPrompt,
// which both exclude o.preparers ∪ o.guards from the LLM surface.
func preparerActionSet(preparers, guards []ContentPreparerEntry) map[string]bool {
	out := make(map[string]bool, len(preparers)+len(guards))
	for _, prep := range preparers {
		out[prep.Plugin+"."+prep.Action] = true
	}
	for _, g := range guards {
		out[g.Plugin+"."+g.Action] = true
	}
	return out
}

// promotedToolsThisTurn is the D4 hook for the get_tool_details
// promotion path. Returns the set of tool fqns the LLM pulled into
// Tier 1 via get_tool_details in the current user turn. D2 always
// returns nil — the meta-tool isn't registered yet — but the tier
// decision already honours the slice when D4 wires it.
func (o *Orchestrator) promotedToolsThisTurn(_ context.Context, _ string) []string {
	return nil
}
