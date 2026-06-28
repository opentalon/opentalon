package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// actionNameCandidates returns the forgiving normalizations of an LLM-supplied
// action name (underscore<->hyphen, and the dropped "<plugin>__" prefix that a
// bridged MCP server's tools carry). The execute path and load_tools both
// try these, so a tool resolves the SAME way whether it is loaded or invoked.
func actionNameCandidates(plugin, action string) []string {
	return []string{
		strings.ReplaceAll(action, "_", "-"),
		plugin + "__" + action,
		plugin + "__" + strings.ReplaceAll(action, "_", "-"),
		strings.ReplaceAll(action, "-", "_"),
		plugin + "__" + strings.ReplaceAll(action, "-", "_"),
	}
}

// load_tools is the orchestrator-owned discovery meta-tool. The system
// prompt renders a one-line CATALOG (name + first-line-of-description)
// of every tool the LLM doesn't yet have in its native tools array.
// Those tools carry only a summary — enough to know they EXIST, not
// enough to invoke them safely. Calling
// _meta__load_tools(names="plugin__a,plugin__b") pulls the named tools
// into the LLM's native tools array (with full schemas) for the rest of
// the session: each named tool gets a sticky KnownTools promotion, and
// the rebuilt tools array on the NEXT request carries its full schema.
//
// load_tools returns STATUS ONLY — a JSON string like
// {"loaded":["plugin__a"],"ready":true} (with "failed":[...] for names
// that didn't resolve or aren't visible to this session). It does NOT
// return the description / parameter schema; that surfaces in the native
// tools array on the next request via the sticky promotion. The contract
// for the LLM is: call load_tools first, then call the loaded tools on
// the next step — never guess parameters from a one-line summary.
//
// The action is AlwaysInclude=true (it is the core discovery mechanism,
// so it must always be present in the tools array) and ReadOnly=true
// (pure bookkeeping, no user-confirmation gate).

// metaPluginName is the orchestrator-owned plugin namespace for
// system meta-tools. Underscore prefix mirrors the existing
// _subprocess precedent — distinguishes a built-in from a user-
// configured plugin and keeps the namespace clear of collisions.
const metaPluginName = "_meta"

// metaLoadTools is the action name within the meta plugin. The
// fully-qualified name LLMs see is "_meta__load_tools".
const metaLoadTools = "load_tools"

// loadToolsExecutor implements PluginExecutor for the meta-tool.
// The closure-over-Orchestrator pattern matches subprocessExecutor —
// the handler needs registry + state-store access plus the per-call
// session id (pulled from ctx via actor.SessionID).
type loadToolsExecutor struct {
	orch *Orchestrator
}

// loadToolsResult is the status-only payload load_tools returns. loaded
// carries the canonical FQNs that were promoted this call; failed carries
// the names that didn't resolve or aren't visible to this session (omitted
// when empty). ready is true whenever at least one tool loaded.
type loadToolsResult struct {
	Loaded []string `json:"loaded"`
	Ready  bool     `json:"ready"`
	Failed []string `json:"failed,omitempty"`
}

// Execute resolves each requested tool name, applies the same profile +
// action-visibility gates the invoke path applies, and persists a sticky
// promotion for the ones that pass. It returns a STATUS JSON string
// (loaded / ready / failed) — never the description or parameter schema.
// The full schema reaches the LLM via the native tools array on the next
// request, once buildToolDefinitions picks up the promotion through
// promotedToolSet.
//
// Batch contract: names is a comma-separated list (tool-call args are
// map[string]string), with a single-name fallback. A failure on one name
// adds it to failed and does NOT abort the rest of the batch. The
// promotion write is best-effort: a store failure logs a warning and the
// name still counts as loaded for this round-trip (the next request
// rebuild simply won't see it).
func (e *loadToolsExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	names := splitToolNames(call.Args["names"])
	if len(names) == 0 {
		names = splitToolNames(call.Args["name"])
	}
	if len(names) == 0 {
		return ToolResult{CallID: call.ID, Error: `missing "names" parameter (expected a comma-separated list of "plugin__action" names)`}
	}

	sessionID := actor.SessionID(ctx)
	allowed := allowedToolsSet(ctx, e.orch)
	allowedPlugins := e.orch.resolveAllowedPlugins(ctx)

	var loaded, failed []string
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		canonical, ok := e.resolveVisibleTool(name, allowed, allowedPlugins)
		if !ok {
			failed = append(failed, name)
			continue
		}
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		if sessionID != "" && e.orch.injectionStateStore != nil {
			e.orch.persistToolPromotion(ctx, sessionID, canonical)
		}
		loaded = append(loaded, canonical)
	}

	payload := loadToolsResult{
		Loaded: loaded,
		Ready:  len(loaded) > 0,
		Failed: failed,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		// Marshalling a slice of strings + two scalars cannot realistically
		// fail; surface it as an error rather than returning malformed JSON.
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("load_tools: encode result failed: %v", err)}
	}
	return ToolResult{CallID: call.ID, Content: string(body)}
}

