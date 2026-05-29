package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// RFC #249 Phase 4 D4 meta-tool: orchestrator-owned built-in that
// returns the full description + parameter schema for a Tier-2 or
// Tier-3 tool the LLM wants to call.
//
// Tier 2 / Tier 3 entries surface in the system prompt with only a
// one-line summary (Tier 2) or bare name (Tier 3), which is enough
// for the LLM to know they EXIST but not enough to invoke them
// safely. Calling _meta.get_tool_details(name="plugin.action")
// returns the full description + parameter schema AND persists a
// Tier-1 promotion in the session's KnownTools so the tool stays
// front-and-center for the rest of the session.
//
// Registration is gated by ToolTiersConfig.EnableGetToolDetails (the
// runtime-normalization step in NewWithRules upgrades the master
// switch when the meta-tool flag is set, so an operator who only
// enables this flag still gets the tier rendering it depends on).
// The action is marked AlwaysInclude=true so the tier decision pins
// it to Tier 0 regardless of RAG scoring — losing it would break
// the LLM's only on-demand schema-expansion path.
//
// Known limitation (deferred from RFC §"get_tool_details meta-tool"
// step 4): the promotion is durable across turns via KnownTools, but
// the SAME-turn next-iteration tools array isn't rebuilt — the LLM
// sees the description in this round-trip's tool_call_result and
// can act on it (especially in text-tool-call mode), but the native
// tools array refresh after a promotion is a separate follow-up
// (would require invalidating the agent loop's cachedTools and
// re-running buildToolDefinitions per iteration after a meta-call).

// metaPluginName is the orchestrator-owned plugin namespace for
// system meta-tools. Underscore prefix mirrors the existing
// _subprocess precedent — distinguishes a built-in from a user-
// configured plugin and keeps the namespace clear of collisions.
const metaPluginName = "_meta"

// metaGetToolDetails is the action name within the meta plugin. The
// fully-qualified name LLMs see is "_meta.get_tool_details".
const metaGetToolDetails = "get_tool_details"

// getToolDetailsExecutor implements PluginExecutor for the meta-tool.
// The closure-over-Orchestrator pattern matches subprocessExecutor —
// the handler needs registry + state-store access plus the per-call
// session id (pulled from ctx via actor.SessionID).
type getToolDetailsExecutor struct {
	orch *Orchestrator
}

