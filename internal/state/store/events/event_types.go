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
	TypePlannerInvoked  = "planner_invoked"
	TypePlannerRequest  = "planner_request"
	TypePlannerResponse = "planner_response"
	TypePlannerStep     = "planner_step"

	// Preparer-phase retrieval + decision events (RFC #249).
	// Emitted children of user_message: each RAG retrieval becomes one
	// *_retrieval event, then preparer_decision is the composite outcome
	// the orchestrator landed on. messages_truncated fires from
	// buildMessages inside the agent loop and parents to turn_start
	// (it can fire more than once per turn when the cutter applies on
	// successive iterations as the conversation grows during tool
	// rounds).
	TypeKnowledgeRetrieval = "knowledge_retrieval"
	TypeGlossaryRetrieval  = "glossary_retrieval"
	TypeToolRetrieval      = "tool_retrieval"
	TypePreparerDecision   = "preparer_decision"
	TypeMessagesTruncated  = "messages_truncated"

	TypeSummarizationTriggered = "summarization_triggered"
	TypeSummarizationCompleted = "summarization_completed"
	TypeSessionTitleInvoked    = "session_title_invoked"
	TypeSessionTitleGenerated  = "session_title_generated"
	TypeModelSwitch            = "model_switch"
	TypeConfirmationRequested  = "confirmation_requested"
	TypeConfirmationResolved   = "confirmation_resolved"
	// confirmation_classification_invoked is the parent sentinel for the
	// free-text confirmation classifier's LLM call, mirroring
	// session_title_invoked: the provider-emitted llm_request/llm_response
	// for the classify call nest under it so the call is visible (and its
	// tokens/cost reachable) in the nerd-mode event log.
	TypeConfirmationClassificationInvoked = "confirmation_classification_invoked"
	TypeRetry                             = "retry"

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

// PromptSnapshotKind values for prompt_snapshots.kind. knowledge_article
// is added by RFC #249: per-turn-injected knowledge bodies are stored
// content-addressed alongside the existing system_prompt / tool_description
// entries, so consumers resolve them through the same /prompt-snapshots
// endpoint.
const (
	PromptKindSystemPrompt       = "system_prompt"
	PromptKindServerInstructions = "server_instructions"
	PromptKindToolDescription    = "tool_description"
	// PromptKindKnowledgeArticle is reserved for the content-addressed
	// body of one [knowledge_context] block. The constant + payload
	// kind value ship now so consumers can resolve the SHA carried in
	// TurnStartPayload.InjectedKnowledge[].ContentSHA256 via the
	// /prompt-snapshots endpoint once a producer exists. No orchestrator-
	// side producer ships in RFC #249's initial scope — the bodies
	// still live inline in the user message's [knowledge_context]
	// envelope (parseable via internal/orchestrator's KC helpers), and
	// reviewer-side rendering today resolves bodies that way. A
	// snapshot-writer (orchestrator-side at dedup-injection time OR
	// plugin-side at retrieval time) is tracked as a follow-up so
	// existing call-sites don't pay a write per turn before consumers
	// actually need it.
	PromptKindKnowledgeArticle = "knowledge_article"
)

// PreparerDecisionMode values for PreparerDecisionPayload.Mode.
//
// instrumentation_only — Phase 2: events emit but dedup/tier logic is
//
//	disabled. All candidates pass through to the LLM unchanged; the event
//	payload records what *would* be the decision had the dedup logic been
//	active, with empty skipped_known / score_overrides_applied buckets.
//
// full — Phase 3+: dedup + tier logic active, payload reflects the
//
//	actual orchestrator decision for this turn.
//
// pull_only — knowledge is pull-only: retrieval still ran, so
//
//	CandidateIDs report what was retrieved, but nothing was auto-injected:
//	Injected and InjectedBytes are empty/zero. The model fetches article
//	bodies on demand via ask_knowledge instead. This is the only mode the
//	current orchestrator emits; the other values above document modes that
//	historical event rows may still carry.
const (
	PreparerDecisionModeInstrumentationOnly = "instrumentation_only"
	PreparerDecisionModeFull                = "full"
	PreparerDecisionModePullOnly            = "pull_only"
)

