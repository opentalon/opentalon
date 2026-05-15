// Package events is the source of truth for session_events payloads.
//
// Every row in the session_events table carries an event_type and a JSON
// payload. This package declares the canonical list of event_type values
// (Type* constants) and the Go struct for each payload variant. Producers
// (orchestrator, planner, tool dispatch, retention worker stub) build a
// struct from here and let the store marshal it; consumers (api-plugin,
// score worker, Rails UI via the api-plugin) decode the same struct.
//
// Schema versioning. Each payload struct embeds a `V int` field tagged as
// "v" in JSON, default 1. When a payload evolves, bump the constant for
// that event type and teach the consumer to handle the older version
// alongside the new one — never break old rows in place (the table is
// append-only).
//
// Raw-capture rule. Payload fields that carry LLM- or tool-emitted bytes
// (raw_content, raw_snippet, response_excerpt, response_body_excerpt,
// refusal_text, arguments, ...) are populated with the EXACT bytes
// observed at the point of first capture, before any parsing or
// normalization. The store enforces UTF-8 validity and a 4 KB excerpt cap
// (with a truncated flag), and nothing else — see Excerpt and
// SanitizeUTF8.
package events

import (
	"encoding/json"
	"unicode/utf8"
)

// Event-type constants. Add new ones here, never invent strings inline.
const (
	// Conversation surface (UI-visible by default).
	TypeTurnStart   = "turn_start"
	TypeUserMessage = "user_message"
	TypeLLMRequest  = "llm_request"
	TypeLLMResponse = "llm_response"

	// Planner / orchestrator internals (debug-view or fold-default in UI).
	TypePlannerInvoked         = "planner_invoked"
	TypePlannerRequest         = "planner_request"
	TypePlannerResponse        = "planner_response"
	TypePlannerStep            = "planner_step"
	TypeToolRetrieval          = "tool_retrieval"
	TypeSummarizationTriggered = "summarization_triggered"
	TypeSummarizationCompleted = "summarization_completed"
	TypeModelSwitch            = "model_switch"
	TypeConfirmationRequested  = "confirmation_requested"
	TypeConfirmationResolved   = "confirmation_resolved"
	TypeRetry                  = "retry"

	// Tool calls.
	TypeToolCallExtracted = "tool_call_extracted"
	TypeToolCallResult    = "tool_call_result"

	// Failure modes (always UI-visible — each its own type for clean analytics).
	TypeToolCallParseFailed = "tool_call_parse_failed"
	TypeToolCallArgsInvalid = "tool_call_args_invalid"
	TypeToolCallNotFound    = "tool_call_not_found"
	TypeLLMRefused          = "llm_refused"
	TypeLLMError            = "llm_error"
	TypeError               = "error"

	// Closing.
	TypeScoreComputed = "score_computed"
)

// PromptSnapshotKind values for prompt_snapshots.kind.
const (
	PromptKindSystemPrompt       = "system_prompt"
	PromptKindServerInstructions = "server_instructions"
	PromptKindToolDescription    = "tool_description"
)

// ExcerptCap is the maximum byte length stored for raw LLM/tool-emitted
// excerpts. Capture is best-effort and informational — bodies exceeding the
// cap are truncated with a flag, and the full body remains available via
// ai_debug_events (when /debug was active for that session).
const ExcerptCap = 4 * 1024

// Excerpt clips s at ExcerptCap bytes and reports whether truncation
// happened. Use this for any free-form payload field whose source is the
// LLM or a tool response.
//
// Trim happens at a rune boundary so the stored excerpt stays valid UTF-8.
// If the input is itself invalid UTF-8 (no rune boundary in the trailing
// few bytes), we fall back to a raw byte cut at ExcerptCap rather than
// returning an empty string — pair with SanitizeUTF8 before storing if
// the source is untrusted.
func Excerpt(s string) (string, bool) {
	if len(s) <= ExcerptCap {
		return s, false
	}
	cut := ExcerptCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 && !utf8.RuneStart(s[0]) {
		// Invalid UTF-8: prefer a too-long-by-a-few-bytes raw cut over an
		// empty excerpt that loses all forensic value. SanitizeUTF8 will
		// scrub continuation-byte fragments at the boundary on its own pass.
		cut = ExcerptCap
	}
	return s[:cut], true
}

// SanitizeUTF8 replaces invalid UTF-8 byte sequences with U+FFFD so the
// result satisfies Postgres' UTF-8 column requirement. Valid input is
// returned unchanged.
func SanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string([]rune(s)) // []rune conversion replaces invalid bytes with U+FFFD
}

// ----- Payload structs -----

// Header is embedded in every payload. Producers set V to the current
// schema version constant for that event type.
type Header struct {
	V int `json:"v"`
}

