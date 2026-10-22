package provider

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Tool names are passed to Anthropic verbatim. The orchestrator's tool
// registry composes fully-qualified names with the "__" separator (see
// pkg/toolfqn), e.g. "crm__crm__list-contacts" or "_meta__load_tools".
// No wire encoding is needed because the composed FQN charset
// `^[a-zA-Z0-9_-]{1,64}$` — Anthropic's tool-name pattern — is now
// ENFORCED at registration (see ToolRegistry.Register), so any name that
// would fail at the API is rejected up front with a clear error instead.

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicMessagesPath   = "/v1/messages"
	anthropicAPIVersion     = "2023-06-01"

	anthropicCacheDiagnosisBeta = "cache-diagnosis-2026-04-07"
	// anthropicThinkingBindingBeta allows thinking.block_binding.
	anthropicThinkingBindingBeta = "thinking-binding-controls-2026-08-01"
	// maxDiagnosedSessions bounds the sessions whose last response id is kept
	// for cache diagnostics.
	maxDiagnosedSessions = 10_000
)

// Phase labels for emit.LLMErrorArgs. Mirrors the OpenAI provider's
// constants so analytics dashboards that group by Phase can pivot
// across both providers without per-provider casing. WIRE-STABLE:
// values are persisted in session_events.payload and downstream
// tooling groups by them — add new phases, never rewrite existing.
const (
	phaseAnthChatTransport    = "chat.transport"
	phaseAnthChatReadResponse = "chat.read_response"
	phaseAnthChatHTTPStatus   = "chat.http_status"
	phaseAnthChatUnmarshal    = "chat.unmarshal"
	phaseAnthChatAPIError     = "chat.api_error"
)

// AnthropicProvider implements the Provider interface for the
// Anthropic Messages API. Emits llm_request / llm_response / llm_error
// session events at every HTTP call so the Nerd-Mode event log and
// AI-Analytics token/cost totals stay populated for Anthropic-routed
// sessions. Mirrors the OpenAI provider's emit boundary; streaming
// hasn't landed for Anthropic yet so there's no stream-side emit
// today.
type AnthropicProvider struct {
	id        string
	baseURL   string
	apiKey    string
	models    []ModelInfo
	client    *http.Client
	eventSink emit.Sink         // structured session-event sink; nil disables emission
	retry     RetryPolicy       // transient-failure retry policy (DefaultRetryPolicy unless configured)
	headers   map[string]string // extra request headers from config; never logged or captured
	secrets   []string          // header values to redact from responses and errors

	promptCache bool // cache markers on requests that carry segments; see cachedSystemBlocks
	// lastIDs is set when cache diagnostics is on: the previous response id
	// of each session, sent on its next chat-loop request.
	lastIDs *lastResponseIDs
}

// AnthropicOption configures an AnthropicProvider.
type AnthropicOption func(*AnthropicProvider)

// WithAnthropicHTTPClient sets a custom HTTP client.
func WithAnthropicHTTPClient(c *http.Client) AnthropicOption {
	return func(p *AnthropicProvider) { p.client = c }
}

// WithAnthropicSessionEventSink wires a structured session-event sink
// so every LLM HTTP exchange emits llm_request / llm_response /
// llm_error. Always-on by design — there is no per-session resolver —
// so a non-nil sink enables capture for every Anthropic call. Match
// the OpenAI provider's behaviour so analytics surfaces (AI-Sessions,
// AI-Analytics totals strip) work uniformly regardless of which
// provider routed a given turn.
func WithAnthropicSessionEventSink(s emit.Sink) AnthropicOption {
	return func(p *AnthropicProvider) { p.eventSink = s }
}

// WithAnthropicHeaders adds extra HTTP headers to every request this provider
// sends. They are applied after the default headers, so a configured
// x-api-key replaces the default one. Entries with an empty value are
// dropped. The map is copied.
func WithAnthropicHeaders(h map[string]string) AnthropicOption {
	return func(p *AnthropicProvider) { p.headers = copyExtraHeaders(h) }
}

// WithAnthropicRetryPolicy sets the transient-failure retry policy. Zero-valued
// fields fall back to DefaultRetryPolicy, so a partial config is safe.
func WithAnthropicRetryPolicy(rp RetryPolicy) AnthropicOption {
	return func(p *AnthropicProvider) { p.retry = rp.withDefaults() }
}

