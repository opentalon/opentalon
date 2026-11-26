package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Tool-catalog discovery model. The native provider `tools` array carries
// only a small set — the always-include core (load_tools + any
// AlwaysInclude action) plus the tools the conversation has loaded since the
// session's last compaction (loadedTools). Every other allowed tool surfaces
// as a one-line CATALOG entry (name + first line of its description) in the
// system prompt, sourced directly from the Core tool registry. The LLM loads
// a catalog tool's full schema on demand via load_tools, or by calling it.

// loadedTools returns the on-demand tools the conversation conv has loaded,
// and marks on each tool result the ones the call it answers loaded
// (Message.LoadedTools), the place a request announces them. A call loads a
// tool when it is a _meta__load_tools call naming it or a call to it (an
// unloaded tool is loaded on call, see answerUnloadedCall), either way with its
// companions; only a call answered by a result in conv counts, so what a
// request offers and what it announces agree. conv starts at the session's
// last compaction, so a compaction drops every loaded tool.
func (o *Orchestrator) loadedTools(ctx context.Context, conv []provider.Message) map[string]bool {
	loaded := map[string]bool{}
	allowed, allowedPlugins := allowedToolsSet(ctx, o), o.resolveAllowedPlugins(ctx)
	var calls map[string]provider.ToolCall
	for i := range conv {
		m := &conv[i]
		var call provider.ToolCall
		switch {
		case m.Role == provider.RoleAssistant:
			calls = map[string]provider.ToolCall{}
			for _, tc := range callsOf(*m) {
				calls[tc.ID] = tc
			}
			continue
		case m.Role == provider.RoleTool:
			call = calls[m.ToolCallID]
		case provider.IsToolResult(*m):
			call = calls[""]
		}
		if call.Name == "" {
			continue
		}
		for _, fqn := range o.toolsLoadedBy(call, allowed, allowedPlugins) {
			if !loaded[fqn] {
				loaded[fqn] = true
				m.LoadedTools = append(m.LoadedTools, fqn)
			}
		}
	}
	return loaded
}

// callsOf returns the calls an assistant message makes: its native calls, or
// the one, without an id, of a text-format row as formatToolCallMessage wrote
// it, answered by the [plugin_output] row after it.
func callsOf(m provider.Message) []provider.ToolCall {
	if m.Role != provider.RoleAssistant {
		return nil
	}
	if len(m.ToolCalls) > 0 {
		return m.ToolCalls
	}
	body, ok := strings.CutPrefix(m.Content, "[tool_call] ")
	if !ok {
		return nil
	}
	call, ok := parseInlineToolCall(body, 1)
	if !ok {
		return nil
	}
	return []provider.ToolCall{{Name: toolFQN(call.Plugin, call.Action), Arguments: call.Args}}
}

// toolsLoadedBy returns the on-demand tools call loads, each followed by its
// companions: the tools a _meta__load_tools call names, or the tool any other
// call calls. A tool the session cannot see loads nothing; one always
// included loads only its companions, and only when a _meta__load_tools call
// names it, as that call's answer says (loadToolsExecutor).
func (o *Orchestrator) toolsLoadedBy(call provider.ToolCall, allowed map[string]struct{}, allowedPlugins cachedAllowedPlugins) []string {
	names, named := []string{call.Name}, false
	if plugin, action, err := parseToolName(call.Name); err == nil && plugin == metaPluginName && action == metaLoadTools {
		if names, named = splitToolNames(call.Arguments["names"]), true; len(names) == 0 {
			names = splitToolNames(call.Arguments["name"])
		}
	}
	var out []string
	for _, name := range names {
		fqn, visible := o.resolveVisibleTool(name, allowed, allowedPlugins)
		if !visible {
			continue
		}
		plugin, action, _ := parseToolName(fqn)
		switch a := o.resolveAction(plugin, action); {
		case a == nil, a.AlwaysInclude && !named:
			continue
		case !a.AlwaysInclude:
			out = append(out, fqn)
		}
		out = append(out, o.companionsOf(fqn, allowed, allowedPlugins)...)
	}
	return out
}

