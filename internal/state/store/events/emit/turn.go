package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TurnStartArgs carries the per-turn prompt/model context. Prompt bodies
// are referenced by SHA256 — callers are responsible for upserting the
// underlying content into prompt_snapshots before (or alongside) the
// turn_start emission so a consumer can resolve the digest.
type TurnStartArgs struct {
	SystemPromptSHA256 string
	ServerInstructions []events.ServerInstructionRef
	AvailableTools     []events.ToolRef
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