// Execute looks up the named action in the registry, returns its
// full description + parameter schema as plain text, and persists a
// Tier-1 promotion so future turns keep the tool visible.
// Validation errors are returned as ToolResult.Error so the LLM sees
// the message and can correct its call (rather than the orchestrator
// silently swallowing it). The promotion-write side effect is
// best-effort: a store failure logs a warning and doesn't fail the
// call — the LLM already has the description in this round-trip.
func (e *getToolDetailsExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	name := strings.TrimSpace(call.Args["name"])
	if name == "" {
		return ToolResult{CallID: call.ID, Error: `missing "name" parameter (expected "plugin.action")`}
	}
	plugin, action, err := parseToolName(name)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("invalid tool name %q: %v", name, err)}
	}
	cap, ok := e.orch.registry.GetCapability(plugin)
	if !ok {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("plugin %q not found", plugin)}
	}
	// Profile-gate the INSPECTED plugin (not the _meta route-through, which
	// pluginAllowed always passes). Without this, an LLM in a profile-
	// restricted multi-tenant deployment could enumerate descriptions and
	// parameter schemas of plugins the operator hid via WhoAmI.Plugins /
	// AllowedGroups. Invocation is already blocked downstream by
	// executeCall's own gate, so this is information-disclosure-only — but
	// the disclosed surface IS what the operator's profile gate exists to
	// hide. Mirror the "plugin not found" branch shape so denied and non-
	// existent plugins are indistinguishable to the LLM. Denial happens
	// before persistToolPromotion below, so the side-effect write does
	// not fire on blocked requests.
	if !e.orch.pluginAllowed(cap, e.orch.resolveAllowedPlugins(ctx)) {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("plugin %q not found", plugin)}
	}
	var found *Action
	for i := range cap.Actions {
		if cap.Actions[i].Name == action {
			found = &cap.Actions[i]
			break
		}
	}
	if found == nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("action %q not found in plugin %q", action, plugin)}
	}

	// Action-level visibility gate. The plugin gate above is too coarse:
	// cap.Actions may contain an action that the session's tools/list filter
	// hides (e.g. an MCP backend whose manifest filter excludes admin tools
	// per auth path, leaving them in the OpenTalon registry only because a
	// downstream plugin instance with broader auth surfaced them on a sync).
	// Without this gate, the LLM could fetch the full description + parameter
	// schema of a tool it can never invoke — information disclosure around the
	// per-session palette. Returns the same "action not found" shape as the
	// missing-action branch so a denied lookup is indistinguishable from a
	// non-existent action; no existence oracle for filtered tools.
	//
	// Same palette computation as the `allowed_tools` ContextArgProvider
	// feeds — allowedToolsSet is the single source of truth for "what this
	// session can see" across both the RAG-retrieval vector (preparer
	// plugins consume the JSON form) and the direct-lookup vector here.
	allowed := allowedToolsSet(ctx, e.orch)
	if _, visible := allowed[name]; !visible {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("action %q not found in plugin %q", action, plugin)}
	}

	if sessionID := actor.SessionID(ctx); sessionID != "" && e.orch.injectionStateStore != nil {
		e.orch.persistToolPromotion(ctx, sessionID, name)
	}

	return ToolResult{
		CallID:  call.ID,
		Content: renderToolDescription(plugin, *found),
	}
}

// renderToolDescription formats one action as a plain-text block —
// "Tool: …", "Description: …", "Parameters: …". The LLM consumes
// the text directly via its tool_call_result, so the format
// prioritizes legibility over machine-parseability (it's not
// expected to be re-parsed).
func renderToolDescription(plugin string, a Action) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Tool: %s.%s\n\n", plugin, a.Name)
	fmt.Fprintf(&sb, "Description:\n%s\n", a.Description)
	if len(a.Parameters) == 0 {
		sb.WriteString("\nParameters: (none)\n")
		return sb.String()
	}
	sb.WriteString("\nParameters:\n")
	for _, p := range a.Parameters {
		req := ""
		if p.Required {
			req = " (required)"
		}
		fmt.Fprintf(&sb, "- %s%s: %s\n", p.Name, req, p.Description)
	}
	return sb.String()
}