// resolveVisibleTool maps one LLM-supplied tool name to its canonical
// FQN, applying the same profile gate (the INSPECTED plugin, not the
// _meta route-through) and action-visibility gate (allowedToolsSet) the
// invoke path applies. Returns (canonicalFQN, true) when the tool
// resolves AND is visible to this session; ("", false) otherwise.
//
// The two gates are why load_tools cannot enumerate tools the operator
// hid: the profile gate blocks plugins hidden via WhoAmI.Plugins /
// AllowedGroups, and allowedToolsSet — the single source of truth for
// "what this session can see" — blocks actions filtered out of the
// per-session palette (preparer/guard actions, UserOnly actions, MCP
// tools an upstream manifest filter excluded for this auth path).
func (e *loadToolsExecutor) resolveVisibleTool(
	name string, allowed map[string]struct{}, allowedPlugins cachedAllowedPlugins,
) (string, bool) {
	plugin, action, err := parseToolName(name)
	if err != nil {
		return "", false
	}
	cap, ok := e.orch.registry.GetCapability(plugin)
	if !ok {
		return "", false
	}
	if !e.orch.pluginAllowed(cap, allowedPlugins) {
		return "", false
	}
	found := false
	for i := range cap.Actions {
		if cap.Actions[i].Name == action {
			found = true
			break
		}
	}
	if !found {
		// Apply the same forgiving resolution as the execute path so a tool can
		// be LOADED by every name it can be INVOKED by — notably a bridged MCP
		// tool addressed as "<server>__<tool>" instead of the canonical
		// "<server>__<server>__<tool>".
		for _, candidate := range actionNameCandidates(plugin, action) {
			for i := range cap.Actions {
				if cap.Actions[i].Name == candidate {
					action = candidate
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}
	if !found {
		return "", false
	}
	// Re-compose the canonical FQN from the resolved (plugin, action) so the
	// visibility check and the promotion key match allowedToolsSet's keys
	// regardless of the separator (or dropped MCP prefix) the LLM passed in.
	canonical := toolFQN(plugin, action)
	if _, visible := allowed[canonical]; !visible {
		return "", false
	}
	return canonical, true
}

// splitToolNames splits a comma-separated tool-name list into trimmed,
// non-empty entries. Tool-call args are map[string]string and the param
// schema type is "string", so a batch arrives as one comma-joined value.
func splitToolNames(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// persistToolPromotion upserts a sticky entry for the named tool in the
// session's KnownTools so future requests keep the tool in the native
// tools array. Existing entries get LRURank refreshed to the current
// turn (so they win the sticky-cap sort), Demoted cleared, and the tier
// marked tier1 — an explicit load_tools call is a strong relevance
// signal. Missing entries are appended.
//
// KnownTools is defensively copied before mutation: stores may return
// slices that share the backing array with their
// internal cache (the in-process fakeInjectionStateStore does, the
// DB-backed one in production doesn't), and mutating the read snapshot in
// place before a write could leave partially-applied state visible to a
// concurrent reader if the write later fails. Copying once keeps the path
// uniform across both store implementations.
//
// Read/write failures log a warning and return — the LLM's load_tools
// call already succeeded for this round-trip; only the durable promotion
// is lost (the next request's rebuild won't see the tool until a
// successful write).
func (o *Orchestrator) persistToolPromotion(ctx context.Context, sessionID, name string) {
	existing, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "load_tools: read state failed, promotion skipped",
			"component", "orchestrator", "session", sessionID, "tool", name, "error", err)
		return
	}
	updated := state.InjectionState{
		KnownTools: append([]state.KnownToolEntry(nil), existing.KnownTools...),
	}
	turn := o.sessionTurnNumber(sessionID)
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
		slog.WarnContext(ctx, "load_tools: write state failed, promotion will not survive request",
			"component", "orchestrator", "session", sessionID, "tool", name, "error", err)
		return
	}
	o.recordRecentPromotion(sessionID, name)
}

// recordRecentPromotion appends a tool name to the per-session
// recents cache. Duplicate names within the same window are
// deduplicated — if the LLM loads the same tool twice in one turn
// (unlikely but possible), the recents cache surfaces it once.
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

// registerLoadToolsTool wires the meta-tool into the registry so the LLM
// sees _meta__load_tools in its tools array and the system-prompt catalog
// becomes actionable. Registered unconditionally — it is the core
// discovery mechanism. AlwaysInclude pins it into the always-present core
// set so the LLM always has a path to load catalog tools.
func (o *Orchestrator) registerLoadToolsTool() {
	cap := PluginCapability{
		Name:        metaPluginName,
		Description: "Orchestrator-owned meta-tools.",
		Actions: []Action{{
			Name: metaLoadTools,
			Description: "Load one or more tools listed in the system prompt's tool catalog into your available tools so you can call them. " +
				"The catalog shows each tool's name and a one-line summary; that summary has NO parameters. " +
				`Call load_tools(names="plugin__action,plugin__action2") with the catalog names you need, ` +
				"then call those tools on your NEXT step (their full parameter schemas appear in your tools then). " +
				"Never guess a tool's parameters from its one-line summary. " +
				`Returns a JSON status, e.g. {"loaded":["plugin__action"],"ready":true}.`,
			AlwaysInclude: true,
			// Pure bookkeeping: marks the named tools as loaded for the
			// rest of the session and returns a status string. No
			// user-visible state changes, so skipping the confirmation
			// gate removes a noise prompt + planner-narration LLM call
			// every time the LLM loads catalog tools.
			ReadOnly: true,
			Parameters: []Parameter{{
				Name:        "names",
				Description: `Comma-separated fully-qualified tool names to load, e.g. "plugin__action,plugin__action2".`,
				Required:    true,
			}},
		}},
	}
	_ = o.registry.Register(cap, &loadToolsExecutor{orch: o})
}
