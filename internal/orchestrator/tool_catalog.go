package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// Tool-catalog discovery model. The native provider `tools` array carries
// only a small set — the always-include core (load_tools + any
// AlwaysInclude action) plus the tools the LLM has explicitly loaded this
// session (sticky). Every other allowed tool surfaces as a one-line
// CATALOG entry (name + first line of its description) in the system
// prompt, sourced directly from the Core tool registry. The LLM loads a
// catalog tool's full schema on demand via load_tools.

// maxStickyTools backstops the sticky (loaded) set: once promoted, a tool
// stays in the native tools array for the session, but the promoted set is
// capped here. When more tools are loaded than the cap, the most-recently-
// used win (highest LRURank). Always-include core tools are EXEMPT from
// the cap — they are never dropped.
const maxStickyTools = 40

// promotedToolSet returns the session's sticky tool set — the tools the
// LLM has loaded via load_tools (and any prior invocations) that should
// stay in the native tools array. Reads the session's KnownTools, keeps
// the non-demoted entries, sorts by LRURank descending, and takes the top
// maxStickyTools. Returns an empty map when there's no session, no state
// store, or a read failure (logged) — the always-include core still
// renders, so the LLM is never left with no tools.
func (o *Orchestrator) promotedToolSet(ctx context.Context) map[string]bool {
	sessionID := actor.SessionID(ctx)
	if sessionID == "" || o.injectionStateStore == nil {
		return map[string]bool{}
	}
	st, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "tool_catalog: read state failed, no sticky tools this request",
			"component", "orchestrator", "session", sessionID, "error", err)
		return map[string]bool{}
	}
	entries := make([]state.KnownToolEntry, 0, len(st.KnownTools))
	for _, kt := range st.KnownTools {
		if kt.Demoted {
			continue
		}
		entries = append(entries, kt)
	}
	// Most-recently-used win when over the cap. Tie-break by name so the
	// cap is deterministic across requests.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].LRURank != entries[j].LRURank {
			return entries[i].LRURank > entries[j].LRURank
		}
		return entries[i].ToolName < entries[j].ToolName
	})
	if len(entries) > maxStickyTools {
		entries = entries[:maxStickyTools]
	}
	out := make(map[string]bool, len(entries))
	for _, kt := range entries {
		out[kt.ToolName] = true
	}
	return out
}

// toolIsNative reports whether an allowed action belongs to the native tools
// array the model receives in native-tools mode: the always-include core plus
// the tools promoted into the session's sticky set via _meta__load_tools. It
// is the SINGLE predicate that defines the native set — buildToolDefinitions
// uses it to decide what to send the model, and the tool-load gate
// (callToUnloadedTool) uses it to decide what the model may call — so the tools
// the model sees in full and the calls the gate admits can never diverge.
//
// Profile / preparer / UserOnly filtering is orthogonal to "is native" and is
// applied separately by every caller (buildToolDefinitions here, the plugin +
// user-only gates in executeCall), so it is deliberately NOT folded in.
func toolIsNative(action Action, fqn string, promoted map[string]bool) bool {
	return action.AlwaysInclude || promoted[fqn]
}

// callToUnloadedTool reports whether a model-originated call targets a tool the
// model has not loaded — a native-mode catalog tool that is neither
// always-include core nor promoted via _meta__load_tools. Such a call means the
// model is inventing arguments from the catalog's one-line summary, which
// carries no parameter schema. It is the shared decision behind the tool-load
// gate, consulted by the two sites that MUST agree:
//   - executeCall refuses the call (the enforcement point present on every
//     dispatch path: agent loop, confirmation-resume, single-step);
//   - maybeRequireConfirmation declines to raise an approval prompt for it — a
//     call executeCall will refuse must not cost the user a confirmation, so an
//     unloaded write never even reaches the approval question.
//
// Returns false (i.e. "the model may call it") for:
//   - text mode — every allowed tool is listed in full inline in the system
//     prompt, so nothing is "unloaded";
//   - an unresolvable action — executeCall's not-found path owns that error, so
//     this gate must not mask it with a misleading "load it first";
//   - always-include core and already-loaded tools.
//
// The action name is resolved canonically (resolveAction applies the same
// LLM-mangling normalizations as executeCall), so the AlwaysInclude flag and
// the promoted-set lookup both key off the registered name regardless of
// separator drift or a dropped MCP prefix in the model-supplied name.
func (o *Orchestrator) callToUnloadedTool(ctx context.Context, call ToolCall) bool {
	if !o.supportsNativeTools() {
		return false
	}
	action := o.resolveAction(call.Plugin, call.Action)
	if action == nil {
		return false
	}
	return !toolIsNative(*action, toolFQN(call.Plugin, action.Name), o.promotedToolSet(ctx))
}

