package emit

import (
	"context"
	"encoding/json"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// LLMRequestArgs is metadata about an outbound LLM call — never the full
// body. The body, when /debug is active, lives in ai_debug_events.
type LLMRequestArgs struct {
	ModelID      string
	MessageCount int
	HasTools     bool
	MaxTokens    int
}

// EmitLLMRequest writes one llm_request event. Emit immediately before
// the provider HTTP call so the row predates llm_response chronologically.
func EmitLLMRequest(ctx context.Context, sink Sink, args LLMRequestArgs) string {
	return send(ctx, sink, events.TypeLLMRequest, events.LLMRequestPayload{
		Header:       events.Header{V: events.LLMRequestVersion},
		ModelID:      args.ModelID,
		MessageCount: args.MessageCount,
		HasTools:     args.HasTools,
		MaxTokens:    args.MaxTokens,
	}, 0)
}

// LLMResponseArgs captures everything observable at the provider edge
// BEFORE the orchestrator parses native tool calls or text-based tool
// syntax.
//
// RawContent is the exact provider-emitted body; the helper sanitizes
// UTF-8, computes the full-content SHA256, and stores the 4 KB excerpt
// with a truncated flag. NativeToolCallsRaw is the provider's ToolCalls
// JSON inlined as-is so consumers don't double-unmarshal.
//
// CostInput / CostOutput are the cost of this call, computed by the
// caller (the provider wrapper) from token counts and the per-million
// rates on the matching ModelInfo. Computing here rather than at read
// time freezes pricing at call time so later config changes do not
// retroactively re-price historical events. Currency is whatever
// ModelInfo.Cost is denominated in — see LLMResponsePayload doc. Zero
// values are passed through unchanged; the payload uses omitempty so
// unpriced models simply leave the fields out.
type LLMResponseArgs struct {
	RawContent         string
	NativeToolCallsRaw json.RawMessage
	FinishReason       string
	TokensIn           int
	TokensOut          int
	CostInput          float64
	CostOutput         float64
	LatencyMS          int64
	ProviderResponseID string
}

// EmitLLMResponse writes one llm_response event. The helper computes
// raw_content_sha256 over the SANITIZED full content (not the excerpt)
// so the digest matches what a re-played raw HTTP body would hash to.
// Returns the event id so the provider can surface it on CompletionResponse,
// letting the orchestrator parent subsequent tool_call_extracted events.
func EmitLLMResponse(ctx context.Context, sink Sink, args LLMResponseArgs) string {
	sanitized := events.SanitizeUTF8(args.RawContent)
	excerpt, truncated := events.Excerpt(sanitized)
	return send(ctx, sink, events.TypeLLMResponse, events.LLMResponsePayload{
		Header:              events.Header{V: events.LLMResponseVersion},
		RawContentExcerpt:   excerpt,
		RawContentTruncated: truncated,
		RawContentSHA256:    sha256Hex(sanitized),
		NativeToolCallsRaw:  args.NativeToolCallsRaw,
		FinishReason:        args.FinishReason,
		TokensIn:            args.TokensIn,
		TokensOut:           args.TokensOut,
		CostInput:           args.CostInput,
		CostOutput:          args.CostOutput,
		LatencyMS:           args.LatencyMS,
		ProviderResponseID:  args.ProviderResponseID,
	}, args.LatencyMS)
}

// LLMErrorArgs describes a provider-side failure (4xx/5xx, transport
// error, malformed response). Phase identifies where it happened
// ("chat", "embeddings", "moderation", …).
type LLMErrorArgs struct {
	Phase            string
	StatusCode       int
	ResponseBodyText string
}

// EmitLLMError writes one llm_error event with an excerpted, UTF-8-clean
// response body.
func EmitLLMError(ctx context.Context, sink Sink, args LLMErrorArgs) string {
	sanitized := events.SanitizeUTF8(args.ResponseBodyText)
	excerpt, truncated := events.Excerpt(sanitized)
	return send(ctx, sink, events.TypeLLMError, events.LLMErrorPayload{
		Header:                events.Header{V: events.LLMErrorVersion},
		Phase:                 args.Phase,
		StatusCode:            args.StatusCode,
		ResponseBodyExcerpt:   excerpt,
		ResponseBodyTruncated: truncated,
	}, 0)
}

// LLMRefusedArgs carries the refusal text plus, optionally, the content
// safety category the provider returned. RefusalText is not excerpt-
// capped — refusals are short by construction.
type LLMRefusedArgs struct {
	RefusalText      string
	ContentSafetyHit string
}

// EmitLLMRefused writes one llm_refused event.
func EmitLLMRefused(ctx context.Context, sink Sink, args LLMRefusedArgs) string {
	return send(ctx, sink, events.TypeLLMRefused, events.LLMRefusedPayload{
		Header:           events.Header{V: events.LLMRefusedVersion},
		RefusalText:      events.SanitizeUTF8(args.RefusalText),
		ContentSafetyHit: args.ContentSafetyHit,
	}, 0)
}