// TurnStartPayload — emitted once per orchestrator turn. References to
// prompt content are by SHA256; bodies are persisted out-of-band in
// prompt_snapshots so the same prompt costs one row across all sessions.
type TurnStartPayload struct {
	Header
	SystemPromptSHA256 string                 `json:"system_prompt_sha256,omitempty"`
	ServerInstructions []ServerInstructionRef `json:"server_instructions,omitempty"`
	AvailableTools     []ToolRef              `json:"available_tools,omitempty"`
	ModelID            string                 `json:"model_id"`
	Temperature        *float64               `json:"temperature,omitempty"`
	ReasoningEffort    string                 `json:"reasoning_effort,omitempty"`
}

type ServerInstructionRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type ToolRef struct {
	Name       string `json:"name"`
	DescSHA256 string `json:"desc_sha256"`
}

const TurnStartVersion = 1

// UserMessagePayload — exact user input as the orchestrator received it.
type UserMessagePayload struct {
	Header
	Content       string `json:"content"`
	ContentLength int    `json:"content_length"`
}

const UserMessageVersion = 1

// LLMRequestPayload — metadata about the request, not the full body (the
// full request body lives in ai_debug_events when /debug is active).
type LLMRequestPayload struct {
	Header
	ModelID      string `json:"model_id"`
	MessageCount int    `json:"message_count"`
	HasTools     bool   `json:"has_tools"`
	MaxTokens    int    `json:"max_tokens,omitempty"`
}

const LLMRequestVersion = 1

// LLMResponsePayload — captured at provider edge BEFORE the orchestrator
// parses native tool calls or interprets text-based tool-call syntax.
// RawContentExcerpt is the exact bytes (subject to ExcerptCap), and
// NativeToolCallsRaw is the provider's ToolCalls structure embedded as
// raw JSON so consumers see exactly what the provider sent — even if the
// shape drifts from the current ToolCall struct.
//
// NativeToolCallsRaw uses json.RawMessage (not string) so it inlines
// directly into the parent payload: `{"native_tool_calls_raw":[{...}]}`
// instead of an escaped-string form. This matters for psql inspection and
// for the api-plugin which would otherwise need a double unmarshal.
//
// CostInput / CostOutput — cost of this call, computed by the provider
// wrapper at call time from token counts and the per-million rates
// configured on the matching ModelInfo. Frozen at call time so later
// config changes (or model retirement) do not retroactively re-price
// historical events. Fields are unitless floats; the currency is
// whatever ModelInfo.Cost is denominated in — operators document the
// convention at deployment level (matching the existing store.UsageRecord
// {InputCost, OutputCost} convention in internal/state/store/usage.go).
// Both fields use omitempty: a zero value means "model not in the
// catalogue at emit time", not "free" — analytics should treat absent
// fields as unpriced rather than summing them as zero-cost calls.
type LLMResponsePayload struct {
	Header
	RawContentExcerpt   string          `json:"raw_content_excerpt"`
	RawContentTruncated bool            `json:"raw_content_truncated,omitempty"`
	RawContentSHA256    string          `json:"raw_content_sha256,omitempty"`
	NativeToolCallsRaw  json.RawMessage `json:"native_tool_calls_raw,omitempty"`
	FinishReason        string          `json:"finish_reason,omitempty"`
	TokensIn            int             `json:"tokens_in,omitempty"`
	TokensOut           int             `json:"tokens_out,omitempty"`
	CostInput           float64         `json:"cost_input,omitempty"`
	CostOutput          float64         `json:"cost_output,omitempty"`
	LatencyMS           int64           `json:"latency_ms,omitempty"`
	ProviderResponseID  string          `json:"provider_response_id,omitempty"`
}

const LLMResponseVersion = 1

// PlannerInvokedPayload / PlannerRequestPayload / PlannerResponsePayload /
// PlannerStepPayload — populated by the future planner instrumentation.
// Structs are reserved here so consumers and the api-plugin can ship a
// stable shape today.
type PlannerInvokedPayload struct {
	Header
	Reason string `json:"reason,omitempty"`
}

const PlannerInvokedVersion = 1

type PlannerRequestPayload struct {
	Header
	ModelID      string `json:"model_id"`
	MessageCount int    `json:"message_count"`
}

const PlannerRequestVersion = 1

type PlannerResponsePayload struct {
	Header
	RawContentExcerpt   string `json:"raw_content_excerpt"`
	RawContentTruncated bool   `json:"raw_content_truncated,omitempty"`
	LatencyMS           int64  `json:"latency_ms,omitempty"`
}

const PlannerResponseVersion = 1

type PlannerStepPayload struct {
	Header
	StepIndex int    `json:"step_index"`
	StepKind  string `json:"step_kind"`
	Note      string `json:"note,omitempty"`
}

const PlannerStepVersion = 1

// ToolRetrievalPayload — Weaviate-backed tool RAG. Hits include score so
// regression analysis can spot retrieval quality drift over time.
type ToolRetrievalPayload struct {
	Header
	Query string             `json:"query"`
	TopK  int                `json:"top_k"`
	Hits  []ToolRetrievalHit `json:"hits"`
}