// KnowledgeRetrievalSearchTextSource values for the search_text_source
// dimension on KnowledgeRetrievalPayload / GlossaryRetrievalPayload.
//
// user_input — raw user message bytes sent as the search query.
// enriched — user message + recent conversation context concatenated
//
//	(see orchestrator's enriched-search-query path) so follow-ups like
//	"both" or "yes" resolve to the right RAG hits.
const (
	SearchTextSourceUserInput = "user_input"
	SearchTextSourceEnriched  = "enriched"
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
//
// RFC #249 Pillar C adds four references back into the preparer phase:
// InjectedKnowledge / PreparerDecisionID / ToolTier1Count / ToolTier3Count.
// These let consumers (review UIs, api-plugin aggregations) render the
// turn's preparer outcome in-place without walking sibling events by
// parent_id. All four are omitempty so pre-Phase-3 deployments and
// no-preparer turns emit a payload byte-identical to the original shape.
type TurnStartPayload struct {
	Header
	SystemPromptSHA256 string                 `json:"system_prompt_sha256,omitempty"`
	ServerInstructions []ServerInstructionRef `json:"server_instructions,omitempty"`
	AvailableTools     []ToolRef              `json:"available_tools,omitempty"`
	InjectedKnowledge  []KnowledgeRef         `json:"injected_knowledge,omitempty"`
	PreparerDecisionID string                 `json:"preparer_decision_id,omitempty"`
	ToolTier1Count     int                    `json:"tool_tier1_count,omitempty"`
	ToolTier3Count     int                    `json:"tool_tier3_count,omitempty"`
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

// KnowledgeRef is the turn_start reference back to one knowledge article
// injected into this turn's [knowledge_context] envelope. ContentSHA256
// participates in the same content-addressed prompt_snapshots store as
// SystemPromptSHA256 / DescSHA256 — once a producer writes a
// PromptKindKnowledgeArticle snapshot, consumers can resolve the body
// via /prompt-snapshots?sha=… without re-running RAG retrieval. Today
// the snapshot writer for that kind is deferred (see the comment on
// PromptKindKnowledgeArticle); the reference itself is durable
// regardless, and the bodies remain readable inline in the user
// message's [knowledge_context] block.
type KnowledgeRef struct {
	ArticleID     string `json:"article_id"`
	ContentSHA256 string `json:"content_sha256"`
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
	ModelID         string `json:"model_id"`
	MessageCount    int    `json:"message_count"`
	HasTools        bool   `json:"has_tools"`
	MaxTokens       int    `json:"max_tokens,omitempty"`
	Reasoning       bool   `json:"reasoning,omitempty"`        // reasoning/chain-of-thought enabled for this call
	ReasoningEffort string `json:"reasoning_effort,omitempty"` // effort actually sent on the wire ("low"/"medium"/"high"); empty = none
}

// LLMRequestVersion bumped to 2 when the payload gained reasoning /
// reasoning_effort. Both are omitempty, so v=1 rows decode cleanly (the
// fields are simply absent) — the bump follows the package's "evolve =>
// bump" convention (mirrors ToolRetrievalVersion) rather than signalling a
// breaking change.
const LLMRequestVersion = 2

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
	Query            string             `json:"query"`
	SearchTextSource string             `json:"search_text_source,omitempty"`
	TopK             int                `json:"top_k,omitempty"`
	MinScore         float64            `json:"min_score,omitempty"`
	LatencyMS        int64              `json:"latency_ms,omitempty"`
	Hits             []ToolRetrievalHit `json:"hits"`
}

type ToolRetrievalHit struct {
	ToolName string  `json:"tool_name"`
	Score    float64 `json:"score"`
}

// ToolRetrievalVersion bumped to 2 when the payload gained
// search_text_source / min_score / latency_ms (RFC #249, Phase 2). No
// v=1 rows exist in the wild — the type was declared in 009 but never
// emitted before — so the bump is documentation hygiene rather than a
// compatibility step.
const ToolRetrievalVersion = 2

// KnowledgeRetrievalPayload — Weaviate-backed knowledge-base RAG. Each
// hit carries a content_sha256 so consumers can resolve the body via the
// prompt_snapshots store (kind=knowledge_article). RFC #249.
type KnowledgeRetrievalPayload struct {
	Header
	Query            string                  `json:"query"`
	SearchTextSource string                  `json:"search_text_source,omitempty"`
	TopK             int                     `json:"top_k,omitempty"`
	MinScore         float64                 `json:"min_score,omitempty"`
	LatencyMS        int64                   `json:"latency_ms,omitempty"`
	Hits             []KnowledgeRetrievalHit `json:"hits"`
}

type KnowledgeRetrievalHit struct {
	ArticleID     string  `json:"article_id"`
	Title         string  `json:"title,omitempty"`
	Score         float64 `json:"score"`
	ContentSHA256 string  `json:"content_sha256,omitempty"`
	Source        string  `json:"source,omitempty"` // "knowledge_base" | future per-tenant sources
}

const KnowledgeRetrievalVersion = 1

// GlossaryRetrievalPayload — Weaviate-backed glossary RAG. Shape mirrors
// KnowledgeRetrievalPayload; the only difference is per-hit "term"
// instead of "title". RFC #249.
type GlossaryRetrievalPayload struct {
	Header
	Query            string                 `json:"query"`
	SearchTextSource string                 `json:"search_text_source,omitempty"`
	TopK             int                    `json:"top_k,omitempty"`
	MinScore         float64                `json:"min_score,omitempty"`
	LatencyMS        int64                  `json:"latency_ms,omitempty"`
	Hits             []GlossaryRetrievalHit `json:"hits"`
}

type GlossaryRetrievalHit struct {
	Term          string  `json:"term"`
	Score         float64 `json:"score"`
	ContentSHA256 string  `json:"content_sha256,omitempty"`
	Source        string  `json:"source,omitempty"`
}

const GlossaryRetrievalVersion = 1

// PreparerDecisionPayload — composite outcome of the preparer phase. One
// event per user turn, parented to the user_message. RFC #249.
//
// Mode is one of the PreparerDecisionMode* constants and discriminates
// the meaning of the sub-blocks. In "instrumentation_only" mode all
// candidates appear under Knowledge.Injected and Tools.Tier1New; the
// skipped/evicted buckets are empty. In "full" mode the blocks reflect
// the real dedup+tier decision.
type PreparerDecisionPayload struct {
	Header
	Mode      string                         `json:"mode"`
	Knowledge PreparerDecisionKnowledgeBlock `json:"knowledge"`
	Tools     PreparerDecisionToolsBlock     `json:"tools"`
}

type PreparerDecisionKnowledgeBlock struct {
	CandidateIDs          []string                        `json:"candidate_ids,omitempty"`
	Injected              []PreparerDecisionInjectedItem  `json:"injected,omitempty"`
	SkippedKnown          []PreparerDecisionSkippedItem   `json:"skipped_known,omitempty"`
	SkippedBelowThreshold []string                        `json:"skipped_below_threshold,omitempty"`
	ScoreOverridesApplied []PreparerDecisionScoreOverride `json:"score_overrides_applied,omitempty"`
	InjectedBytes         int                             `json:"injected_bytes,omitempty"`
}

// PreparerDecisionInjectedItem records one injected knowledge article and
// the reason it was selected. Reason is a free-form wire-format string
// (e.g. "new", "score_override") describing why the candidate ended up in
// the LLM-visible KC block.
type PreparerDecisionInjectedItem struct {
	ArticleID     string `json:"article_id"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// PreparerDecisionSkippedItem records one candidate skipped by dedup.
// Reason is a free-form wire-format string (e.g. "content_sha_already_known",
// "cap_exceeded") describing why the candidate didn't make it.
type PreparerDecisionSkippedItem struct {
	ArticleID string `json:"article_id"`
	Reason    string `json:"reason"`
}

type PreparerDecisionScoreOverride struct {
	ArticleID    string  `json:"article_id"`
	CurrentScore float64 `json:"current_score"`
	Threshold    float64 `json:"threshold"`
}

// PreparerDecisionToolsBlock records what tool RAG retrieved this turn.
// Tier1New carries every retrieved tool candidate (the instrumentation
// signal); tool discovery is now the registry catalog + load_tools sticky
// model, so retrieval no longer drives the LLM's tools array and the old
// per-tier bookkeeping fields are gone.
type PreparerDecisionToolsBlock struct {
	Tier1New []string `json:"tier1_new,omitempty"`
}

const PreparerDecisionVersion = 1

// MessagesTruncatedPayload — emitted when the sliding-window cutter
// drops messages from the assembled LLM input. DroppedSeqRange is
// [from, to] inclusive, indexed into sess.Messages (position-based,
// since the in-memory provider.Message slice does not carry a seq
// column).
type MessagesTruncatedPayload struct {
	Header
	DroppedSeqRange []int `json:"dropped_seq_range,omitempty"` // [from, to] inclusive
	DroppedCount    int   `json:"dropped_count"`
}

const MessagesTruncatedVersion = 1

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

// SessionTitleInvokedPayload — sentinel that anchors the parent scope for
// the inner LLM call's llm_request / llm_response events. Mirrors the
// planner_invoked / summarization_triggered role: emitted at the very
// start of the maybeGenerateTitle goroutine, its event_id is then wrapped
// onto the goroutine's ctx via emit.WithParent so the provider's auto-
// emit lands under the title span instead of with parent_id = NULL.
// MessageCount is the assistant-turn count at trigger time — meaningful
// only as "1" today (titles fire once after the first turn), but kept
// for parity with summarization's analogous field and for a future
// re-titling pass that might fire later in a session.
type SessionTitleInvokedPayload struct {
	Header
	MessageCount int `json:"message_count,omitempty"`
}

const SessionTitleInvokedVersion = 1

// SessionTitleGeneratedPayload — one event per session, emitted after the
// background title-generation LLM call completes. Parented to the matching
// session_title_invoked sentinel (the maybeGenerateTitle entry point) so
// the nested llm_request / llm_response events the provider emits land
// under the same parent and the call's token + cost data is reachable via
// session_events without duplicating it here. Title is the final stored
// string (already trimmed / length-capped by the orchestrator).
type SessionTitleGeneratedPayload struct {
	Header
	Title     string `json:"title"`
	ModelID   string `json:"model_id"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

const SessionTitleGeneratedVersion = 1

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
	// ResolvedBy records how the choice was decided: "frontend_button"
	// (deterministic structured signal), "classifier" (LLM free-text
	// classification), or "fallback" (classifier unavailable -> reject).
	ResolvedBy string `json:"resolved_by,omitempty"`
	// Reason is the classifier's one-sentence explanation, surfaced to the
	// user on reject and useful for diagnosing amend/reject distribution.
	Reason string `json:"reason,omitempty"`
}

// ConfirmationResolvedVersion is 2: v2 adds resolved_by + reason (both
// omitempty, so v1 consumers reading older rows are unaffected).
const ConfirmationResolvedVersion = 2

// ConfirmationClassificationInvokedPayload is the sentinel emitted just before
// the free-text confirmation classifier's LLM call so the nested
// llm_request/llm_response land under it (see TypeConfirmationClassificationInvoked).
type ConfirmationClassificationInvokedPayload struct {
	Header
}

const ConfirmationClassificationInvokedVersion = 1

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
//
// Content/StructuredContent split mirrors the ToolResult shape: a tool
// may return a human-readable response (Content) and a structured JSON
// payload that gets appended to the LLM-bound message via
// nativeToolContent. Both halves are captured here as independent
// excerpts so the audit log records the full picture, not just the
// human-readable half. Each field gets its own truncation flag because
// a 500-byte response with a 50 KB JSON tail is a realistic shape.
//
// StructuredExcerpt is forensic-only — when truncated it is cut at a
// byte boundary and is therefore NOT guaranteed to be valid JSON.
// Consumers must check structured_truncated before attempting to
// parse, and treat the field as an opaque prefix when the flag is set.
type ToolCallResultPayload struct {
	Header
	CallID              string `json:"call_id"`
	Status              string `json:"status"`
	ResponseExcerpt     string `json:"response_excerpt"`
	ResponseTruncated   bool   `json:"response_truncated,omitempty"`
	StructuredExcerpt   string `json:"structured_excerpt,omitempty"`
	StructuredTruncated bool   `json:"structured_truncated,omitempty"`
	LatencyMS           int64  `json:"latency_ms,omitempty"`
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
