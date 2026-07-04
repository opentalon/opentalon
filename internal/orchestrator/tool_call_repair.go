package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Tool-call repair phase.
//
// When a tool call fails the core's own pre-dispatch argument validation
// (rejectUnknownArgs — the tool provably never ran), the orchestrator does
// not immediately feed the error back to the main LLM — which would re-plan
// a brand-new call and, for privileged writes, re-enter the confirmation
// gate, burning one user approval per correction attempt. Instead a bounded
// repair loop runs: a one-shot corrector side-call (own planner, own model,
// own timeout — the confirmation-classifier pattern) fixes the SHAPE of the
// call, and the corrected call re-executes.
//
// Consent boundary: a repair re-executes either inside an approval the user
// actually granted (the pending-call approve path passes approved=true plus
// the approved prompt) or for calls that need no approval at all (read-only
// actions, no confirmation gate configured). A call that WOULD require
// confirmation but has no approval — e.g. a privileged write whose unknown
// argument names made the pre-confirmation validation skip the gate — is
// never repaired: its error falls back to the model, whose corrected
// re-plan then goes through the normal confirmation gate.
// ConfirmationBypass is set only when a real approval exists.
//
// The hard boundary is form-only repair: the corrector may rename unknown
// parameters and restructure nesting, never change substance. Enforced
// twice — in the corrector prompt (pipeline.RepairToolCall) and
// mechanically here (repairedArgsPreserveSubstance): the global scalar
// leaf-value multiset must be unchanged (empty arrays/objects count as
// values), and a value may only move away from its key when that key was
// NOT a declared parameter (i.e. an unknown key being renamed) — keys that
// keep their name keep their exact value, including array order and
// nested structure (repair only ever triggers on rejectUnknownArgs, which
// validates TOP-LEVEL argument names, so a legitimate shape repair never
// needs to rename keys inside a nested payload). The only residual trust
// left with the corrector prompt is values relocated away from
// renamed-away unknown top-level keys, which stay covered by the multiset
// check alone. The corrector also may not switch to a different plugin or
// action: the corrected call keeps both.
//
// Only failures that provably happened before dispatch are repairable,
// keyed on the typed ToolResult.ArgsInvalid flag set by executeCall's
// rejectUnknownArgs path — never on error-text matching, because
// plugin-authored error strings routinely embed downstream "invalid
// params"-style errors produced AFTER a partial mutation. (The design
// issue also listed tool-side schema rejections as repairable; that needs
// a structured pre-execution signal from the plugin transport, which does
// not exist yet, so they are deliberately NOT repairable.) When a
// corrected call is dispatched and fails, the corrected call and ITS error
// are what the caller records: session history must show the call that
// actually reached the tool, or the model would re-plan — and the user
// re-approve — a write that may already have run.
//
// Error-tracking integration: a repaired success resets the
// consecutive-error counters exactly like a normal success, but every
// corrector invocation is additionally counted per tool
// (recordRepairAttempt) and feeds the same sticky-demotion threshold, so a
// repair loop can never mask a tool whose schema chronically misleads the
// planner.

// defaultRepairMaxAttempts is the per-failed-call repair budget applied when
// RepairConfig.MaxAttempts is unset. Each attempt is one corrector side-call
// plus one re-execution; the first successful re-execution wins.
const defaultRepairMaxAttempts = 2

// isRepairableToolResult reports whether a failed tool result may enter the
// repair loop: only the typed pre-dispatch flag counts. Deliberately no
// error-text matching — tool error strings are plugin-authored free text
// and can embed schema-rejection phrases from downstream systems emitted
// after a partial mutation, where a same-approval re-execution would
// double-apply the write.
func isRepairableToolResult(r ToolResult) bool {
	return r.ArgsInvalid && r.Error != ""
}

// resolveAction returns the declared Action for plugin/action, mirroring
// executeCall's lookup: exact name first, then the LLM-mangling
// normalizations from actionNameCandidates. nil when unresolvable.
func (o *Orchestrator) resolveAction(pluginName, actionName string) *Action {
	cap, ok := o.registry.GetCapability(pluginName)
	if !ok {
		return nil
	}
	names := []string{actionName}
	names = append(names, actionNameCandidates(pluginName, actionName)...)
	for _, name := range names {
		for i := range cap.Actions {
			if cap.Actions[i].Name == name {
				return &cap.Actions[i]
			}
		}
	}
	return nil
}

