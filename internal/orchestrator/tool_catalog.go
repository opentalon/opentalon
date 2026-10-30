package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
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
// stay in the native tools array: the stored set (storedPromotedToolSet)
// plus the tools loaded this turn (turnToolLoads: through load_tools, on
// call, and their companions). The latter come from memory, so they stay in
// the array for the rest of the turn even when the store did not save their
// promotion, and they are exempt from the sticky cap, so a tool just
// loaded is never missing from the next request.
func (o *Orchestrator) promotedToolSet(ctx context.Context) map[string]bool {
	out := o.storedPromotedToolSet(ctx)
	for fqn := range turnToolLoadsFromContext(ctx).snapshot() {
		out[fqn] = true
	}
	return out
}

// storedPromotedToolSet reads the session's KnownTools, keeps the
// non-demoted entries, sorts by LRURank descending, and takes the top
// maxStickyTools. Returns an empty map when there's no session, no state
// store, or a read failure (logged) — the always-include core still
// renders, so the LLM is never left with no tools.
func (o *Orchestrator) storedPromotedToolSet(ctx context.Context) map[string]bool {
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
// the tools promoted into the session's sticky set via _meta__load_tools. It is
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
func toolIsNative(action Action, fqn string, promoted map[string]bool) bool {
	return action.AlwaysInclude || promoted[fqn]
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
// ctx), not a fresh read of session state, so it cannot disagree with the array
// the model saw: a tool that was sent stays admissible even if it is later
// demoted or evicted mid-turn, and a tool loaded by an earlier call in the SAME
// response is not run until its schema arrives on the next round. The action is
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
//     is loaded the way load_tools loads it — a sticky promotion in the session
//     store — and also recorded in the turn's turnToolLoads, so the next
//     request of the turn carries its schema whether or not the store saved the
//     promotion. Its companions are loaded with it (loadCompanions). The answer
//     names them and tells the model to call the tool again, directly, from its
//     description (loadOnCallAnswer); it is marked NotRun, and its
//     tool_call_result has the status not_run.
//   - A demoted tool keeps the answer it had before loading on call existed
//     (load it first): promoting it would clear the demotion.
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
	sessionID := actor.SessionID(ctx)
	if o.toolDemoted(ctx, sessionID, canonical) {
		loadTools := toolFQN(metaPluginName, metaLoadTools)
		slog.Warn("BLOCKED LLM call to unloaded demoted tool", "plugin", call.Plugin, "action", call.Action, "load_tools", loadTools)
		return o.emitRefusalResult(ctx, call, fmt.Sprintf(
			"tool %q is not loaded: its parameters are not in your context, so you cannot call it yet. "+
				"First call %s(names=%q), then call %s on your next step. Do not guess the parameters.",
			canonical, loadTools, canonical, canonical), dispatchStart)
	}
	if sessionID != "" && o.injectionStateStore != nil {
		o.persistToolPromotion(ctx, sessionID, canonical)
	}
	turnToolLoadsFromContext(ctx).record(canonical)
	companions := o.loadCompanions(ctx, canonical, allowed, allowedPlugins)
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

// toolDemoted reports whether the session store marks the tool demoted. No
// session, no store, no entry or a failed read all read as not demoted.
func (o *Orchestrator) toolDemoted(ctx context.Context, sessionID, fqn string) bool {
	if sessionID == "" || o.injectionStateStore == nil {
		return false
	}
	st, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		return false
	}
	for _, kt := range st.KnownTools {
		if kt.ToolName == fqn {
			return kt.Demoted
		}
	}
	return false
}

// turnToolLoads is one agent-loop turn's set of the tools loaded this turn, by
// either load path; outside the agent loop there is none (nil).
type turnToolLoads struct {
	mu    sync.Mutex // the load_tools executor records from its own goroutine, which outlives its deadline
	tools map[string]bool
}

type turnToolLoadsKey struct{}

func newTurnToolLoads() *turnToolLoads {
	return &turnToolLoads{tools: map[string]bool{}}
}

func withTurnToolLoads(ctx context.Context, loads *turnToolLoads) context.Context {
	return context.WithValue(ctx, turnToolLoadsKey{}, loads)
}

func turnToolLoadsFromContext(ctx context.Context) *turnToolLoads {
	loads, _ := ctx.Value(turnToolLoadsKey{}).(*turnToolLoads)
	return loads
}

func (l *turnToolLoads) record(fqn string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tools[fqn] = true
}

func (l *turnToolLoads) snapshot() map[string]bool {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]bool, len(l.tools))
	for fqn := range l.tools {
		out[fqn] = true
	}
	return out
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
			// Same predicate buildToolDefinitions sends by, so a tool is in
			// exactly one of the two surfaces (native array or catalog).
			if toolIsNative(action, fqn, promoted) {
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
