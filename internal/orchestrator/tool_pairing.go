package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/provider"
)

// pairToolMessages enforces the native tool-calling invariant on an assembled
// request: every tool result directly follows the assistant message that
// carries its tool call, and every tool call is answered by a result in the
// messages right after it. Providers reject a request that breaks it (Anthropic:
// "unexpected tool_use_id found in tool_result blocks" / "tool_use ids were
// found without tool_result blocks immediately after"), which fails the turn
// and every later turn that rebuilds the same history.
//
// History can break the invariant in ways the assembly cannot rule out: a
// failed write of one half of a pair (session writes are best-effort), the
// store's max-messages trim, or a summarization cut. This is the last line of
// defence, run on the finished request: an unanswered tool call is removed from
// its assistant message (the message is dropped if nothing is left), and a
// result without its call is dropped. Text-format [plugin_output] turns are
// plain messages to the provider and are left alone.
func pairToolMessages(ctx context.Context, msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	droppedCalls, droppedResults := 0, 0
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role == provider.RoleTool {
			// Results are consumed together with the assistant message before
			// them; one reaching here has no call.
			droppedResults++
			continue
		}
		if m.Role != provider.RoleAssistant || len(m.ToolCalls) == 0 {
			out = append(out, m)
			continue
		}

		// The results that answer this message are the tool messages right after it.
		end := i + 1
		for end < len(msgs) && msgs[end].Role == provider.RoleTool {
			end++
		}
		answered := make(map[string]bool, end-i-1)
		for _, r := range msgs[i+1 : end] {
			answered[r.ToolCallID] = true
		}
		calls := make([]provider.ToolCall, 0, len(m.ToolCalls))
		issued := make(map[string]bool, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			if answered[tc.ID] {
				calls = append(calls, tc)
				issued[tc.ID] = true
			} else {
				droppedCalls++
			}
		}
		if len(calls) == 0 {
			calls = nil
		}
		// An assistant message left with neither a call nor text is dropped:
		// providers reject empty assistant turns.
		if calls != nil || m.Content != "" {
			m.ToolCalls = calls
			out = append(out, m)
		}
		for _, r := range msgs[i+1 : end] {
			if issued[r.ToolCallID] {
				out = append(out, r)
				delete(issued, r.ToolCallID) // a duplicate result is an orphan too
			} else {
				droppedResults++
			}
		}
		i = end - 1
	}
	if droppedCalls > 0 || droppedResults > 0 {
		logger.FromContext(ctx).Warn("dropped unpaired tool messages from LLM request",
			"tool_calls", droppedCalls, "tool_results", droppedResults)
	}
	return out
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