// persistToolPromotion upserts a Tier-1 entry for the named tool in
// the session's KnownTools. Existing entries get their tier bumped
// to "tier1", LRURank refreshed to the current turn (so they win
// the next LRU sort), and Demoted=false — an explicit
// schema-request via get_tool_details is a strong relevance signal,
// matching the spirit of RFC §"Tool error handling" self-healing
// even though the tool itself hasn't been invoked yet. Missing
// entries are appended.
//
// Defensive copy of KnownTools mirrors the Phase-3
// applyKnowledgeDedup pattern: stores may return slices that share
// the backing array with their internal cache (the in-process
// fakeInjectionStateStore does, the DB-backed one in production
// doesn't), and mutating the read snapshot in place before a write
// could leave partially-applied state visible to a concurrent
// reader if the write later fails. Copying once keeps the path
// uniform across both store implementations.
//
// Read failures log a warning and return — the next turn's lazy
// reconciliation cannot recover a lost promotion, so we surface the
// failure for operator follow-up but don't fail the LLM's call.
// Write failures log similarly; the LLM still has the description
// from this round-trip.
func (o *Orchestrator) persistToolPromotion(ctx context.Context, sessionID, name string) {
	existing, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "get_tool_details: read state failed, promotion skipped",
			"component", "orchestrator", "session", sessionID, "tool", name, "error", err)
		return
	}
	updated := state.InjectionState{
		KnownKnowledge: existing.KnownKnowledge,
		KnownTools:     append([]state.KnownToolEntry(nil), existing.KnownTools...),
	}
	turn := o.turnNumberForDedup(sessionID)
	found := false
	for i := range updated.KnownTools {
		if updated.KnownTools[i].ToolName != name {
			continue
		}
		updated.KnownTools[i].Tier = state.KnownToolTier1
		if turn > updated.KnownTools[i].LRURank {
			updated.KnownTools[i].LRURank = turn
		}
		updated.KnownTools[i].Demoted = false
		found = true
		break
	}
	if !found {
		updated.KnownTools = append(updated.KnownTools, state.KnownToolEntry{
			ToolName: name,
			Tier:     state.KnownToolTier1,
			LRURank:  turn,
		})
	}
	if err := o.injectionStateStore.UpdateInjectionState(ctx, sessionID, updated); err != nil {
		slog.WarnContext(ctx, "get_tool_details: write state failed, promotion will not survive turn",
			"component", "orchestrator", "session", sessionID, "tool", name, "error", err)
		// Skip the recent-promotions cache when the persist failed: the
		// next preparer pass will read stale InjectionState that doesn't
		// have this tool at LRURank=turn, so flagging the tool as
		// "promoted via get_tool_details" in the event payload would
		// be misleading provenance.
		return
	}
	// Record the promotion in the per-session recents cache so the
	// NEXT preparer pass can surface it via promoted_via_get_tool_details
	// in preparer_decision. The InjectionState write above puts the
	// tool into Tier 1 via LRU rank regardless; this cache is purely
	// the provenance signal.
	o.recordRecentPromotion(sessionID, name)
}

// recordRecentPromotion appends a tool name to the per-session
// recents cache. Duplicate names within the same window are
// deduplicated — if the LLM calls get_tool_details for the same
// tool twice in one turn (unlikely but possible), the next preparer
// surfaces it once.
func (o *Orchestrator) recordRecentPromotion(sessionID, name string) {
	o.recentToolPromotionsMu.Lock()
	defer o.recentToolPromotionsMu.Unlock()
	existing := o.recentToolPromotions[sessionID]
	for _, n := range existing {
		if n == name {
			return
		}
	}
	o.recentToolPromotions[sessionID] = append(existing, name)
}

// registerGetToolDetailsTool wires the meta-tool into the registry
// so the LLM sees _meta.get_tool_details in its tools array and the
// Tier 2/3 hint text becomes actionable. Called from NewWithRules
// when ToolTiersConfig.EnableGetToolDetails is true.
func (o *Orchestrator) registerGetToolDetailsTool() {
	cap := PluginCapability{
		Name:        metaPluginName,
		Description: "Orchestrator-owned meta-tools.",
		Actions: []Action{{
			Name:          metaGetToolDetails,
			Description:   "Returns the full description and parameter schema for any tool listed in the system prompt's summary or other-tools sections. Calling it also promotes the tool back to the LLM's tools array for the rest of the session.",
			AlwaysInclude: true,
			// Pure lookup: returns a description string, mutates no
			// user-visible state. The state change (Tier-3 → Tier-1
			// promotion of the inspected tool) is orchestrator-internal
			// bookkeeping for the next turn, not an action the user
			// would want to confirm. Skipping the confirmation gate
			// here removes a noise prompt + planner-narration LLM call
			// every time the LLM asks for tool details.
			ReadOnly: true,
			Parameters: []Parameter{{
				Name:        "name",
				Description: `Fully-qualified tool name, e.g. "plugin.action".`,
				Required:    true,
			}},
		}},
	}
	_ = o.registry.Register(cap, &getToolDetailsExecutor{orch: o})
}