// toolIsNative reports whether an allowed action belongs to the native tools
// array the model receives in native-tools mode: the always-include core plus
// the tools the conversation has loaded (loadedTools). It is
// the SINGLE predicate that decides native-vs-catalog — buildToolDefinitions
// uses it to pick what to SEND the model, and renderToolCatalog uses its
// negation to list the rest as catalog entries — so a tool is in exactly one of
// the two surfaces. The tool-load gate then checks the actual SENT set (recorded
// on ctx, see sentNativeToolsKey), so what the model saw and what the gate
// admits are identical by construction, with no re-derivation.
//
// Profile / preparer / UserOnly filtering is orthogonal to "is native" and is
// applied separately by every caller (buildToolDefinitions, renderToolCatalog,
// and the plugin + user-only gates in executeCall), so it is deliberately NOT
// folded in.
func toolIsNative(action Action, fqn string, loaded map[string]bool) bool {
	return action.AlwaysInclude || loaded[fqn]
}

// callToUnloadedTool reports whether a model-originated call targets a tool that
// was NOT in the native tools array sent to the model this request — a catalog
// tool the model has not loaded via _meta__load_tools, so it is inventing
// arguments from the catalog's one-line summary (which carries no parameter
// schema). It is the shared decision behind the tool-load gate, consulted by the
// two sites that MUST agree:
//   - executeCall answers the call without running it (answerUnloadedCall, the
//     enforcement point in the agent loop);
//   - maybeRequireConfirmation declines to raise an approval prompt for it — a
//     call executeCall will not run must not cost the user a confirmation, so an
//     unloaded write never even reaches the approval question.
//
// It checks membership in the set actually sent this request (sentNativeTools on
// ctx), not a fresh derivation, so it cannot disagree with the array the model
// saw: a tool loaded by an earlier call in the SAME response is not run until
// its schema arrives on the next round. The action is
// resolved canonically (resolveAction applies the same LLM-mangling
// normalizations as executeCall), so the lookup keys off the registered name
// regardless of separator drift or a dropped MCP prefix in the model's name.
//
// Returns false (i.e. "the model may call it") when:
//   - no native array was sent for this request (text mode lists every tool in
//     full inline; the sub-agent loop does the same; a non-agent-loop caller
//     surfaced no tools) — nothing is "unloaded", so the gate is a no-op;
//   - the action is unresolvable — executeCall's not-found path owns that error,
//     so this gate must not mask it with a misleading "load it first".
func (o *Orchestrator) callToUnloadedTool(ctx context.Context, call ToolCall) bool {
	sent, ok := sentNativeToolsFromContext(ctx)
	if !ok {
		return false
	}
	action := o.resolveAction(call.Plugin, call.Action)
	if action == nil {
		return false
	}
	_, isNative := sent[toolFQN(call.Plugin, action.Name)]
	return !isNative
}

// answerUnloadedCall answers a model-originated call to a tool that was not in
// the native tools array sent this request (callToUnloadedTool). The call never
// runs: its arguments were guessed from a catalog line, which carries no
// parameters and none of the rules the tool's description states.
//   - A tool _meta__load_tools could load for this session (resolveVisibleTool)
//     is loaded, with its companions, by the call itself: the stored call and
//     this answer put it into the next request's tools (loadedTools). The
//     answer names the companions and tells the model to call the tool again,
//     directly, from its description (loadOnCallAnswer); it is marked NotRun,
//     and its tool_call_result has the status not_run.
//   - Anything else (a preparer or guard action, an action the session palette
//     excludes) is not available. Hidden plugins, user-only actions and unknown
//     names never get here: executeCall's earlier gates refuse them first.
func (o *Orchestrator) answerUnloadedCall(ctx context.Context, call ToolCall, dispatchStart time.Time) ToolResult {
	fqn := toolFQN(call.Plugin, call.Action)
	allowed, allowedPlugins := allowedToolsSet(ctx, o), o.resolveAllowedPlugins(ctx)
	canonical, visible := o.resolveVisibleTool(fqn, allowed, allowedPlugins)
	if !visible {
		slog.Warn("BLOCKED LLM call to a tool not available to this session", "plugin", call.Plugin, "action", call.Action)
		return o.emitRefusalResult(ctx, call, fmt.Sprintf("tool %q is not available", fqn), dispatchStart)
	}
	companions := o.companionsOf(canonical, allowed, allowedPlugins)
	slog.Info("LLM called an unloaded tool: loaded it, call not run",
		"plugin", call.Plugin, "action", call.Action, "companions", companions)
	answer := loadOnCallAnswer(canonical, companions)
	emit.EmitToolCallResult(ctx, o.eventSink, emit.ToolCallResultArgs{
		CallID:    call.ID,
		Status:    "not_run",
		Response:  answer,
		LatencyMS: time.Since(dispatchStart).Milliseconds(),
	})
	return ToolResult{CallID: call.ID, Error: answer, NotRun: true}
}