// catalogEntry is one row of the rendered tool catalog: the tool's
// fully-qualified name and the one-line summary (first line of its
// description).
type catalogEntry struct {
	fqn     string
	summary string
}

// renderToolCatalog returns the "## Tool catalog" system-prompt block
// listing every allowed tool that is NOT already in the native tools
// array (i.e. not always-include and not sticky-promoted). Each entry is
// name + the first line of its description. The block nudges the LLM to
// load the tools it needs via load_tools before calling them.
//
// Source is the Core tool registry directly — no RAG, no tier decision.
// Walks o.registry.ListCapabilities(), skips plugins the profile gate
// blocks, and per action skips preparer/guard actions, UserOnly actions,
// and anything already native (AlwaysInclude or promoted). Returns "" when
// there's nothing to surface (the caller can append unconditionally).
func (o *Orchestrator) renderToolCatalog(promoted map[string]bool, allowedPlugins cachedAllowedPlugins) string {
	preparerAction := o.preparerActions

	var entries []catalogEntry
	for _, cap := range o.registry.ListCapabilities() {
		if !o.pluginAllowed(cap, allowedPlugins) {
			continue
		}
		for _, action := range cap.Actions {
			fqn := toolFQN(cap.Name, action.Name)
			if preparerAction[fqn] || action.UserOnly {
				continue
			}
			// Already in the native tools array — no catalog entry needed.
			if action.AlwaysInclude || promoted[fqn] {
				continue
			}
			entries = append(entries, catalogEntry{fqn: fqn, summary: firstLine(action.Description)})
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].fqn < entries[j].fqn })

	var sb strings.Builder
	sb.WriteString("## Tool catalog — name + one-line summary\n")
	fmt.Fprintf(&sb, "These tools are available but NOT yet loaded — they are separate from the tools already in your available tools list, "+
		"which you call directly. Each line is a name + a one-line summary with NO parameter schema. "+
		"You MUST call `%s(names=\"plugin__action,plugin__action2\")` to load a tool's full schema BEFORE you call it, "+
		"then call it on your next step. Calling a catalog tool you have not loaded is REJECTED — you cannot see its "+
		"parameters, so never guess them from the summary.\n",
		toolFQN(metaPluginName, metaLoadTools))
	for _, e := range entries {
		if e.summary == "" {
			fmt.Fprintf(&sb, "- %s\n", e.fqn)
			continue
		}
		fmt.Fprintf(&sb, "- %s: %s\n", e.fqn, e.summary)
	}
	sb.WriteString("\n")
	return sb.String()
}

// preparerActionSet pre-computes the FQNs the preparer pipeline itself
// owns (both pre-LLM preparers and guard preparers) so they're excluded
// from the LLM-visible tool surface. Computed once in NewWithRules and
// read by buildToolDefinitions, buildSystemPrompt, renderToolCatalog, and
// allowedToolsSet so the filter chain stays consistent across them.
func preparerActionSet(preparers, guards []ContentPreparerEntry) map[string]bool {
	out := make(map[string]bool, len(preparers)+len(guards))
	for _, prep := range preparers {
		out[toolFQN(prep.Plugin, prep.Action)] = true
	}
	for _, g := range guards {
		out[toolFQN(g.Plugin, g.Action)] = true
	}
	return out
}

// firstLine returns the part of s before the first newline, trimmed of
// leading / trailing whitespace. Keeps catalog summaries to a single line
// even when the underlying action description spans multiple paragraphs.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
