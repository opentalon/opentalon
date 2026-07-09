package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TurnStartArgs carries the per-turn prompt/model context. Prompt bodies
// are referenced by SHA256 — callers are responsible for upserting the
// underlying content into prompt_snapshots before (or alongside) the
// turn_start emission so a consumer can resolve the digest.
//
// RFC #249 Pillar C: InjectedKnowledge / PreparerDecisionID /
// ToolTier1Count / ToolTier3Count cross-reference the preparer phase
// emitted just before turn_start. Callers populate them from the
// preparer aggregate stashed on ctx; pre-preparer-phase code paths
// leave them zero/nil and the JSON payload omits them.
type TurnStartArgs struct {
	SystemPromptSHA256 string
	ServerInstructions []events.ServerInstructionRef
	AvailableTools     []events.ToolRef
	InjectedKnowledge  []events.KnowledgeRef
	PreparerDecisionID string
	ToolTier1Count     int
	ToolTier3Count     int
	ModelID            string
	Temperature        *float64
	ReasoningEffort    string
}

// EmitTurnStart writes a turn_start event for the current session.
// Returns the generated event id so callers can chain it as the parent
// of the rest of the turn via WithParent.
func EmitTurnStart(ctx context.Context, sink Sink, args TurnStartArgs) string {
	return send(ctx, sink, events.TypeTurnStart, events.TurnStartPayload{
		Header:             events.Header{V: events.TurnStartVersion},
		SystemPromptSHA256: args.SystemPromptSHA256,
		ServerInstructions: args.ServerInstructions,
		AvailableTools:     args.AvailableTools,
		InjectedKnowledge:  args.InjectedKnowledge,
		PreparerDecisionID: args.PreparerDecisionID,
		ToolTier1Count:     args.ToolTier1Count,
		ToolTier3Count:     args.ToolTier3Count,
		ModelID:            args.ModelID,
		Temperature:        args.Temperature,
		ReasoningEffort:    args.ReasoningEffort,
	}, 0)
}

// EmitUserMessage writes a user_message event carrying the raw user
// content. The helper sanitizes UTF-8 first; ContentLength is the byte
// length of what's actually stored (post-sanitization) so the payload
// stays internally consistent — analytics counting bytes never see a
// length that disagrees with the content field.
func EmitUserMessage(ctx context.Context, sink Sink, content string) string {
	sanitized := events.SanitizeUTF8(content)
	return send(ctx, sink, events.TypeUserMessage, events.UserMessagePayload{
		Header:        events.Header{V: events.UserMessageVersion},
		Content:       sanitized,
		ContentLength: len(sanitized),
	}, 0)
}

// TurnFinishedArgs is the terminal snapshot of one orchestrator turn. See
// events.TurnFinishedPayload for the field semantics; the orchestrator
// derives these from the RunResult on every Run return path.
type TurnFinishedArgs struct {
	Outcome         string
	MessageProduced bool
	ResponseLength  int
	ToolCallCount   int
	LatencyMS       int64
}

// EmitTurnFinished writes the turn_finished event that closes the turn
// opened by EmitUserMessage. The orchestrator registers it as a deferred
// emit right after user_message so it fires on every return path — normal
// answer, pending confirmation, pipeline cancel, and error alike. The row
// duration column mirrors LatencyMS so latency queries index it directly.
func EmitTurnFinished(ctx context.Context, sink Sink, args TurnFinishedArgs) string {
	return send(ctx, sink, events.TypeTurnFinished, events.TurnFinishedPayload{
		Header:          events.Header{V: events.TurnFinishedVersion},
		Outcome:         args.Outcome,
		MessageProduced: args.MessageProduced,
		ResponseLength:  args.ResponseLength,
		ToolCallCount:   args.ToolCallCount,
		LatencyMS:       args.LatencyMS,
	}, args.LatencyMS)
}