// loadOnCallAnswer's first sentence, with the tool's name as the first quoted
// string, is what measurements of chat sessions key on.
func loadOnCallAnswer(tool string, companions []string) string {
	loadTools := toolFQN(metaPluginName, metaLoadTools)
	answer := fmt.Sprintf("tool %q was NOT run: it was not loaded, so its parameters were not in your context. ", tool)
	if len(companions) == 0 {
		return answer + fmt.Sprintf("It is loaded now: its description and parameters are in your tools. "+
			"Read its description, then call %s again directly with the parameters it defines; "+
			"do not load it again with %s.", tool, loadTools)
	}
	names := strings.Join(companions, ", ")
	return answer + fmt.Sprintf("It is loaded now, together with %s: their descriptions and parameters are in your tools. "+
		"Read its description, then call %s again directly with the parameters it defines, "+
		"and call %s directly when you need them; do not load any of these tools again with %s.",
		names, tool, names, loadTools)
}

// retriesOfUnrunCalls returns the calls that repeat, with the same arguments, a
// call of the previous response that was answered without running (unrun holds
// those calls' signatures, see toolCallSignature).
func retriesOfUnrunCalls(calls []ToolCall, unrun map[string]bool) []ToolCall {
	var retries []ToolCall
	for i := range calls {
		if unrun[toolCallSignature(calls[i:i+1])] {
			retries = append(retries, calls[i])
		}
	}
	return retries
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
// array (i.e. not always-include and not loaded). Each entry is
// name + the first line of its description. The block nudges the LLM to
// load the tools it needs via load_tools before calling them.
//
// Source is the Core tool registry directly — no RAG, no tier decision.
// Walks o.registry.ListCapabilities(), skips plugins the profile gate
// blocks, and per action skips preparer/guard actions, UserOnly actions,
// and anything already native (AlwaysInclude or loaded). Returns "" when
// there's nothing to surface (the caller can append unconditionally).
func (o *Orchestrator) renderToolCatalog(loaded map[string]bool, allowedPlugins cachedAllowedPlugins) string {
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
			// Same predicate buildToolDefinitions sends by, so a tool is in
			// exactly one of the two surfaces (native array or catalog).
			if toolIsNative(action, fqn, loaded) {
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
	writeCatalogEntries(&sb, entries)
	return sb.String()
}

func renderInlineToolCatalog(callable, loadable []provider.ToolDefinition) string {
	var entries []catalogEntry
	for _, defs := range [][]provider.ToolDefinition{callable, loadable} {
		for _, td := range defs {
			if td.OnDemand {
				entries = append(entries, catalogEntry{fqn: td.Name, summary: firstLine(td.Description)})
			}
		}
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].fqn < entries[j].fqn })
	var sb strings.Builder
	sb.WriteString(prompts.OrchestratorToolCatalogInline)
	sb.WriteString("\n")
	writeCatalogEntries(&sb, entries)
	return sb.String()
}

func writeCatalogEntries(sb *strings.Builder, entries []catalogEntry) {
	for _, e := range entries {
		if e.summary == "" {
			fmt.Fprintf(sb, "- %s\n", e.fqn)
			continue
		}
		fmt.Fprintf(sb, "- %s: %s\n", e.fqn, e.summary)
	}
	sb.WriteString("\n")
}

// toolArraySet is the loaded set under which renderToolCatalog lists exactly the allowed tools the array lacks.
func toolArraySet(tools []provider.ToolDefinition) map[string]bool {
	set := make(map[string]bool, len(tools))
	for _, td := range tools {
		set[td.Name] = true
	}
	return set
}

// toolArrayNames is the set of the tools array's names, as the tool-load gate reads it.
func toolArrayNames(tools []provider.ToolDefinition) map[string]struct{} {
	set := make(map[string]struct{}, len(tools))
	for _, td := range tools {
		set[td.Name] = struct{}{}
	}
	return set
}

func withToolCatalog(segs provider.SystemSegments, catalog, inlineCatalog string) provider.SystemSegments {
	segs.Catalog = catalog
	segs.InlineCatalog = inlineCatalog
	return segs
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