// WithAnthropicPromptCache turns Anthropic prompt caching on or off.
func WithAnthropicPromptCache(on bool) AnthropicOption {
	return func(p *AnthropicProvider) { p.promptCache = on }
}

// WithAnthropicCacheDiagnostics turns Anthropic cache diagnostics on or off
// for the requests that carry segments.
func WithAnthropicCacheDiagnostics(on bool) AnthropicOption {
	return func(p *AnthropicProvider) {
		p.lastIDs = nil
		if on {
			p.lastIDs = newLastResponseIDs(maxDiagnosedSessions)
		}
	}
}

// NewAnthropicProvider creates a provider for the Anthropic API.
func NewAnthropicProvider(id, baseURL, apiKey string, models []ModelInfo, opts ...AnthropicOption) *AnthropicProvider {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	p := &AnthropicProvider{
		id:      id,
		baseURL: baseURL,
		apiKey:  apiKey,
		models:  models,
		client:  &http.Client{Timeout: 120 * time.Second},
		retry:   DefaultRetryPolicy(),
	}
	for _, o := range opts {
		o(p)
	}
	// Retry lives in the transport (see withRetry) — provider-agnostic and
	// transparent to Complete.
	p.secrets = redactionSecrets(p.headers)
	p.client = withRetry(guardClient(p.client, p.headers), p.retry, p.eventSink)
	return p
}

// usage returns a response's tokens and cost. Anthropic's input_tokens
// leaves out the tokens written to and read from the prompt cache, so
// InputTokens adds them. Per million tokens, uncached ones cost Cost.Input,
// written ones Cost.CacheWrite and read ones Cost.CacheRead. A modelID not
// in p.models costs 0, which LLMResponsePayload leaves out (omitempty)
// rather than recording a misleading "free call". Currency is unitless:
// the operator's deployment convention sets the unit.
func (p *AnthropicProvider) usage(modelID string, u anthUsage) Usage {
	usage := Usage{
		InputTokens:      u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
	}
	for _, m := range p.models {
		if m.ID != modelID {
			continue
		}
		usage.InputCost = (float64(u.InputTokens)*m.Cost.Input +
			float64(u.CacheCreationInputTokens)*m.Cost.CacheWrite +
			float64(u.CacheReadInputTokens)*m.Cost.CacheRead) / 1_000_000
		usage.OutputCost = float64(u.OutputTokens) * m.Cost.Output / 1_000_000
		break
	}
	return usage
}

func (p *AnthropicProvider) ID() string { return p.id }

func (p *AnthropicProvider) Models() []ModelInfo { return p.models }

func (p *AnthropicProvider) SupportsFeature(f Feature) bool {
	for _, m := range p.models {
		if m.SupportsFeature(f) {
			return true
		}
	}
	return false
}

// -- Anthropic wire types --

