package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RFC #249 Phase 4 system-prompt rendering for Tier 2 + Tier 3.
//
// Tier 0 + Tier 1 tools land in the LLM's native `tools` array with
// full schemas (buildToolDefinitions filters by the relevant_tools
// set that prepareToolTierDecision narrows to Tier 0 + Tier 1).
// Tier 2 + Tier 3 land in the system prompt as text so the LLM
// knows they exist without paying the per-tool full-schema cost:
//
//   - Tier 2: name + one-line summary (~20 tokens per tool)
//   - Tier 3: names-only, grouped by plugin (~8 tokens per tool)
//
// Both tier sections nudge the LLM to call `get_tool_details(name=…)`
// before invoking a not-yet-fully-defined tool — that meta-tool is
// the D4 promotion path. D3 lands the system-prompt copy + the
// surrounding plumbing; D4 will register the actual meta-tool so
// the nudge is actionable.

// toolTierDecisionKey is the context key the orchestrator uses to
// stash the per-turn tier decision so buildSystemPrompt can read it
// during agent-loop iterations. Mirrors the relevantToolsKey
// precedent — empty struct, pointer value.
type toolTierDecisionKey struct{}

// withToolTierDecision stores the tier decision in ctx so the
// system-prompt builder can render Tier 2 + Tier 3 sections from it.
// Pass nil to clear (rare — used by tests that need to invalidate a
// previously-stashed decision).
func withToolTierDecision(ctx context.Context, d *toolTierDecision) context.Context {
	return context.WithValue(ctx, toolTierDecisionKey{}, d)
}

// toolTierDecisionFromContext returns the tier decision stashed by
// the preparer-loop wiring, or nil when tiers weren't active this
// turn. Callers must tolerate nil (the pre-Phase-4 path doesn't
// produce one).
func toolTierDecisionFromContext(ctx context.Context) *toolTierDecision {
	d, _ := ctx.Value(toolTierDecisionKey{}).(*toolTierDecision)
	return d
}

// renderTier2Section returns a markdown block listing Tier 2 tools
// with one-line summaries. Each line: "- plugin__action: <summary>".
// The summary is the first line of the action's description so a
// multi-paragraph description (typically intended for the full
// schema) doesn't bloat the system prompt. Returns an empty string
// when the tier has no entries — the caller can append unconditionally
// without producing a stray header.
func renderTier2Section(decision *toolTierDecision, registry *ToolRegistry) string {
	if decision == nil || len(decision.Tier2) == 0 {
		return ""
	}
	descByFQN := actionDescriptionMap(registry, decision.Tier2)
	var sb strings.Builder
	sb.WriteString("## Tool catalog — name + one-line summary\n")
	fmt.Fprintf(&sb, "Pick the tool(s) whose summary fits the request. The summary has NO parameters: you MUST call `%s(name=\"plugin__action\")` to read a tool's full description and parameters BEFORE invoking it — never guess parameters from a summary. When the summary makes the choice clear, fetch that one tool's details and proceed; when several could fit, fetch the top candidates' details and compare first.\n", toolFQN(metaPluginName, metaGetToolDetails))
	for _, fqn := range decision.Tier2 {
		summary := firstLine(descByFQN[fqn])
		if summary == "" {
			fmt.Fprintf(&sb, "- %s\n", fqn)
			continue
		}
		fmt.Fprintf(&sb, "- %s: %s\n", fqn, summary)
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderTier3Section returns a names-only block grouped by plugin.
// Format:
//
//	## Other available tools (request details before use)
//	- <plugin>: action1, action2, …
//
// Plugins (groups) and actions within a plugin are sorted
// alphabetically so the system prompt is reproducible across
// orchestrator restarts. The header nudges the LLM toward
// get_tool_details for any Tier-3 entry it wants to actually call.
func renderTier3Section(decision *toolTierDecision) string {
	if decision == nil || len(decision.Tier3) == 0 {
		return ""
	}
	byPlugin := groupTier3ByPlugin(decision.Tier3)
	if len(byPlugin) == 0 {
		return ""
	}
	plugins := sortedKeys(byPlugin)
	var sb strings.Builder
	sb.WriteString("## Other available tools (request details before use)\n")
	fmt.Fprintf(&sb, "These tools exist but their full schemas aren't loaded. Call `%s(name=\"plugin__action\")` to see parameters before invoking.\n", toolFQN(metaPluginName, metaGetToolDetails))
	for _, plugin := range plugins {
		actions := byPlugin[plugin]
		fmt.Fprintf(&sb, "- %s: %s\n", plugin, strings.Join(actions, ", "))
	}
	sb.WriteString("\n")
	return sb.String()
}

// groupTier3ByPlugin splits the Tier 3 fqn list into a
// plugin→[action,…] map, sorting the actions within each group so
// the output is stable. fqns that don't parse as plugin__action are
// silently skipped — those would be a malformed input.
func groupTier3ByPlugin(fqns []string) map[string][]string {
	out := make(map[string][]string)
	for _, fqn := range fqns {
		plugin, action, err := parseToolName(fqn)
		if err != nil {
			continue
		}
		out[plugin] = append(out[plugin], action)
	}
	for plugin := range out {
		sort.Strings(out[plugin])
	}
	return out
}

// actionDescriptionMap walks the registry once to collect
// fqn→description entries for the subset named in fqns. The
// registry iteration is O(plugins × actions) but the result map is
// O(|fqns|), so callers can do a fast lookup per Tier-2 entry
// without re-scanning the registry for each.
func actionDescriptionMap(registry *ToolRegistry, fqns []string) map[string]string {
	want := make(map[string]bool, len(fqns))
	for _, fqn := range fqns {
		want[fqn] = true
	}
	out := make(map[string]string, len(fqns))
	for _, cap := range registry.ListCapabilities() {
		for _, a := range cap.Actions {
			fqn := toolFQN(cap.Name, a.Name)
			if want[fqn] {
				out[fqn] = a.Description
			}
		}
	}
	return out
}

// firstLine returns the part of s before the first newline,
// trimmed of leading / trailing whitespace. Used to keep Tier 2
// summaries to a single line even when the underlying action
// description spans multiple paragraphs of markdown.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
