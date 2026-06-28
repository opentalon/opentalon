package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Tool names are passed to Anthropic verbatim. The orchestrator's tool
// registry composes fully-qualified names with the "__" separator (see
// pkg/toolfqn), e.g. "timly__timly__list-items" or "_meta__load_tools",
// which already satisfy Anthropic's tool-name pattern
// `^[a-zA-Z0-9_-]{1,128}$` — no wire encoding needed.

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicMessagesPath   = "/v1/messages"
	anthropicAPIVersion     = "2023-06-01"
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
	eventSink emit.Sink // structured session-event sink; nil disables emission
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
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// costForTokens computes input/output cost for tokensIn/tokensOut against
// this provider's configured per-million-token rate for modelID. Returns
// (0, 0) when modelID is not in p.models — emit helpers stamp
// LLMResponsePayload with omitempty so a zero cost simply leaves the
// fields out rather than recording a misleading "free call". Currency
// is unitless: ModelInfo.Cost values are stamped as-is, the operator's
// deployment convention sets the unit. Mirrors openai.go's costForTokens
// so the two pricing paths stay consistent.
func (p *AnthropicProvider) costForTokens(modelID string, tokensIn, tokensOut int) (float64, float64) {
	for _, m := range p.models {
		if m.ID != modelID {
			continue
		}
		return float64(tokensIn) * m.Cost.Input / 1_000_000,
			float64(tokensOut) * m.Cost.Output / 1_000_000
	}
	return 0, 0
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
	Model     string        `json:"model"`
	System    string        `json:"system,omitempty"`
	Messages  []anthMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	Thinking  *anthThinking `json:"thinking,omitempty"`
	Tools     []anthTool    `json:"tools,omitempty"`
}

type anthTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type anthThinking struct {
	Type         string `json:"type"`          // "enabled" or "disabled"
	BudgetTokens int    `json:"budget_tokens"` // max tokens for thinking
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
}

type anthContentBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthImageSource `json:"source,omitempty"`
	// tool_use (assistant emits to invoke a tool)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result (user echoes back the tool result)
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

type anthImageSource struct {
	Type      string `json:"type"`                 // "base64" or "text"
	MediaType string `json:"media_type,omitempty"` // e.g. "image/png"; omitted for text sources
	Data      string `json:"data"`                 // base64-encoded bytes or plain text
}

type anthUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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

	// Log reasoning configuration.
	if anthReq.Thinking != nil {
		slog.DebugContext(ctx, "anthropic reasoning enabled",
			"model", anthReq.Model,
			"thinking_type", anthReq.Thinking.Type,
			"budget_tokens", anthReq.Thinking.BudgetTokens,
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

	// Structured session-event capture: emit metadata about the outbound
	// LLM call immediately before the HTTP send so the row predates
	// llm_response chronologically.
	emit.EmitLLMRequest(ctx, p.eventSink, emit.LLMRequestArgs{
		ModelID:      anthReq.Model,
		MessageCount: len(anthReq.Messages),
		HasTools:     len(anthReq.Tools) > 0,
		MaxTokens:    anthReq.MaxTokens,
	})

	start := time.Now()
	httpResp, err := p.client.Do(httpReq)
	if err != nil {
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

	content, thinking, toolCalls := p.extractContent(anthResp.Content)

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

	costIn, costOut := p.costForTokens(anthReq.Model, anthResp.Usage.InputTokens, anthResp.Usage.OutputTokens)
	eventID := emit.EmitLLMResponse(ctx, p.eventSink, emit.LLMResponseArgs{
		RawContent:         content,
		NativeToolCallsRaw: nativeToolCallsRaw,
		FinishReason:       anthResp.StopReason,
		TokensIn:           anthResp.Usage.InputTokens,
		TokensOut:          anthResp.Usage.OutputTokens,
		CostInput:          costIn,
		CostOutput:         costOut,
		LatencyMS:          latencyMS,
		ProviderResponseID: anthResp.ID,
	})

	return &CompletionResponse{
		ID:        anthResp.ID,
		Model:     anthResp.Model,
		Content:   content,
		ToolCalls: toolCalls,
		Usage: Usage{
			InputTokens:  anthResp.Usage.InputTokens,
			OutputTokens: anthResp.Usage.OutputTokens,
		},
		EventID: eventID,
	}, nil
}

// Stream is not yet implemented; returns an error.
func (p *AnthropicProvider) Stream(_ context.Context, _ *CompletionRequest) (ResponseStream, error) {
	return nil, fmt.Errorf("streaming not yet implemented for provider %s", p.id)
}

func (p *AnthropicProvider) toAnthRequest(req *CompletionRequest) (anthRequest, error) {
	var system string
	msgs := make([]anthMessage, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			system = m.Content
			continue
		}
		msg, err := p.toAnthMessage(m)
		if err != nil {
			return anthRequest{}, err
		}
		msgs = append(msgs, msg)
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

	if req.Reasoning {
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
//     [optional text] + [one tool_use block per ToolCall], so a replay
//     of multi-turn history echoes back the assistant's prior tool
//     invocation in the exact shape Anthropic produced it.
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
// Anthropic emitted: optional preamble text + N tool_use blocks (id,
// name, input). Arguments come back from the orchestrator's typed
// ToolCall.Arguments map[string]string; re-shape to JSON object so
// Anthropic accepts the input field.
func assistantWithToolUseMessage(m Message) (anthMessage, error) {
	var blocks []anthContentBlock
	if m.Content != "" {
		blocks = append(blocks, anthContentBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		// ToolCall.Arguments is map[string]string at the orchestrator boundary
		// (string-coerced to dodge JSON ambiguity around `null`, numeric vs
		// string ids, etc.). Anthropic accepts any JSON value in `input`, so
		// the map serialises as-is.
		argsRaw, err := json.Marshal(tc.Arguments)
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
// dispatcher code provider-agnostic.
func (p *AnthropicProvider) extractContent(blocks []anthContentBlock) (content, thinking string, toolCalls []ToolCall) {
	var textParts, thinkParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking":
			thinkParts = append(thinkParts, b.Text)
		case "tool_use":
			toolCalls = append(toolCalls, anthToolUseToToolCall(b))
		}
	}
	return joinStrings(textParts), joinStrings(thinkParts), toolCalls
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
}