type ToolRetrievalHit struct {
	ToolName string  `json:"tool_name"`
	Score    float64 `json:"score"`
}

const ToolRetrievalVersion = 1

type SummarizationTriggeredPayload struct {
	Header
	MessageCount int    `json:"message_count"`
	Reason       string `json:"reason,omitempty"`
}

const SummarizationTriggeredVersion = 1

type SummarizationCompletedPayload struct {
	Header
	SummaryExcerpt   string `json:"summary_excerpt"`
	SummaryTruncated bool   `json:"summary_truncated,omitempty"`
	KeptMessages     int    `json:"kept_messages"`
	LatencyMS        int64  `json:"latency_ms,omitempty"`
}

const SummarizationCompletedVersion = 1

type ModelSwitchPayload struct {
	Header
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

const ModelSwitchVersion = 1

type ConfirmationRequestedPayload struct {
	Header
	Prompt     string   `json:"prompt"`
	Choices    []string `json:"choices,omitempty"`
	ToolCallID string   `json:"tool_call_id,omitempty"`
}

const ConfirmationRequestedVersion = 1

type ConfirmationResolvedPayload struct {
	Header
	Choice     string `json:"choice"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

const ConfirmationResolvedVersion = 1

type RetryPayload struct {
	Header
	Phase     string `json:"phase"`
	Attempt   int    `json:"attempt"`
	LastError string `json:"last_error,omitempty"`
}

const RetryVersion = 1

// ToolCallExtractedPayload — the orchestrator's decoded view of one tool
// call. Mode discriminates how the call was obtained: "native" for
// provider-emitted function calls, "text" for free-text parser hits.
type ToolCallExtractedPayload struct {
	Header
	CallID    string            `json:"call_id"`
	Plugin    string            `json:"plugin"`
	Action    string            `json:"action"`
	Arguments map[string]string `json:"arguments,omitempty"`
	Mode      string            `json:"mode"` // "native" | "text"
}

const ToolCallExtractedVersion = 1

// ToolCallResultPayload — emitted after the tool dispatch returns or
// errors out. Status mirrors plugin convention ("ok" / "error"). Parent
// event_id of the corresponding tool_call_extracted is linked via the
// session_events.parent_id column, not duplicated in payload.
type ToolCallResultPayload struct {
	Header
	CallID            string `json:"call_id"`
	Status            string `json:"status"`
	ResponseExcerpt   string `json:"response_excerpt"`
	ResponseTruncated bool   `json:"response_truncated,omitempty"`
	LatencyMS         int64  `json:"latency_ms,omitempty"`
}

const ToolCallResultVersion = 1

// ToolCallParseFailedPayload — text-based tool-call syntax that the
// orchestrator's parser could not interpret. Carries the exact substring
// the parser saw so post-hoc analysis can diff against working examples.
type ToolCallParseFailedPayload struct {
	Header
	RawSnippet string `json:"raw_snippet"`
	ParserUsed string `json:"parser_used"`
	ParseError string `json:"parse_error"`
}

const ToolCallParseFailedVersion = 1

type ToolCallArgsInvalidPayload struct {
	Header
	CallID          string `json:"call_id"`
	Plugin          string `json:"plugin"`
	Action          string `json:"action"`
	ValidationError string `json:"validation_error"`
}

const ToolCallArgsInvalidVersion = 1

type ToolCallNotFoundPayload struct {
	Header
	RequestedName string `json:"requested_name"`
}

const ToolCallNotFoundVersion = 1

// LLMRefusedPayload — content-safety refusal or policy block from the
// provider. RefusalText is the model's stated refusal (no excerpt cap —
// these are short by construction).
type LLMRefusedPayload struct {
	Header
	RefusalText      string `json:"refusal_text"`
	ContentSafetyHit string `json:"content_safety_hit,omitempty"`
}

const LLMRefusedVersion = 1

type LLMErrorPayload struct {
	Header
	Phase                 string `json:"phase"`
	StatusCode            int    `json:"status_code,omitempty"`
	ResponseBodyExcerpt   string `json:"response_body_excerpt,omitempty"`
	ResponseBodyTruncated bool   `json:"response_body_truncated,omitempty"`
}

const LLMErrorVersion = 1

// ErrorPayload — generic catch-all. Prefer a typed variant above when one
// exists; use Error only when none fits.
type ErrorPayload struct {
	Header
	Where   string `json:"where"`
	Message string `json:"message"`
}

const ErrorVersion = 1

// ScoreComputedPayload — written by the score worker (separate ticket).
// Reasoning is informational free text; the numeric score plus
// rubric_version are the analytics fields.
type ScoreComputedPayload struct {
	Header
	Score         float64 `json:"score"`
	RubricVersion string  `json:"rubric_version"`
	Reasoning     string  `json:"reasoning,omitempty"`
}

const ScoreComputedVersion = 1
