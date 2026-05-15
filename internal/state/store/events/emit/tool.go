package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// ToolCallMode discriminates how the orchestrator obtained a tool call.
type ToolCallMode string

const (
	ToolCallModeNative ToolCallMode = "native"
	ToolCallModeText   ToolCallMode = "text"
)

// ToolCallExtractedArgs is the orchestrator's decoded view of one tool
// call — emitted after parsing but BEFORE plugin dispatch. CallID links
// the corresponding tool_call_result via the session_events.parent_id
// column (caller stamps WithParent for the dispatch span).
type ToolCallExtractedArgs struct {
	CallID    string
	Plugin    string
	Action    string
	Arguments map[string]string
	Mode      ToolCallMode
}

// EmitToolCallExtracted writes one tool_call_extracted event. Returns the
// generated event id so the dispatcher can stamp it as the parent of the
// matching tool_call_result via WithParent.
func EmitToolCallExtracted(ctx context.Context, sink Sink, args ToolCallExtractedArgs) string {
	return send(ctx, sink, events.TypeToolCallExtracted, events.ToolCallExtractedPayload{
		Header:    events.Header{V: events.ToolCallExtractedVersion},
		CallID:    args.CallID,
		Plugin:    args.Plugin,
		Action:    args.Action,
		Arguments: args.Arguments,
		Mode:      string(args.Mode),
	}, 0)
}

// ToolCallResultArgs is the dispatch outcome. Status follows plugin
// convention ("ok" or "error"). Response is sanitized + excerpted; the
// parent linkage to tool_call_extracted is carried on ctx via
// emit.WithParent at the dispatch site.
type ToolCallResultArgs struct {
	CallID    string
	Status    string
	Response  string
	LatencyMS int64
}

// EmitToolCallResult writes one tool_call_result event.
func EmitToolCallResult(ctx context.Context, sink Sink, args ToolCallResultArgs) string {
	sanitized := events.SanitizeUTF8(args.Response)
	excerpt, truncated := events.Excerpt(sanitized)
	return send(ctx, sink, events.TypeToolCallResult, events.ToolCallResultPayload{
		Header:            events.Header{V: events.ToolCallResultVersion},
		CallID:            args.CallID,
		Status:            args.Status,
		ResponseExcerpt:   excerpt,
		ResponseTruncated: truncated,
		LatencyMS:         args.LatencyMS,
	}, args.LatencyMS)
}

// ToolCallParseFailedArgs captures text-based tool-call syntax that the
// parser could not interpret. RawSnippet is the exact substring the
// parser saw — sanitized only, not excerpted, because parse failures
// are short by construction (the parser bails on the first bad token).
//
// If a snippet ever exceeds ExcerptCap we still excerpt it rather than
// truncating arbitrarily; the truncated flag would surface that.
type ToolCallParseFailedArgs struct {
	RawSnippet string
	ParserUsed string
	ParseError string
}

// EmitToolCallParseFailed writes one tool_call_parse_failed event.
func EmitToolCallParseFailed(ctx context.Context, sink Sink, args ToolCallParseFailedArgs) string {
	sanitized := events.SanitizeUTF8(args.RawSnippet)
	excerpt, _ := events.Excerpt(sanitized)
	return send(ctx, sink, events.TypeToolCallParseFailed, events.ToolCallParseFailedPayload{
		Header:     events.Header{V: events.ToolCallParseFailedVersion},
		RawSnippet: excerpt,
		ParserUsed: args.ParserUsed,
		ParseError: args.ParseError,
	}, 0)
}

// ToolCallArgsInvalidArgs is emitted when a tool call passed parsing but
// failed plugin-side argument validation. CallID matches the extracted
// event so a consumer can link them.
type ToolCallArgsInvalidArgs struct {
	CallID          string
	Plugin          string
	Action          string
	ValidationError string
}

// EmitToolCallArgsInvalid writes one tool_call_args_invalid event.
func EmitToolCallArgsInvalid(ctx context.Context, sink Sink, args ToolCallArgsInvalidArgs) string {
	return send(ctx, sink, events.TypeToolCallArgsInvalid, events.ToolCallArgsInvalidPayload{
		Header:          events.Header{V: events.ToolCallArgsInvalidVersion},
		CallID:          args.CallID,
		Plugin:          args.Plugin,
		Action:          args.Action,
		ValidationError: args.ValidationError,
	}, 0)
}

// EmitToolCallNotFound writes one tool_call_not_found event when the
// LLM names a plugin/action the dispatcher does not know about.
func EmitToolCallNotFound(ctx context.Context, sink Sink, requestedName string) string {
	return send(ctx, sink, events.TypeToolCallNotFound, events.ToolCallNotFoundPayload{
		Header:        events.Header{V: events.ToolCallNotFoundVersion},
		RequestedName: requestedName,
	}, 0)
}
