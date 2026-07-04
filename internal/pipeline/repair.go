package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// DefaultRepairTimeout bounds the tool-call corrector's LLM call. Independent
// of DefaultPlanTimeout for the same reason the confirmation classifier's
// timeout is: a repair attempt sits between a failed tool call and the user's
// answer, so "corrector unavailable -> fall back to the normal error flow"
// must stay snappy regardless of the multi-step plan budget.
const DefaultRepairTimeout = 10 * time.Second

// repairMaxTokens caps the corrector's output. The reply is a small JSON
// object; the headroom exists for a reasoning model that emits reasoning
// tokens BEFORE the JSON and for large repaired argument payloads (a batch
// call can carry a sizeable args object).
const repairMaxTokens = 2048

// RepairToolCallRequest is the corrector's deliberately tiny context: the
// tool's definition, the failed arguments, the error text, and (when the call
// went through a confirmation) the prompt the user approved — NOT the session
// history. Model optionally routes the side-call to a dedicated (typically
// stronger) corrector model; PromptOverride replaces the built-in
// instructions (it must preserve the JSON output contract or parsing fails
// and the repair falls back safely).
type RepairToolCallRequest struct {
	Model          string
	PromptOverride string
	ToolDefinition string            // rendered tool name + parameter names/descriptions
	FailedArgs     map[string]string // the arguments the failed attempt ran with
	ErrorText      string            // the failed attempt's error message
	ApprovedPrompt string            // confirmation prompt the user approved; "" when none
}

// RepairResult is the corrector's structured verdict: either repaired
// arguments (typed, as decoded from the model's JSON — numbers arrive as
// json.Number so large ids survive) or an explicit abort with a reason.
type RepairResult struct {
	RepairedArgs map[string]any
	Aborted      bool
	AbortReason  string
}

// RepairToolCall asks the LLM to fix the SHAPE of a failed tool call (rename
// mis-named parameters, restructure nesting) without changing any value. It
// is one-shot and strict: any LLM error, parse failure, or empty result is
// returned as an error so the caller falls back to the normal error flow —
// the corrector can never make a call worse, only fix it or step aside. The
// value-preservation rule is enforced twice: here in the prompt, and
// mechanically by the orchestrator's form-only guard on the returned args
// (global leaf-value multiset plus per-key value binding).
func (p *Planner) RepairToolCall(ctx context.Context, req RepairToolCallRequest) (RepairResult, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	instructions := repairToolCallPrompt
	if req.PromptOverride != "" {
		instructions = req.PromptOverride
	}
	var b strings.Builder
	b.WriteString(req.ToolDefinition)
	fmt.Fprintf(&b, "\nFailed arguments:\n%s\n", formatPendingArgs(req.FailedArgs))
	fmt.Fprintf(&b, "\nError:\n%s\n", req.ErrorText)
	if req.ApprovedPrompt != "" {
		fmt.Fprintf(&b, "\nThe user approved this action as:\n%s\n", req.ApprovedPrompt)
	}
	// Keep this call FAST, mirroring ClassifyConfirmation: cap output and dial
	// reasoning to low so a reasoning model can't blow the short repair timeout.
	resp, err := p.llm.Complete(ctx, &CompletionRequest{
		Model:           req.Model,
		Messages:        []Message{{Role: "system", Content: instructions}, {Role: "user", Content: b.String()}},
		MaxTokens:       repairMaxTokens,
		ReasoningEffort: "low",
	})
	if err != nil {
		return RepairResult{}, fmt.Errorf("repair tool call: %w", err)
	}
	var result struct {
		RepairedArgs map[string]any `json:"repaired_args"`
		Abort        bool           `json:"abort"`
		Reason       string         `json:"reason"`
	}
	// UseNumber keeps numeric arg values as json.Number so large record ids
	// don't collapse into float64 scientific notation on the way back out.
	dec := json.NewDecoder(strings.NewReader(extractJSON(resp.Content)))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		return RepairResult{}, fmt.Errorf("repair tool call parse: %w", err)
	}
	if result.Abort {
		return RepairResult{Aborted: true, AbortReason: result.Reason}, nil
	}
	if len(result.RepairedArgs) == 0 {
		return RepairResult{}, fmt.Errorf("repair tool call: corrector returned neither repaired_args nor abort")
	}
	return RepairResult{RepairedArgs: result.RepairedArgs}, nil
}

// repairToolCallPrompt is the corrector's built-in instructions. Kept as a Go
// const in this package (like confirmationClassifyPrompt) rather than an
// internal/prompts .txt: it is a side-call template, not a session system
// prompt, and operators override it via config when needed.
const repairToolCallPrompt = `A tool call failed BEFORE execution because its arguments did not match the tool's schema. Your ONLY job is to repair the SHAPE of the call: rename mis-named parameters, restructure nesting, or move a value from a mis-named field to the correct field, guided by the tool definition and the error.

STRICT RULES:
- NEVER change, add, or remove any VALUE. Every value in the repaired arguments must already appear in the failed arguments, and no value may be dropped.
- NEVER move a value away from a field that already has a correct name, and never swap values between fields.
- NEVER invent a missing value. If the error demands a value that is not present in the failed arguments, abort.
- Keep the SAME tool. Do not suggest a different tool or action.
- If you are not confident the call can be repaired without changing its substance, abort.

Respond ONLY with valid JSON, no prose. Either:
{"repaired_args": {"<param>": <value>, ...}}
or:
{"abort": true, "reason": "<one short sentence>"}`
