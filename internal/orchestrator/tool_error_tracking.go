package orchestrator

import (
	"fmt"

	"github.com/opentalon/opentalon/internal/provider"
)

// turnToolErrors counts each tool's consecutive errors within one turn: at
// ToolErrorHandlingConfig.LoopCapPerTurn the next request of the turn carries
// a nudge — "Tool X failed N times in this turn — consider a different
// approach." — so the model stops slamming the same broken tool. A success
// resets the tool's count. Opt-in: a zero/unset LoopCapPerTurn sends no nudge.
type turnToolErrors map[string]int

// recordToolOutcome applies one call's outcome and returns the nudge for the
// next request, nil when none is due. A call answered without running
// (result.NotRun) neither failed nor succeeded and leaves the count as it was;
// a refused check (result.RefusedByCheck) counts like an error, since a
// repeated refused call is the loop the nudge breaks.
func (o *Orchestrator) recordToolOutcome(errs turnToolErrors, call ToolCall, result ToolResult) *provider.Message {
	if result.NotRun {
		return nil
	}
	fqn := toolFQN(call.Plugin, call.Action)
	if result.Error == "" && !result.RefusedByCheck {
		delete(errs, fqn)
		return nil
	}
	errs[fqn]++
	return o.loopCapNudge(fqn, errs[fqn])
}

// loopCapNudge is the message that tells the model to stop repeating a tool
// once turnCount, its errors in this turn, reaches LoopCapPerTurn; nil below
// it or when the cap is off.
func (o *Orchestrator) loopCapNudge(fqn string, turnCount int) *provider.Message {
	if o.toolErrorHandling.LoopCapPerTurn > 0 && turnCount >= o.toolErrorHandling.LoopCapPerTurn {
		// Carried back to the agent loop as a transient RoleUser message with
		// the "[system]" textual prefix the other retry nudges use. NOT a
		// RoleSystem message: the native Anthropic adapter folds every
		// RoleSystem message into the request's single `system` field, so a
		// nudge appended mid-array would contaminate the real system prompt.
		return &provider.Message{
			Role:    provider.RoleUser,
			Content: fmt.Sprintf("[system] Tool %s failed %d times in this turn — consider a different approach.", fqn, turnCount),
		}
	}
	return nil
}