// callRequiresConfirmation reports whether executing the call would
// require a user approval: the confirmation gate is configured, the
// action is not read-only, and the confirmation plugin requires
// confirmation for it. This is the shared head of the confirmation
// decision — maybeRequireConfirmation delegates here (after its own
// ConfirmationBypass check and the repair-gated unknown-args skip), and
// the repair phase's consent boundary asks it whether re-executing a
// failed call would need an approval that was never granted.
// checkConfirmationPlugin fails safe — a plugin error reads as "requires
// confirmation", which for repair means "do not repair".
func (o *Orchestrator) callRequiresConfirmation(ctx context.Context, call ToolCall) bool {
	if o.confirmationPlugin == "" || o.confirmationAction == "" ||
		o.registry.IsActionReadOnly(call.Plugin, call.Action) {
		return false
	}
	return o.checkConfirmationPlugin(ctx, []*pipeline.Step{{
		ID:      call.ID,
		Name:    call.Action,
		Command: &pipeline.PluginCommand{Plugin: call.Plugin, Action: call.Action},
	}}).RequiresConfirmation
}

// maybeRepairToolCall runs the bounded repair loop for a failed tool call.
// approved says whether the user actually approved this call (the
// pending-call approve path); approvedPrompt is the confirmation question
// they approved ("" when none was shown). Returns the call/result pair the
// caller must record plus true when that pair REPLACES the inputs: either
// a repaired re-execution succeeded, or a corrected call was dispatched
// and failed (the corrected call + its real error are then what history
// must carry). In every other case (repair disabled, error not repairable,
// missing approval, corrector abort or failure, guard rejection, budget
// exhausted — the tool never ran) it returns the inputs unchanged and
// false so the caller follows the normal error flow.
func (o *Orchestrator) maybeRepairToolCall(
	ctx context.Context, sessionID string, call ToolCall, failed ToolResult, approvedPrompt string, approved bool,
) (ToolCall, ToolResult, bool) {
	if o.repairer == nil || !isRepairableToolResult(failed) {
		return call, failed, false
	}
	action := o.resolveAction(call.Plugin, call.Action)
	if action == nil {
		return call, failed, false
	}
	log := logger.FromContext(ctx)
	// Consent boundary: without a real approval, only calls that need no
	// confirmation may be repaired. A privileged write reaches this point
	// unapproved exactly when its unknown argument names made the
	// pre-confirmation validation skip the gate — falling back here sends
	// the error to the model, whose corrected re-plan goes through the
	// normal gate.
	if !approved && o.callRequiresConfirmation(ctx, call) {
		log.Info("tool call repair: call requires a confirmation that was never granted; falling back to normal error flow",
			"plugin", call.Plugin, "action", call.Action)
		return call, failed, false
	}
	toolDef := renderActionDefinition(call.Plugin, action)

	lastCall, lastResult := call, failed
	for attempt := 1; attempt <= o.repairConfig.MaxAttempts; attempt++ {
		// Parent the repair span onto the failed attempt's tool_call_result /
		// tool_call_args_invalid event; the corrector's llm_request /
		// llm_response then nest under the repair sentinel.
		invokeCtx := ctx
		if lastResult.EventID != "" {
			invokeCtx = emit.WithParent(ctx, lastResult.EventID)
		}
		repairCtx := ctx
		if invokedID := emit.EmitToolCallRepairInvoked(invokeCtx, o.eventSink, emit.ToolCallRepairInvokedArgs{
			CallID:          call.ID,
			Plugin:          call.Plugin,
			Action:          call.Action,
			Attempt:         attempt,
			ValidationError: lastResult.Error,
		}); invokedID != "" {
			repairCtx = emit.WithParent(ctx, invokedID)
		}
		// Counted separately from the error counters (which a repaired
		// success resets) so chronic repair pressure still trips demotion.
		o.recordRepairAttempt(ctx, sessionID, call)

		res, err := o.repairer.RepairToolCall(repairCtx, pipeline.RepairToolCallRequest{
			Model:          o.repairConfig.Model,
			PromptOverride: o.repairConfig.Prompt,
			ToolDefinition: toolDef,
			FailedArgs:     lastCall.Args,
			ErrorText:      lastResult.Error,
			ApprovedPrompt: approvedPrompt,
		})
		if err != nil {
			log.Warn("tool call repair: corrector failed; falling back to normal error flow",
				"plugin", call.Plugin, "action", call.Action, "attempt", attempt, "error", err)
			return call, failed, false
		}
		if res.Aborted {
			log.Info("tool call repair: corrector aborted",
				"plugin", call.Plugin, "action", call.Action, "attempt", attempt, "reason", res.AbortReason)
			return call, failed, false
		}
		repairedArgs := pipelineArgsToWire(res.RepairedArgs)
		// Mechanical form-only guard: unknown-key renames and restructures
		// pass, any changed/added/removed value — or a value moved away
		// from a key that keeps its name — fails. This is the safety core:
		// a corrector that changes substance is rejected regardless of what
		// its prompt promised.
		if !repairedArgsPreserveSubstance(action, call.Args, repairedArgs) {
			log.Warn("tool call repair: corrector changed argument substance; repair rejected",
				"plugin", call.Plugin, "action", call.Action, "attempt", attempt)
			return call, failed, false
		}

		corrected := lastCall
		corrected.Args = repairedArgs
		// Same approval: only a call the user actually approved may skip
		// the confirmation gate on re-execution. Unapproved calls passed
		// the needs-no-confirmation check above and carry no bypass.
		corrected.ConfirmationBypass = approved
		result := o.executeCall(repairCtx, corrected)
		if result.Error == "" {
			emit.EmitToolCallRepaired(repairCtx, o.eventSink, emit.ToolCallRepairedArgs{
				CallID:    call.ID,
				Plugin:    call.Plugin,
				Action:    call.Action,
				Attempt:   attempt,
				Arguments: repairedArgs,
				Status:    "ok",
			})
			log.Info("tool call repaired",
				"plugin", call.Plugin, "action", call.Action, "attempt", attempt)
			return corrected, result, true
		}
		if !result.ArgsInvalid {
			// The corrected call reached (or may have reached) execution:
			// no further re-execution inside this approval, and the
			// corrected call + ITS error are what the caller records —
			// masking a dispatched attempt behind the original shape error
			// would let the model re-plan (and the user re-approve) a
			// write that may already have run.
			emit.EmitToolCallRepaired(repairCtx, o.eventSink, emit.ToolCallRepairedArgs{
				CallID:    call.ID,
				Plugin:    call.Plugin,
				Action:    call.Action,
				Attempt:   attempt,
				Arguments: repairedArgs,
				Status:    "error",
			})
			log.Info("tool call repair: repaired call was dispatched and failed; recording the corrected call and its error",
				"plugin", call.Plugin, "action", call.Action, "attempt", attempt, "error", result.Error)
			return corrected, result, true
		}
		lastCall, lastResult = corrected, result
	}
	log.Info("tool call repair: budget exhausted; falling back to normal error flow",
		"plugin", call.Plugin, "action", call.Action, "attempts", o.repairConfig.MaxAttempts)
	return call, failed, false
}

