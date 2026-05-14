package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// EmitPlannerInvoked writes one planner_invoked event with a short
// human-readable reason ("user_request", "retry", "summarization_gap", …).
func EmitPlannerInvoked(ctx context.Context, sink Sink, reason string) {
	send(ctx, sink, events.TypePlannerInvoked, events.PlannerInvokedPayload{
		Header: events.Header{V: events.PlannerInvokedVersion},
		Reason: reason,
	}, 0)
}

// PlannerRequestArgs is metadata about the planner's LLM call. Mirrors
// LLMRequestArgs for symmetry — analytics consumers expect identical
// shape across the two request types.
type PlannerRequestArgs struct {
	ModelID      string
	MessageCount int
}

// EmitPlannerRequest writes one planner_request event.
func EmitPlannerRequest(ctx context.Context, sink Sink, args PlannerRequestArgs) {
	send(ctx, sink, events.TypePlannerRequest, events.PlannerRequestPayload{
		Header:       events.Header{V: events.PlannerRequestVersion},
		ModelID:      args.ModelID,
		MessageCount: args.MessageCount,
	}, 0)
}

// PlannerResponseArgs carries the planner's raw output for triage. The
// helper sanitizes + excerpts RawContent in lockstep with LLMResponse so
// downstream tooling can treat both uniformly.
type PlannerResponseArgs struct {
	RawContent string
	LatencyMS  int64
}

// EmitPlannerResponse writes one planner_response event.
func EmitPlannerResponse(ctx context.Context, sink Sink, args PlannerResponseArgs) {
	sanitized := events.SanitizeUTF8(args.RawContent)
	excerpt, truncated := events.Excerpt(sanitized)
	send(ctx, sink, events.TypePlannerResponse, events.PlannerResponsePayload{
		Header:              events.Header{V: events.PlannerResponseVersion},
		RawContentExcerpt:   excerpt,
		RawContentTruncated: truncated,
		LatencyMS:           args.LatencyMS,
	}, args.LatencyMS)
}

// PlannerStepArgs describes one decision step inside the planner. Kind
// is an open-vocab discriminator ("decide_tool", "decide_clarify", …);
// callers should keep the value set small and stable so it's a useful
// analytics dimension.
type PlannerStepArgs struct {
	StepIndex int
	StepKind  string
	Note      string
}

// EmitPlannerStep writes one planner_step event.
func EmitPlannerStep(ctx context.Context, sink Sink, args PlannerStepArgs) {
	send(ctx, sink, events.TypePlannerStep, events.PlannerStepPayload{
		Header:    events.Header{V: events.PlannerStepVersion},
		StepIndex: args.StepIndex,
		StepKind:  args.StepKind,
		Note:      args.Note,
	}, 0)
}

// ToolRetrievalArgs carries one Weaviate (or other RAG backend) tool-
// retrieval result set. Hits are stored in returned-rank order so a
// consumer can compute position-based metrics without re-sorting.
type ToolRetrievalArgs struct {
	Query string
	TopK  int
	Hits  []events.ToolRetrievalHit
}

// EmitToolRetrieval writes one tool_retrieval event.
func EmitToolRetrieval(ctx context.Context, sink Sink, args ToolRetrievalArgs) {
	send(ctx, sink, events.TypeToolRetrieval, events.ToolRetrievalPayload{
		Header: events.Header{V: events.ToolRetrievalVersion},
		Query:  events.SanitizeUTF8(args.Query),
		TopK:   args.TopK,
		Hits:   args.Hits,
	}, 0)
}
