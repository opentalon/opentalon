package orchestrator

import (
	"context"
	"strings"

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
//   - a call's result found later in the call's exchange (before the next user
//     turn or tool-calling assistant message) is moved back to the call;
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

	used := make([]bool, len(msgs))
	// nextResult returns the first unused result for id after the call at
	// position i, searching only the call's own exchange: it ends at the next
	// user turn or the next assistant message that calls tools. Call ids are
	// not unique across rounds (a provider that omits them gets "call-1" every
	// round, planner calls are "planner-<plugin>-<action>"), so a result past
	// that point belongs to a later call with the same id, not to this one.
	// Assistant text with no calls (e.g. a notification) does not end it.
	nextResult := func(id string, i int) (int, bool) {
		for j := i + 1; j < len(msgs); j++ {
			if bordersExchange(msgs[j]) {
				break
			}
			if msgs[j].Role == provider.RoleTool && msgs[j].ToolCallID == id && !used[j] {
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
// last keep messages stay verbatim. The cut moves earlier while a kept result
// would lose its call to the summary: when it lands on a tool result, or
// inside an exchange whose call is before the cut and whose result is after it
// (e.g. a notification between the two halves).
func summaryCut(msgs []provider.Message, keep int) int {
	cut := len(msgs) - keep
	for cut > 0 && cut < len(msgs) {
		if isToolResultMessage(msgs[cut]) {
			cut--
			continue
		}
		k, ok := openExchangeCall(msgs, cut)
		if !ok {
			break
		}
		cut = k
	}
	return cut
}

// openExchangeCall reports whether a cut at position cut splits a native tool
// exchange: the closest tool-calling assistant message before cut, with no
// user turn in between, has a result at or after cut (before the exchange
// ends at the next user turn or tool-calling assistant message). It returns
// that assistant message's index.
func openExchangeCall(msgs []provider.Message, cut int) (int, bool) {
	k := cut - 1
	for k >= 0 && !bordersExchange(msgs[k]) {
		k--
	}
	if k < 0 || msgs[k].Role == provider.RoleUser {
		return 0, false
	}
	ids := make(map[string]bool, len(msgs[k].ToolCalls))
	for _, tc := range msgs[k].ToolCalls {
		ids[tc.ID] = true
	}
	for j := cut; j < len(msgs); j++ {
		if bordersExchange(msgs[j]) {
			break
		}
		if msgs[j].Role == provider.RoleTool && ids[msgs[j].ToolCallID] {
			return k, true
		}
	}
	return 0, false
}

// bordersExchange reports whether m opens or closes a native tool exchange (a
// tool-calling assistant message and the results that answer it): a user turn
// or another tool-calling assistant message. Assistant text without calls,
// such as a notification, sits inside an exchange without ending it.
func bordersExchange(m provider.Message) bool {
	return m.Role == provider.RoleUser || (m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0)
}

// pairingReported remembers what a turn already reported, so damage that stays
// in the history is reported once per turn rather than on every LLM round,
// while newly found damage still is. It counts occurrences per key rather than
// remembering keys: call ids repeat across rounds, so a second unanswered
// "call-1" is new damage even when the first was already reported. Unanswered
// calls are keyed on id and tool name.
type pairingReported struct {
	seen    map[string]int
	dropped int
}

// fresh returns the part of r not reported yet this turn and marks it reported.
func (p *pairingReported) fresh(r toolPairingReport) toolPairingReport {
	if p.seen == nil {
		p.seen = make(map[string]int)
	}
	counts := make(map[string]int)
	isNew := func(parts ...string) bool {
		k := strings.Join(parts, "\x00")
		counts[k]++
		if counts[k] > p.seen[k] {
			p.seen[k] = counts[k]
			return true
		}
		return false
	}
	var out toolPairingReport
	for i, id := range r.UnansweredCallIDs {
		if isNew("unanswered", id, r.UnansweredTools[i]) {
			out.UnansweredCallIDs = append(out.UnansweredCallIDs, id)
			out.UnansweredTools = append(out.UnansweredTools, r.UnansweredTools[i])
		}
	}
	for _, id := range r.MovedResultIDs {
		if isNew("moved", id) {
			out.MovedResultIDs = append(out.MovedResultIDs, id)
		}
	}
	for _, id := range r.OrphanResultIDs {
		if isNew("orphan", id) {
			out.OrphanResultIDs = append(out.OrphanResultIDs, id)
		}
	}
	if r.DroppedCallCount > p.dropped {
		out.DroppedCallCount = r.DroppedCallCount - p.dropped
		p.dropped = r.DroppedCallCount
	}
	return out
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