// renderActionDefinition renders the tool definition block the corrector
// sees: FQN, description, and the declared parameters. This — not the
// session history — is the corrector's whole world view of the tool.
func renderActionDefinition(pluginName string, action *Action) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tool: %s\n", toolFQN(pluginName, action.Name))
	if action.Description != "" {
		fmt.Fprintf(&b, "Description: %s\n", action.Description)
	}
	if len(action.Parameters) == 0 {
		b.WriteString("Parameters: (none)\n")
		return b.String()
	}
	b.WriteString("Parameters:\n")
	for _, p := range action.Parameters {
		req := ""
		if p.Required {
			req = " (required)"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", p.Name, req, p.Description)
	}
	return b.String()
}

// repairedArgsPreserveSubstance is the mechanical form-only guard on
// corrector output. Two checks, both must hold:
//
//  1. Global leaf multiset: the repaired args carry exactly the original
//     call's scalar leaf values (sameArgLeafValues) — nothing changed,
//     added, or removed, with empty arrays/objects counting as values so
//     a corrector cannot smuggle in e.g. a clear-this-collection [].
//  2. Key binding: a value may only move away from its key when that key
//     was NOT a declared parameter (an unknown key being renamed — the
//     only thing a shape repair legitimately renames). Every original key
//     that is declared, or that also appears in the repaired args, must
//     keep its exact value (canonicalArgValue: array order preserved,
//     nested key names and pairings included) — otherwise a corrector
//     could swap values between fields (from/to, item/user), including
//     siblings inside a nested payload, and invert the call's meaning
//     while the multiset stays identical.
//
// Values relocated from renamed unknown TOP-LEVEL keys remain covered
// only by the multiset check (their destination is by definition
// unknowable mechanically); that is the residual trust placed in the
// corrector prompt.
func repairedArgsPreserveSubstance(action *Action, original, repaired map[string]string) bool {
	if !sameArgLeafValues(original, repaired) {
		return false
	}
	declared := make(map[string]struct{}, len(action.Parameters))
	for _, p := range action.Parameters {
		declared[p.Name] = struct{}{}
	}
	for k, ov := range original {
		rv, kept := repaired[k]
		if _, isDeclared := declared[k]; !isDeclared && !kept {
			// Unknown key renamed away — its values are pooled into the
			// multiset check above.
			continue
		}
		if !kept || canonicalArgValue(ov) != canonicalArgValue(rv) {
			return false
		}
	}
	return true
}

