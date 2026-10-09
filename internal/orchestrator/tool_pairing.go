package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// unrecordedToolResultNotice answers a tool call whose result never reached
// the session history. The call may well have run — the usual cause is a lost
// write of the result row — so the model is told to check before repeating it,
// not that it failed: repeating a write that did run would duplicate it.
const unrecordedToolResultNotice = "NO RESULT RECORDED — the result of this call was lost before it was saved. The call may or may not have executed. Check its effect (for example by reading the affected record) before issuing it again."

// toolPairingReport lists what pairToolMessages changed; empty when the
// request was already valid.
type toolPairingReport struct {
	UnansweredCallIDs []string // kept, answered with unrecordedToolResultNotice
	UnansweredTools   []string // tool names of UnansweredCallIDs, same order
	MovedResultIDs    []string // found later in the history, moved back to their call
	OrphanResultIDs   []string // no call before them; dropped
	DroppedCallCount  int      // empty or repeated id; dropped
}

func (r toolPairingReport) empty() bool {
	return len(r.UnansweredCallIDs) == 0 && len(r.MovedResultIDs) == 0 &&
		len(r.OrphanResultIDs) == 0 && r.DroppedCallCount == 0
}

// pairToolMessages enforces the native tool-calling invariant on an assembled
// request: every tool call is answered by a result in the messages right after
// its assistant message, and every result answers a call in the assistant
// message right before it. Providers reject a request that breaks it
// (Anthropic: "unexpected tool_use_id found in tool_result blocks" / "tool_use
// ids were found without tool_result blocks immediately after"), which fails
// the turn and every later turn that rebuilds the same history.
//
// History can break the invariant in ways assembly cannot rule out: a failed
// write of one half of a pair (session writes are best-effort), a message
// appended between the two halves by another writer, or the store's
// max-messages trim. This is the last line of defence, run on the finished
// request:
//   - a call's result found later in the history is moved back to the call;
//   - a call with no result is kept and answered with
//     unrecordedToolResultNotice, so the model checks instead of repeating a
//     call that may have run;
//   - a result with no call before it is dropped — there is no call to
//     reconstruct it from;
//   - a call with an empty or repeated id cannot be answered and is dropped
//     (an assistant message left with neither calls nor text goes with it).
//
// Text-format [plugin_output] turns are plain messages to the provider and are
// left alone.
func pairToolMessages(msgs []provider.Message) ([]provider.Message, toolPairingReport) {
	var report toolPairingReport

	// Where each call id's results sit, in order, so a misplaced result can be
	// found from its call.
	resultsByID := make(map[string][]int)
	for i, m := range msgs {
		if m.Role == provider.RoleTool && m.ToolCallID != "" {
			resultsByID[m.ToolCallID] = append(resultsByID[m.ToolCallID], i)
		}
	}
	used := make([]bool, len(msgs))
	// nextResult returns the first unused result for id after position i.
	nextResult := func(id string, i int) (int, bool) {
		for _, j := range resultsByID[id] {
			if j > i && !used[j] {
				return j, true
			}
		}
		return 0, false
	}

	out := make([]provider.Message, 0, len(msgs))
	for i, m := range msgs {
		if used[i] {
			continue
		}
		if m.Role == provider.RoleTool {
			// Every result that answers a call was consumed with it.
			report.OrphanResultIDs = append(report.OrphanResultIDs, m.ToolCallID)
			continue
		}
		if m.Role != provider.RoleAssistant || len(m.ToolCalls) == 0 {
			out = append(out, m)
			continue
		}

		// The results directly after this message are the expected ones; any
		// other position is a move.
		adjacentEnd := i + 1
		for adjacentEnd < len(msgs) && msgs[adjacentEnd].Role == provider.RoleTool {
			adjacentEnd++
		}
		var calls []provider.ToolCall
		var results []provider.Message
		seen := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || seen[tc.ID] {
				report.DroppedCallCount++
				continue
			}
			seen[tc.ID] = true
			calls = append(calls, tc)
			if j, ok := nextResult(tc.ID, i); ok {
				used[j] = true
				results = append(results, msgs[j])
				if j >= adjacentEnd {
					report.MovedResultIDs = append(report.MovedResultIDs, tc.ID)
				}
				continue
			}
			results = append(results, provider.Message{Role: provider.RoleTool, Content: unrecordedToolResultNotice, ToolCallID: tc.ID})
			report.UnansweredCallIDs = append(report.UnansweredCallIDs, tc.ID)
			report.UnansweredTools = append(report.UnansweredTools, tc.Name)
		}
		if len(calls) == 0 {
			// Nothing answerable is left. Keep the text, if any, as a plain
			// turn: providers reject an empty assistant message.
			if m.Content != "" {
				m.ToolCalls = nil
				out = append(out, m)
			}
			continue
		}
		m.ToolCalls = calls
		out = append(out, m)
		out = append(out, results...)
	}
	return out, report
}

// summaryCut returns the index where summarization splits msgs so that the
// last keep messages stay verbatim. The cut moves earlier while it would land
// on a tool result, so a kept result never loses the call before it to the
// summary.
func summaryCut(msgs []provider.Message, keep int) int {
	cut := len(msgs) - keep
	for cut > 0 && cut < len(msgs) && isToolResultMessage(msgs[cut]) {
		cut--
	}
	return cut
}

// reportToolPairing records a repair by pairToolMessages: a warning and a
// tool_messages_repaired event, both carrying the call ids so the damaged
// session history can be found.
func (o *Orchestrator) reportToolPairing(ctx context.Context, sessionID string, r toolPairingReport) {
	logger.FromContext(ctx).Warn("repaired unpaired tool messages in LLM request",
		"session_id", sessionID,
		"unanswered_call_ids", r.UnansweredCallIDs,
		"unanswered_tools", r.UnansweredTools,
		"moved_result_ids", r.MovedResultIDs,
		"orphan_result_ids", r.OrphanResultIDs,
		"dropped_calls", r.DroppedCallCount)
	emit.EmitToolMessagesRepaired(ctx, o.eventSink, emit.ToolMessagesRepairedArgs{
		UnansweredCallIDs: r.UnansweredCallIDs,
		UnansweredTools:   r.UnansweredTools,
		MovedResultIDs:    r.MovedResultIDs,
		OrphanResultIDs:   r.OrphanResultIDs,
		DroppedCallCount:  r.DroppedCallCount,
	})
}