type anthRequest struct {
	Model     string          `json:"model"`
	System    json.RawMessage `json:"system,omitempty"` // string, or []anthTextBlock with prompt caching
	Messages  []anthMessage   `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
	Thinking  *anthThinking   `json:"thinking,omitempty"`
	// OutputConfig carries the effort of a model with adaptive thinking.
	OutputConfig *anthOutputConfig `json:"output_config,omitempty"`
	Tools        []anthTool        `json:"tools,omitempty"`
	// CacheControl is the automatic cache marker: Anthropic caches the
	// request up to its last block, so the conversation so far.
	CacheControl *anthCacheControl `json:"cache_control,omitempty"`
	Diagnostics  *anthDiagnostics  `json:"diagnostics,omitempty"`
}

// anthDiagnostics opts a request in to cache diagnostics. PreviousMessageID
// is the session's previous response id, null on its first request.
type anthDiagnostics struct {
	PreviousMessageID *string `json:"previous_message_id"`
}

type anthTextBlock struct {
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	CacheControl *anthCacheControl `json:"cache_control,omitempty"`
}

// anthCacheControl marks a cache breakpoint. Type "ephemeral" with no ttl is
// Anthropic's default 5-minute lifetime.
type anthCacheControl struct {
	Type string `json:"type"`
}

type anthTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type anthThinking struct {
	Type         string `json:"type"`                    // "enabled" or "adaptive"
	BudgetTokens int    `json:"budget_tokens,omitempty"` // max tokens for thinking; none with "adaptive"
	// BlockBinding is sent with "adaptive" only, the one form that takes it.
	BlockBinding *anthBlockBinding `json:"block_binding,omitempty"`
}

// anthBlockBinding says what Anthropic does with a replayed thinking block
// whose earlier conversation changed: "drop_block" leaves the block out
// instead of rejecting the request.
type anthBlockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type anthOutputConfig struct {
	Effort string `json:"effort"` // "low", "medium", "high", "xhigh" or "max"
}

// effort returns the effort sent, or "" when c is nil.
func (c *anthOutputConfig) effort() string {
	if c == nil {
		return ""
	}
	return c.Effort
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string for text-only, []anthContentBlock for multipart
}

type anthResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Model      string             `json:"model"`
	Content    []anthContentBlock `json:"content"`
	StopReason string             `json:"stop_reason,omitempty"`
	Usage      anthUsage          `json:"usage"`
	Error      *anthError         `json:"error,omitempty"`
	// Diagnostics is null when there was nothing to compare or nothing
	// diverged; its cache_miss_reason is null while the comparison runs.
	Diagnostics *struct {
		CacheMissReason *struct {
			Type string `json:"type"`
		} `json:"cache_miss_reason"`
	} `json:"diagnostics"`
	// InputTransformations lists what the API changed in the request, such
	// as a thinking block it dropped ("thinking_dropped").
	InputTransformations []struct {
		Type string `json:"type"`
	} `json:"input_transformations"`
}

// thinkingDropped returns how many replayed thinking blocks the API dropped.
func (r *anthResponse) thinkingDropped() int {
	n := 0
	for _, t := range r.InputTransformations {
		if t.Type == "thinking_dropped" {
			n++
		}
	}
	return n
}

type anthContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Thinking blocks carry their text in a dedicated `thinking` field, not
	// `text` — extractContent reads this for the "thinking" case.
	Thinking string           `json:"thinking,omitempty"`
	Source   *anthImageSource `json:"source,omitempty"`
	// tool_use (assistant emits to invoke a tool)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result (user echoes back the tool result)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	// raw is a response block's JSON as received; a thinking block must go
	// back exactly as it came.
	raw json.RawMessage
}

// UnmarshalJSON decodes a block and keeps its JSON as received in b.raw.
func (b *anthContentBlock) UnmarshalJSON(data []byte) error {
	type plain anthContentBlock
	if err := json.Unmarshal(data, (*plain)(b)); err != nil {
		return err
	}
	b.raw = append(json.RawMessage(nil), data...)
	return nil
}

type anthImageSource struct {
	Type      string `json:"type"`                 // "base64" or "text"
	MediaType string `json:"media_type,omitempty"` // e.g. "image/png"; omitted for text sources
	Data      string `json:"data"`                 // base64-encoded bytes or plain text
}

type anthUsage struct {
	InputTokens              int `json:"input_tokens"` // uncached input tokens only
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Complete sends a non-streaming completion request.
func (p *AnthropicProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	anthReq, err := p.toAnthRequest(req)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	session := actor.SessionID(ctx)
	diagnose := p.lastIDs != nil && req.Segments != nil
	if diagnose {
		anthReq.Diagnostics = &anthDiagnostics{PreviousMessageID: p.lastIDs.get(session)}
	}

	// Log reasoning configuration.
	if anthReq.Thinking != nil {
		slog.DebugContext(ctx, "anthropic reasoning enabled",
			"model", anthReq.Model,
			"thinking_type", anthReq.Thinking.Type,
			"budget_tokens", anthReq.Thinking.BudgetTokens,
			"effort", anthReq.OutputConfig.effort(),
			"max_tokens", anthReq.MaxTokens,
		)
	}

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+anthropicMessagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq)
	if diagnose {
		addListHeader(httpReq.Header, "anthropic-beta", anthropicCacheDiagnosisBeta)
	}
	if anthReq.Thinking != nil && anthReq.Thinking.BlockBinding != nil {
		addListHeader(httpReq.Header, "anthropic-beta", anthropicThinkingBindingBeta)
	}

	// Structured session-event capture: emit metadata about the outbound
	// LLM call immediately before the HTTP send so the row predates
	// llm_response chronologically.
	emit.EmitLLMRequest(ctx, p.eventSink, emit.LLMRequestArgs{
		ModelID:      anthReq.Model,
		MessageCount: len(anthReq.Messages),
		HasTools:     len(anthReq.Tools) > 0,
		MaxTokens:    anthReq.MaxTokens,
		// Thinking != nil means reasoning is on for this turn. Only adaptive
		// thinking sends an effort; a budget is a token count.
		Reasoning:       anthReq.Thinking != nil,
		ReasoningEffort: anthReq.OutputConfig.effort(),
	})

	start := time.Now()
	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		err = cleanClientError(err, p.secrets)
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseAnthChatTransport,
			ResponseBodyText: err.Error(),
		})
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseAnthChatReadResponse,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: err.Error(),
		})
		return nil, fmt.Errorf("read response: %w", err)
	}

	// Capture latency here — network/server side of the call is over.
	// Subsequent unmarshal cost is CPU on this host and would skew the
	// "LLM latency" measurement consumed by analytics dashboards.
	latencyMS := time.Since(start).Milliseconds()

	if httpResp.StatusCode != http.StatusOK {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseAnthChatHTTPStatus,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: string(respBody),
		})
		return nil, fmt.Errorf("anthropic api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var anthResp anthResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseAnthChatUnmarshal,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: string(respBody),
		})
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if anthResp.Error != nil {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseAnthChatAPIError,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: anthResp.Error.Message,
		})
		return nil, fmt.Errorf("anthropic error [%s]: %s", anthResp.Error.Type, anthResp.Error.Message)
	}

	var cacheMissReason string
	if diagnose {
		p.lastIDs.put(session, anthResp.ID)
		if d := anthResp.Diagnostics; d != nil && d.CacheMissReason != nil {
			cacheMissReason = d.CacheMissReason.Type
		}
	}

	content, thinking, thinkingBlocks, toolCalls := p.extractContent(anthResp.Content)

	// Log thinking blocks when present.
	if thinking != "" {
		slog.DebugContext(ctx, "anthropic reasoning response",
			"model", anthResp.Model,
			"thinking_len", len(thinking),
			"thinking", thinking,
			"content_len", len(content),
		)
	}

	// Re-marshal the tool_use content blocks so analytics persists the
	// provider's wire shape verbatim, mirroring openai.go's behaviour for
	// native tool_calls. Filter to tool_use type only — the rest of the
	// content array is the assistant's text reply which travels in
	// RawContent.
	var nativeToolCallsRaw json.RawMessage
	if len(toolCalls) > 0 {
		toolBlocks := make([]anthContentBlock, 0, len(toolCalls))
		for _, b := range anthResp.Content {
			if b.Type == "tool_use" {
				toolBlocks = append(toolBlocks, b)
			}
		}
		nativeToolCallsRaw, _ = json.Marshal(toolBlocks)
	}

	usage := p.usage(anthReq.Model, anthResp.Usage)
	eventID := emit.EmitLLMResponse(ctx, p.eventSink, emit.LLMResponseArgs{
		RawContent:         content,
		NativeToolCallsRaw: nativeToolCallsRaw,
		FinishReason:       anthResp.StopReason,
		TokensIn:           usage.InputTokens,
		TokensOut:          usage.OutputTokens,
		CacheWriteTokens:   usage.CacheWriteTokens,
		CacheReadTokens:    usage.CacheReadTokens,
		CostInput:          usage.InputCost,
		CostOutput:         usage.OutputCost,
		LatencyMS:          latencyMS,
		ProviderResponseID: anthResp.ID,
		ChatLoop:           req.Segments != nil,
		CacheMissReason:    cacheMissReason,
		ThinkingDropped:    anthResp.thinkingDropped(),
	})

	return &CompletionResponse{
		ID:        anthResp.ID,
		Model:     anthResp.Model,
		Content:   content,
		ToolCalls: toolCalls,
		Thinking:  thinkingBlocks,
		Usage:     usage,
		EventID:   eventID,
	}, nil
}

// Stream is not yet implemented; returns an error.
func (p *AnthropicProvider) Stream(_ context.Context, _ *CompletionRequest) (ResponseStream, error) {
	return nil, fmt.Errorf("streaming not yet implemented for provider %s", p.id)
}

func (p *AnthropicProvider) toAnthRequest(req *CompletionRequest) (anthRequest, error) {
	model := findModel(p.models, req.Model)
	// Thinking blocks go back only on a request that turns thinking on: a
	// model thinking by default gets no block_binding without it.
	replayThinking := model.Thinking == "adaptive" || req.Reasoning

	// Anthropic carries the system prompt in a single top-level `system`
	// field, not as a message. Concatenate every RoleSystem message (joined
	// by a blank line) rather than last-wins overwriting: the normal case is
	// one leading system message, but a stray mid-array system message (e.g.
	// a transient nudge) must not silently clobber the real prompt.
	var systemParts []string
	msgs := make([]anthMessage, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			systemParts = append(systemParts, m.Content)
			continue
		}
		if !replayThinking {
			m.Thinking = nil
		}
		msg, err := p.toAnthMessage(m)
		if err != nil {
			return anthRequest{}, err
		}
		msgs = append(msgs, msg)
	}
	system, cached, err := p.toAnthSystem(req, systemParts)
	if err != nil {
		return anthRequest{}, err
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	ar := anthRequest{
		Model:     req.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: maxTokens,
		Tools:     toolDefsToAnthTools(req.Tools),
	}
	if cached {
		ar.CacheControl = &anthCacheControl{Type: "ephemeral"}
	}

	if model.Thinking == "adaptive" {
		// A replayed thinking block whose earlier conversation OpenTalon
		// changed is dropped by the API instead of failing the request.
		ar.Thinking = &anthThinking{Type: "adaptive", BlockBinding: &anthBlockBinding{PrefixMismatchBehavior: "drop_block"}}
		effort := req.ReasoningEffort
		if effort == "" {
			effort = model.ReasoningEffort
		}
		if effort != "" {
			ar.OutputConfig = &anthOutputConfig{Effort: effort}
		}
	} else if req.Reasoning {
		budget := req.BudgetTokens
		if budget == 0 {
			budget = 10000 // default thinking budget
		}
		ar.Thinking = &anthThinking{Type: "enabled", BudgetTokens: budget}
		// Anthropic requires max_tokens to be at least budget_tokens + expected output.
		if ar.MaxTokens < budget+1024 {
			ar.MaxTokens = budget + 4096
		}
	}

	return ar, nil
}

// toAnthSystem renders the system field from the request's system messages,
// systemParts in order: as cache-marked text blocks when cachedSystemBlocks
// returns any, which cached reports, else as their join in one string.
func (p *AnthropicProvider) toAnthSystem(req *CompletionRequest, systemParts []string) (system json.RawMessage, cached bool, err error) {
	if blocks := p.cachedSystemBlocks(req, systemParts); len(blocks) > 0 {
		system, err = json.Marshal(blocks)
		if err != nil {
			return nil, false, fmt.Errorf("marshal system blocks: %w", err)
		}
		return system, true, nil
	}
	text := joinStrings(systemParts)
	if text == "" {
		return nil, false, nil
	}
	system, err = json.Marshal(text)
	if err != nil {
		return nil, false, fmt.Errorf("marshal system prompt: %w", err)
	}
	return system, false, nil
}

// cachedSystemBlocks returns the system field's text blocks when prompt
// caching is on and req.Segments describes the leading system message: one
// block per segment, then one per further system message, leaving out blank
// ones (Anthropic rejects them), with a cache marker on the first block.
// Otherwise it returns nil.
func (p *AnthropicProvider) cachedSystemBlocks(req *CompletionRequest, systemParts []string) []anthTextBlock {
	segs := req.Segments
	if !p.promptCache || segs == nil || len(req.Messages) == 0 ||
		req.Messages[0].Role != RoleSystem || req.Messages[0].Content != segs.Join() {
		return nil
	}
	var blocks []anthTextBlock
	// systemParts[0] is the leading system message, sent as its segments.
	for _, text := range append([]string{segs.Fixed, segs.Conversation, segs.Round}, systemParts[1:]...) {
		if strings.TrimSpace(text) != "" {
			blocks = append(blocks, anthTextBlock{Type: "text", Text: text})
		}
	}
	if len(blocks) > 0 {
		blocks[0].CacheControl = &anthCacheControl{Type: "ephemeral"}
	}
	return blocks
}

// toolDefsToAnthTools maps the orchestrator's typed tool definitions onto
// Anthropic's request-side tool schema. Parameters is JSON-Schema-shaped
// in both providers, so it passes through unchanged into Anthropic's
// input_schema field. Returns nil (not an empty slice) when the request
// carries no tools so json:"omitempty" on the wrapper drops the field
// entirely — Anthropic returns 400 if `tools` is present but empty.
func toolDefsToAnthTools(defs []ToolDefinition) []anthTool {
	if len(defs) == 0 {
		return nil
	}
	tools := make([]anthTool, len(defs))
	for i, d := range defs {
		tools[i] = anthTool{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: d.Parameters,
		}
	}
	return tools
}

// toAnthMessage converts a provider.Message to an anthMessage. Routes by
// role and metadata onto Anthropic's content-block shapes:
//
//   - RoleTool (tool result): rewritten to role:user with a tool_result
//     content block — Anthropic carries tool results in user-role
//     messages, not in a separate "tool" role like OpenAI.
//   - RoleAssistant with ToolCalls: emitted as content blocks =
//     [stored thinking blocks] + [optional text] + [one tool_use block per
//     ToolCall], so a replay of multi-turn history echoes back the
//     assistant's prior tool invocation in the exact shape Anthropic
//     produced it.
//   - Plain text (no files, no tool_calls): content is a JSON string,
//     the most compact representation.
//   - Messages with file attachments: content becomes a JSON array of
//     blocks (files first, then optional text). Image mime types map
//     to "image" blocks, application/pdf maps to "document" base64
//     source, text-like types map to "document" text source.
func (p *AnthropicProvider) toAnthMessage(m Message) (anthMessage, error) {
	if m.Role == RoleTool {
		return toolResultMessage(m)
	}
	if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
		return assistantWithToolUseMessage(m)
	}
	if len(m.Files) == 0 {
		raw, err := json.Marshal(m.Content)
		if err != nil {
			return anthMessage{}, fmt.Errorf("marshal message content: %w", err)
		}
		return anthMessage{Role: string(m.Role), Content: raw}, nil
	}

	var blocks []anthContentBlock
	for _, f := range m.Files {
		switch ClassifyFile(f.MimeType, f.Data) {
		case FileClassImage:
			blocks = append(blocks, anthContentBlock{
				Type: "image",
				Source: &anthImageSource{
					Type:      "base64",
					MediaType: f.MimeType,
					Data:      base64.StdEncoding.EncodeToString(f.Data),
				},
			})
		case FileClassPDF:
			blocks = append(blocks, anthContentBlock{
				Type: "document",
				Source: &anthImageSource{
					Type:      "base64",
					MediaType: f.MimeType,
					Data:      base64.StdEncoding.EncodeToString(f.Data),
				},
			})
		case FileClassText:
			// Anthropic's text source requires media_type "text/plain" regardless of
			// the original MIME type — it is the only value the API accepts for this source kind.
			blocks = append(blocks, anthContentBlock{
				Type: "document",
				Source: &anthImageSource{
					Type:      "text",
					MediaType: "text/plain",
					Data:      string(f.Data),
				},
			})
		default:
			return anthMessage{}, fmt.Errorf("unsupported file mime type %q", f.MimeType)
		}
	}
	if m.Content != "" {
		blocks = append(blocks, anthContentBlock{Type: "text", Text: m.Content})
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return anthMessage{}, fmt.Errorf("marshal multipart message: %w", err)
	}
	return anthMessage{Role: string(m.Role), Content: raw}, nil
}

// toolResultMessage wraps a tool's output into Anthropic's tool_result
// content block, carried in a role:user message. m.Content is the raw
// tool output text; it goes through as a JSON string in the block's
// "content" field. m.ToolCallID is the corresponding tool_use id from
// the prior assistant message — Anthropic rejects orphan tool_results
// with no matching pending tool_use.
func toolResultMessage(m Message) (anthMessage, error) {
	contentRaw, err := json.Marshal(m.Content)
	if err != nil {
		return anthMessage{}, fmt.Errorf("marshal tool result content: %w", err)
	}
	blocks := []anthContentBlock{{Type: "tool_result", ToolUseID: m.ToolCallID, Content: contentRaw}}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return anthMessage{}, fmt.Errorf("marshal tool result message: %w", err)
	}
	return anthMessage{Role: string(RoleUser), Content: raw}, nil
}

// assistantWithToolUseMessage rebuilds an assistant turn whose response
// contained tool_use blocks. Replay history needs the exact wire shape
// Anthropic emitted: the response's thinking blocks unchanged + optional
// preamble text + N tool_use blocks (id, name, input). Arguments come back
// from the orchestrator's typed ToolCall.Arguments map[string]string;
// re-shape to JSON object so Anthropic accepts the input field.
func assistantWithToolUseMessage(m Message) (anthMessage, error) {
	blocks := make([]any, 0, len(m.Thinking)+1+len(m.ToolCalls))
	for _, t := range m.Thinking {
		blocks = append(blocks, t)
	}
	if m.Content != "" {
		blocks = append(blocks, anthContentBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		// ToolCall.Arguments is map[string]string at the orchestrator boundary
		// (string-coerced to dodge JSON ambiguity around `null`, numeric vs
		// string ids, etc.). Anthropic takes an object in `input`, so a call
		// without arguments goes out as {}, never null.
		args := tc.Arguments
		if args == nil {
			args = map[string]string{}
		}
		argsRaw, err := json.Marshal(args)
		if err != nil {
			return anthMessage{}, fmt.Errorf("marshal tool call arguments: %w", err)
		}
		blocks = append(blocks, anthContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: argsRaw,
		})
	}
	raw, err := json.Marshal(blocks)
	if err != nil {
		return anthMessage{}, fmt.Errorf("marshal assistant tool_use message: %w", err)
	}
	return anthMessage{Role: string(RoleAssistant), Content: raw}, nil
}

// extractContent walks the response's content array, joining text and
// thinking blocks into their respective scalars and parsing tool_use
// blocks into the orchestrator's ToolCall shape. The arguments JSON
// object is flattened to map[string]string (nil and "" entries dropped)
// to match how openai.go shapes ToolCall.Arguments — keeps downstream
// dispatcher code provider-agnostic. thinkingBlocks holds the thinking and
// redacted_thinking blocks as received, for the replay.
func (p *AnthropicProvider) extractContent(blocks []anthContentBlock) (content, thinking string, thinkingBlocks []json.RawMessage, toolCalls []ToolCall) {
	var textParts, thinkParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking":
			thinkParts = append(thinkParts, b.Thinking)
			thinkingBlocks = append(thinkingBlocks, b.raw)
		case "redacted_thinking":
			thinkingBlocks = append(thinkingBlocks, b.raw)
		case "tool_use":
			toolCalls = append(toolCalls, anthToolUseToToolCall(b))
		}
	}
	return joinStrings(textParts), joinStrings(thinkParts), thinkingBlocks, toolCalls
}

func anthToolUseToToolCall(b anthContentBlock) ToolCall {
	args := make(map[string]string)
	var raw map[string]interface{}
	if len(b.Input) > 0 {
		if err := json.Unmarshal(b.Input, &raw); err == nil {
			for k, v := range raw {
				if k == "" || v == nil {
					continue
				}
				args[k] = nativeArgToString(v)
			}
		}
	}
	return ToolCall{ID: b.ID, Name: b.Name, Arguments: args}
}

func joinStrings(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	result := parts[0]
	for _, p := range parts[1:] {
		result += "\n\n" + p
	}
	return result
}

func (p *AnthropicProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	applyExtraHeaders(req, p.headers)
}

// lastResponseIDs holds the last response id of each session, for at most
// limit sessions: beyond that, the session updated longest ago is dropped.
type lastResponseIDs struct {
	mu    sync.Mutex
	limit int
	ids   map[string]*list.Element // session -> element of order
	order *list.List               // *sessionResponseID, most recently updated first
}

type sessionResponseID struct{ session, id string }

func newLastResponseIDs(limit int) *lastResponseIDs {
	return &lastResponseIDs{limit: limit, ids: make(map[string]*list.Element), order: list.New()}
}

// get returns the session's last response id, or nil when it has none.
func (s *lastResponseIDs) get(session string) *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.ids[session]
	if !ok {
		return nil
	}
	id := e.Value.(*sessionResponseID).id
	return &id
}

// put records id as the session's last response id. Requests without a
// session have no previous response.
func (s *lastResponseIDs) put(session, id string) {
	if session == "" || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.ids[session]; ok {
		e.Value.(*sessionResponseID).id = id
		s.order.MoveToFront(e)
		return
	}
	s.ids[session] = s.order.PushFront(&sessionResponseID{session: session, id: id})
	if s.order.Len() > s.limit {
		oldest := s.order.Remove(s.order.Back()).(*sessionResponseID)
		delete(s.ids, oldest.session)
	}
}