// sameArgLeafValues reports whether two wire-level arg maps carry the same
// multiset of scalar leaf VALUES, ignoring key names and structure. Values
// that are themselves JSON (including JSON embedded in strings, e.g. a
// "tasks" argument holding a JSON array) are parsed and traversed, so a key
// rename inside a nested payload still passes while any changed, added, or
// removed leaf value fails. Empty arrays and objects count as leaves ("[]"
// / "{}") so adding or dropping one is a value change. JSON numbers compare
// exactly but spelling-leniently ("5.0" == 5, no float64 truncation for
// large ids); plain strings that are not valid JSON compare verbatim — a
// "007"-style identifier is a value, not a number spelling.
func sameArgLeafValues(original, repaired map[string]string) bool {
	return maps.Equal(argLeafValues(original), argLeafValues(repaired))
}

// argLeafValues collects the canonical-form multiset of scalar leaves across
// all values of a wire-level arg map.
func argLeafValues(args map[string]string) map[string]int {
	counts := make(map[string]int)
	for _, v := range args {
		collectLeafValues(parseArgValue(v), counts)
	}
	return counts
}

// parseArgValue decodes a wire string as JSON when it is exactly one valid
// JSON value (UseNumber keeps large ids precise); otherwise the raw string
// itself is the value. Requiring full consumption keeps "5 apples" a string
// instead of the number 5 followed by garbage.
func parseArgValue(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	if dec.More() {
		return s
	}
	return v
}

// collectLeafValues walks a decoded value and counts canonical scalar
// leaves. Strings that themselves contain JSON are parsed and traversed
// (nested-JSON-string arguments, e.g. a "tasks" value holding a JSON array);
// JSON-quoted strings unwrap one level so `"abc"` and `abc` count the same.
// Empty containers count as their own leaf tokens so the multiset sees a
// corrector adding or dropping an [] / {} argument.
func collectLeafValues(v any, counts map[string]int) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			counts["{}"]++
			return
		}
		for _, child := range t {
			collectLeafValues(child, counts)
		}
	case []any:
		if len(t) == 0 {
			counts["[]"]++
			return
		}
		for _, child := range t {
			collectLeafValues(child, counts)
		}
	case string:
		nested := parseArgValue(t)
		if s, isStr := nested.(string); isStr {
			if s == t {
				// Not JSON — a plain string leaf.
				counts[canonicalLeaf(t)]++
			} else {
				// JSON-quoted string: unwrap and keep walking (the inner
				// text may itself be JSON). s != t guarantees progress.
				collectLeafValues(s, counts)
			}
			return
		}
		// Container, number, bool, or null encoded in the string.
		collectLeafValues(nested, counts)
	default:
		counts[canonicalLeaf(t)]++
	}
}

// canonicalArgValue renders one wire arg value in a structural canonical
// form for the per-key binding check: scalars via canonicalLeaf (string
// leaves strconv.Quote'd so string content can never alias structure
// characters — '["a,b"]' and '["a","b"]' render differently), arrays keep
// element order, nested maps render as sorted quotedKey:value pairs —
// nested key names and their value pairings are part of a kept key's
// substance. Repair only ever triggers on rejectUnknownArgs, which
// validates TOP-LEVEL argument names, so a legitimate shape repair never
// needs to rename keys inside a nested payload; a corrector that does is
// rejected and the error falls back to the normal flow.
func canonicalArgValue(wire string) string {
	return canonicalizeParsed(parseArgValue(wire))
}

func canonicalizeParsed(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return "{}"
		}
		parts := make([]string, 0, len(t))
		for key, child := range t {
			parts = append(parts, strconv.Quote(key)+":"+canonicalizeParsed(child))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		parts := make([]string, len(t))
		for i, child := range t {
			parts[i] = canonicalizeParsed(child)
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		nested := parseArgValue(t)
		if s, isStr := nested.(string); isStr {
			if s == t {
				return strconv.Quote(t)
			}
			return canonicalizeParsed(s)
		}
		return canonicalizeParsed(nested)
	default:
		return canonicalLeaf(v)
	}
}

// canonicalLeaf renders one scalar in canonical form. JSON numbers
// collapse across spellings (json.Number "5.0" and "5" both render "5")
// because the corrector's typed JSON is converted back to wire strings
// before the guard runs — a respelling on that round-trip is form, not
// substance. Plain strings that are NOT valid JSON render verbatim: JSON
// has no way to spell "007" or "+49…" as a number, so a corrector turning
// them into 7 / 49… changed the value, and the guard must see that.
func canonicalLeaf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return canonicalNumber(string(t))
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

// canonicalNumber normalizes a numeric literal exactly: integer form when
// it fits int64, otherwise an exact rational rendering via math/big — no
// float64 round-trip, so ids beyond 19 digits stay distinct and "1e3"
// still equals "1000".
func canonicalNumber(s string) string {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(i, 10)
	}
	if r, ok := new(big.Rat).SetString(s); ok {
		return r.RatString()
	}
	return s
}
