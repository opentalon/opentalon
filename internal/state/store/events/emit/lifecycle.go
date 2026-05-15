package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// SummarizationTriggeredArgs records why a summarization was kicked off
// (context-window pressure, explicit user "clear", …) plus the message
// count at trigger time.
type SummarizationTriggeredArgs struct {
	MessageCount int
	Reason       string
}

// EmitSummarizationTriggered writes one summarization_triggered event.
func EmitSummarizationTriggered(ctx context.Context, sink Sink, args SummarizationTriggeredArgs) string {
	return send(ctx, sink, events.TypeSummarizationTriggered, events.SummarizationTriggeredPayload{
		Header:       events.Header{V: events.SummarizationTriggeredVersion},
		MessageCount: args.MessageCount,
		Reason:       args.Reason,
	}, 0)
}

// SummarizationCompletedArgs is the post-summarization snapshot. Summary
// is sanitized + excerpted; KeptMessages is the count of original
// messages that survived past the summary boundary.
type SummarizationCompletedArgs struct {
	Summary      string
	KeptMessages int
	LatencyMS    int64
}

// EmitSummarizationCompleted writes one summarization_completed event.
func EmitSummarizationCompleted(ctx context.Context, sink Sink, args SummarizationCompletedArgs) string {
	sanitized := events.SanitizeUTF8(args.Summary)
	excerpt, truncated := events.Excerpt(sanitized)
	return send(ctx, sink, events.TypeSummarizationCompleted, events.SummarizationCompletedPayload{
		Header:           events.Header{V: events.SummarizationCompletedVersion},
		SummaryExcerpt:   excerpt,
		SummaryTruncated: truncated,
		KeptMessages:     args.KeptMessages,
		LatencyMS:        args.LatencyMS,
	}, args.LatencyMS)
}

// ModelSwitchArgs captures a runtime model swap inside a turn (e.g.
// fallback after refusal, escalation for hard requests).
type ModelSwitchArgs struct {
	From   string
	To     string
	Reason string
}

// EmitModelSwitch writes one model_switch event.
func EmitModelSwitch(ctx context.Context, sink Sink, args ModelSwitchArgs) string {
	return send(ctx, sink, events.TypeModelSwitch, events.ModelSwitchPayload{
		Header: events.Header{V: events.ModelSwitchVersion},
		From:   args.From,
		To:     args.To,
		Reason: args.Reason,
	}, 0)
}

// ConfirmationRequestedArgs records that the orchestrator is asking the
// frontend / user for an explicit yes-or-no on a privileged action.
// ToolCallID links to the proposed tool_call_extracted event.
type ConfirmationRequestedArgs struct {
	Prompt     string
	Choices    []string
	ToolCallID string
}

// EmitConfirmationRequested writes one confirmation_requested event.
// Returns the event id so the caller can persist it alongside the
// pending state and use it as parent_id on the matching resolved event
// once the user replies in a later turn.
func EmitConfirmationRequested(ctx context.Context, sink Sink, args ConfirmationRequestedArgs) string {
	return send(ctx, sink, events.TypeConfirmationRequested, events.ConfirmationRequestedPayload{
		Header:     events.Header{V: events.ConfirmationRequestedVersion},
		Prompt:     events.SanitizeUTF8(args.Prompt),
		Choices:    args.Choices,
		ToolCallID: args.ToolCallID,
	}, 0)
}

// ConfirmationResolvedArgs records the user/frontend response.
type ConfirmationResolvedArgs struct {
	Choice     string
	ToolCallID string
}

// EmitConfirmationResolved writes one confirmation_resolved event.
func EmitConfirmationResolved(ctx context.Context, sink Sink, args ConfirmationResolvedArgs) string {
	return send(ctx, sink, events.TypeConfirmationResolved, events.ConfirmationResolvedPayload{
		Header:     events.Header{V: events.ConfirmationResolvedVersion},
		Choice:     args.Choice,
		ToolCallID: args.ToolCallID,
	}, 0)
}

// RetryArgs describes one retry attempt inside a phase (LLM call,
// planner call, tool dispatch, …). LastError is sanitized free text.
type RetryArgs struct {
	Phase     string
	Attempt   int
	LastError string
}

// EmitRetry writes one retry event.
func EmitRetry(ctx context.Context, sink Sink, args RetryArgs) string {
	return send(ctx, sink, events.TypeRetry, events.RetryPayload{
		Header:    events.Header{V: events.RetryVersion},
		Phase:     args.Phase,
		Attempt:   args.Attempt,
		LastError: events.SanitizeUTF8(args.LastError),
	}, 0)
}
