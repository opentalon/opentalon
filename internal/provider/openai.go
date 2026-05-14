package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

const (
	openAIDefaultBaseURL  = "https://api.openai.com/v1"
	openAICompletionsPath = "/chat/completions"
)

// OpenAIProvider implements the Provider interface for any
// OpenAI-compatible API (OpenAI, Azure, Ollama, vLLM, Groq,
// Together, OVH, etc.).
type OpenAIProvider struct {
	id           string
	baseURL      string
	apiKey       string
	models       []ModelInfo
	client       *http.Client
	debugSink    DebugEventSink       // optional; nil disables persistent debug capture
	debugResolve DebugContextResolver // optional; returns (sessionID, traceID, enabled?) for this ctx
	eventSink    emit.Sink            // structured session event sink; always non-nil (NoOpSink default)
}

// OpenAIOption configures an OpenAIProvider.
type OpenAIOption func(*OpenAIProvider)

// WithOpenAIHTTPClient sets a custom HTTP client.
func WithOpenAIHTTPClient(c *http.Client) OpenAIOption {
	return func(p *OpenAIProvider) { p.client = c }
}

// WithOpenAIDebugSink wires a DebugEventSink so request/response/error
// exchanges are persisted whenever the resolver (see WithOpenAIDebugResolver)
// reports enabled=true. Both options are required to actually capture: a nil
// sink or a missing resolver disables the path entirely.
func WithOpenAIDebugSink(s DebugEventSink) OpenAIOption {
	return func(p *OpenAIProvider) { p.debugSink = s }
}

// WithOpenAIDebugResolver wires the per-request resolver that returns
// (sessionID, traceID, enabled?). In production this is fed by
// actor.SessionID + logger.TraceID + logger.IsSessionDebug from main.go.
func WithOpenAIDebugResolver(r DebugContextResolver) OpenAIOption {
	return func(p *OpenAIProvider) { p.debugResolve = r }
}

// WithOpenAISessionEventSink wires a structured session-event sink so
// every LLM HTTP exchange emits llm_request / llm_response (or
// llm_refused when finish_reason indicates a content-safety block) /
// llm_error events. Unlike the debug sink, capture is always-on — no
// per-session opt-in — because structured events are the canonical audit
// trail for analytics, score worker, and the review UI. A nil sink is
// permitted: it is replaced with an emit.NoOpSink so every emission path
// stays nil-safe without per-call checks.
func WithOpenAISessionEventSink(s emit.Sink) OpenAIOption {
	return func(p *OpenAIProvider) {
		if s == nil {
			p.eventSink = emit.NoOpSink{}
			return
		}
		p.eventSink = s
	}
}

// resolveDebug returns capture metadata for ctx. When any wiring is missing
// or the session has not opted in, ok is false and the caller skips capture
// before any body work — keeping the LLM hot path zero-cost for non-debug
// sessions.
func (p *OpenAIProvider) resolveDebug(ctx context.Context) (sessionID, traceID string, ok bool) {
	if p.debugSink == nil || p.debugResolve == nil {
		return "", "", false
	}
	return p.debugResolve(ctx)
}

// NewOpenAIProvider creates a provider for any OpenAI-compatible endpoint.
func NewOpenAIProvider(id, baseURL, apiKey string, models []ModelInfo, opts ...OpenAIOption) *OpenAIProvider {
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	p := &OpenAIProvider{
		id:        id,
		baseURL:   baseURL,
		apiKey:    apiKey,
		models:    models,
		client:    &http.Client{Timeout: 120 * time.Second},
		eventSink: emit.NoOpSink{},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *OpenAIProvider) ID() string { return p.id }

func (p *OpenAIProvider) Models() []ModelInfo { return p.models }

func (p *OpenAIProvider) SupportsFeature(f Feature) bool {
	for _, m := range p.models {
		if m.SupportsFeature(f) {
			return true
		}
	}
	return false
}

// -- OpenAI wire types --

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiRequest struct {
	Model           string            `json:"model"`
	Messages        []oaiMessage      `json:"messages"`
	Tools           []oaiTool         `json:"tools,omitempty"`
	MaxTokens       int               `json:"max_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	StreamOptions   *oaiStreamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"` // "low", "medium", "high" for reasoning models (gpt-oss-120b, o1, etc.)
}

type oaiTool struct {
	Type     string          `json:"type"` // "function"
	Function oaiToolFunction `json:"function"`
}

type oaiToolFunction struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters,omitempty"`
}

type oaiMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"` // reasoning models return thinking here
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`        // native tool calls from LLM
	ToolCallID       string        `json:"tool_call_id,omitempty"`      // for role=tool messages
}

type oaiToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"` // "function"
	Function oaiToolCallFunction `json:"function"`
}

type oaiToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type oaiResponse struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
	Error   *oaiError   `json:"error,omitempty"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// -- OpenAI streaming wire types --

type oaiStreamChunk struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []oaiStreamChoice `json:"choices"`
	Usage   *oaiUsage         `json:"usage,omitempty"`
	Error   *oaiError         `json:"error,omitempty"`
}

type oaiStreamChoice struct {
	Index        int            `json:"index"`
	Delta        oaiStreamDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type oaiStreamDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// oaiResponseStream implements ResponseStream over an SSE connection.
//
// When debug capture is on for the originating session, accumulator collects
// every SSE line the bufio.Scanner returns, joined back with single '\n'
// separators. This is line-normalized SSE replay (it would render any real
// `\r\n\r\n` event boundary as a single '\n'), not byte-for-byte wire
// fidelity — sufficient for diagnosing what the upstream actually sent
// (deltas, malformed chunks, the [DONE] marker) without the cost of a
// TeeReader on the raw response body. On Close() the accumulated body is
// forwarded to onClose, which writes it as a single `direction=response`
// row. We capture the line stream rather than the assembled
// CompletionResponse so /debug shows what came over the network — including
// chunks the orchestrator skipped on parse failure — which is exactly the
// surface area /debug exists to expose.
type oaiResponseStream struct {
	body        io.ReadCloser
	scanner     *bufio.Scanner
	accumulator *streamAccumulator // nil when capture disabled for this stream
	onClose     func(rawBody []byte)
	closed      bool // guard for double-Close (body.Close + onClose flush)

	// --- Session-event capture state. ---
	// Distinct from the debug-only `accumulator` + `onClose` block
	// above: that one is opt-in per session (driven by /debug);
	// everything below is always-on so every stream produces exactly
	// one llm_response / llm_refused / llm_error event at Close,
	// regardless of /debug state.
	//
	// Storing ctx on the struct is deliberate: ResponseStream.Close
	// has no ctx parameter today and a chunked SSE stream is short-
	// lived, so the usual ctx-in-struct guidance (avoid hiding
	// cancellation) does not apply here.
	emitCtx      context.Context //nolint:containedctx // Close has no ctx arg; stream is short-lived. Re-instated defensively so a future containedctx-enabled lint config has the suppression already in place — costs nothing today, prevents a regression search-and-tag later.
	eventSink    emit.Sink
	startTime    time.Time
	lastChunkAt  time.Time // wall-clock of the most recent chunk; used as latency endpoint instead of Close so we don't bill the orchestrator's drain time as LLM latency
	responseID   string
	contentBuf   strings.Builder
	finishReason string
	tokensIn     int
	tokensOut    int
	streamErr    error // non-nil → Close emits llm_error instead of llm_response/llm_refused
}

// streamAccumulator buffers the raw SSE wire bytes for end-of-stream debug
// capture. Bounded so a runaway stream cannot blow up memory: events
// truncate at maxStreamCaptureBytes with a sentinel marker.
type streamAccumulator struct {
	buf       []byte
	truncated bool
}

const maxStreamCaptureBytes = 4 * 1024 * 1024 // 4MB; large gpt-oss responses fit, runaway streams do not

func (a *streamAccumulator) append(line string) {
	if a == nil || a.truncated {
		return
	}
	if len(a.buf)+len(line)+1 > maxStreamCaptureBytes {
		a.buf = append(a.buf, []byte("\n[truncated by /debug capture]\n")...)
		a.truncated = true
		return
	}
	a.buf = append(a.buf, line...)
	a.buf = append(a.buf, '\n')
}

// Complete sends a non-streaming completion request.
func (p *OpenAIProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	oaiReq, err := p.toOAIRequest(req)
	if err != nil {
		return nil, err
	}
	oaiReq.Stream = false

	if oaiReq.ReasoningEffort != "" {
		slog.DebugContext(ctx, "openai reasoning enabled",
			"model", oaiReq.Model,
			"reasoning_effort", oaiReq.ReasoningEffort,
		)
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	slog.DebugContext(ctx, "openai request",
		"model", oaiReq.Model,
		"messages", len(oaiReq.Messages),
		"tools", len(oaiReq.Tools),
		"body_bytes", len(body),
	)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+openAICompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq)

	// Raw HTTP capture: emitted as a slog event for live tailing and (when
	// /debug capture is wired + this session opted in) persisted to the
	// ai_debug_events table. Tagged "openai raw http" + direction so a
	// single `kubectl logs -f | jq 'select(.msg=="openai raw http")'`
	// covers both request and response.
	p.captureRawHTTP(ctx, "request", 0, body, nil)
	// Structured session-event capture: always-on metadata about the
	// outbound LLM call (no full body — bloat). Paired with EmitLLMResponse
	// / EmitLLMError below so analytics get one request row per turn even
	// when the response path fails.
	emit.EmitLLMRequest(ctx, p.eventSink, emit.LLMRequestArgs{
		ModelID:      oaiReq.Model,
		MessageCount: len(oaiReq.Messages),
		HasTools:     len(oaiReq.Tools) > 0,
		MaxTokens:    oaiReq.MaxTokens,
	})

	start := time.Now()
	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		p.captureRawHTTP(ctx, "error", 0, nil, err)
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseChatTransport,
			ResponseBodyText: err.Error(),
		})
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		p.captureRawHTTP(ctx, "error", 0, nil, err)
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseChatReadResponse,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: err.Error(),
		})
		return nil, fmt.Errorf("read response: %w", err)
	}

	p.captureRawHTTP(ctx, "response", httpResp.StatusCode, respBody, nil)
	// Capture latency here — the network/server side of the call is
	// over. Unmarshal cost below is CPU on this host and would skew the
	// "LLM latency" measurement consumed by analytics dashboards.
	latencyMS := time.Since(start).Milliseconds()

	if httpResp.StatusCode != http.StatusOK {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseChatHTTPStatus,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: string(respBody),
		})
		return nil, fmt.Errorf("openai api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseChatUnmarshal,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: string(respBody),
		})
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if oaiResp.Error != nil {
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseChatAPIError,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: oaiResp.Error.Message,
		})
		return nil, fmt.Errorf("openai error [%s]: %s", oaiResp.Error.Type, oaiResp.Error.Message)
	}

	content := ""
	var toolCalls []ToolCall
	if len(oaiResp.Choices) > 0 {
		msg := oaiResp.Choices[0].Message
		content = msg.Content

		// Log reasoning content when present.
		if msg.ReasoningContent != "" {
			slog.DebugContext(ctx, "openai reasoning response",
				"model", oaiResp.Model,
				"reasoning_len", len(msg.ReasoningContent),
				"reasoning", msg.ReasoningContent,
				"content_len", len(content),
			)
		}

		// Log native tool calls when present.
		if len(msg.ToolCalls) > 0 {
			tcNames := make([]string, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				tcNames = append(tcNames, tc.Function.Name)
			}
			slog.DebugContext(ctx, "openai native tool calls",
				"model", oaiResp.Model,
				"count", len(msg.ToolCalls),
				"tools", tcNames,
			)
		}

		// Parse native tool calls from the response.
		for _, tc := range msg.ToolCalls {
			if tc.Type != "function" {
				continue
			}
			args := make(map[string]string)
			var rawArgs map[string]interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &rawArgs); err == nil {
				for k, v := range rawArgs {
					// Drop null values and empty keys — LLMs sometimes
					// emit {"page": null} for optional params or {"": ...}
					// for zero-arg tools. Passing null as the string "null"
					// causes schema validation failures downstream.
					if k == "" || v == nil {
						continue
					}
					args[k] = nativeArgToString(v)
				}
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
	}

	// Capture the provider-emitted ToolCalls JSON verbatim so the
	// downstream llm_response event preserves the on-the-wire shape even
	// if the parser above evolves (drops nulls, normalizes keys, etc).
	var nativeToolCallsRaw json.RawMessage
	finishReason := ""
	if len(oaiResp.Choices) > 0 {
		finishReason = oaiResp.Choices[0].FinishReason
		if len(oaiResp.Choices[0].Message.ToolCalls) > 0 {
			nativeToolCallsRaw, _ = json.Marshal(oaiResp.Choices[0].Message.ToolCalls)
		}
	}

	// Refusal detection: OpenAI signals content-safety blocks via
	// finish_reason="content_filter" (no separate refusal field). Emit
	// llm_refused so analytics keeps refusals distinct from successful
	// generations — they have different remediation paths (prompt tuning
	// vs model swap vs user education).
	if finishReason == openAIFinishReasonContentFilter {
		emit.EmitLLMRefused(ctx, p.eventSink, emit.LLMRefusedArgs{
			RefusalText:      content,
			ContentSafetyHit: openAIFinishReasonContentFilter,
		})
	} else {
		emit.EmitLLMResponse(ctx, p.eventSink, emit.LLMResponseArgs{
			RawContent:         content,
			NativeToolCallsRaw: nativeToolCallsRaw,
			FinishReason:       finishReason,
			TokensIn:           oaiResp.Usage.PromptTokens,
			TokensOut:          oaiResp.Usage.CompletionTokens,
			LatencyMS:          latencyMS,
			ProviderResponseID: oaiResp.ID,
		})
	}

	return &CompletionResponse{
		ID:        oaiResp.ID,
		Model:     oaiResp.Model,
		Content:   content,
		ToolCalls: toolCalls,
		Usage: Usage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}, nil
}

// openAIFinishReasonContentFilter is the wire-level finish_reason that
// OpenAI returns when the response was suppressed by content moderation.
// Centralised here so the Complete and Stream paths agree on the spelling.
const openAIFinishReasonContentFilter = "content_filter"

// Phase labels for emit.LLMErrorArgs. Hoisted into named constants so
// the spelling is asserted by the compiler at every emission and test
// site — a typo in a string literal would otherwise silently fragment
// analytics dashboards that group by Phase.
//
// Naming convention: <call_type>.<failure_mode>, dot-separated, all
// lowercase. <call_type> is "chat" for the non-streaming Complete path
// and "stream" for the streaming Stream/Recv/Close path.
//
// VALUES ARE WIRE-STABLE. Once persisted into session_events.payload as
// the Phase field, the literal strings here become part of the analytics
// surface that downstream tooling (api-plugin, score worker, Rails UI)
// groups by. Renaming a constant's *value* (the right-hand side) silently
// rotates analytics dashboards — historical rows keep the old spelling
// while new rows get the new one. Add new phases here, never rewrite
// existing ones. The Go identifier (left-hand side) can be renamed
// freely since it's an internal symbol.
const (
	phaseChatTransport    = "chat.transport"
	phaseChatReadResponse = "chat.read_response"
	phaseChatHTTPStatus   = "chat.http_status"
	phaseChatUnmarshal    = "chat.unmarshal"
	phaseChatAPIError     = "chat.api_error"
	phaseStreamTransport  = "stream.transport"
	phaseStreamHTTPStatus = "stream.http_status"
	phaseStreamRecv       = "stream.recv"
)

// Stream sends a streaming completion request and returns a ResponseStream
// that yields chunks as they arrive via SSE.
func (p *OpenAIProvider) Stream(ctx context.Context, req *CompletionRequest) (ResponseStream, error) {
	oaiReq, err := p.toOAIRequest(req)
	if err != nil {
		return nil, err
	}
	oaiReq.Stream = true
	oaiReq.StreamOptions = &oaiStreamOptions{IncludeUsage: true}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+openAICompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq)

	// Request is captured the same way as in the non-streaming path; the
	// streaming response is captured at end-of-stream by the accumulator
	// inside oaiResponseStream.
	p.captureRawHTTP(ctx, "request", 0, body, nil)
	emit.EmitLLMRequest(ctx, p.eventSink, emit.LLMRequestArgs{
		ModelID:      oaiReq.Model,
		MessageCount: len(oaiReq.Messages),
		HasTools:     len(oaiReq.Tools) > 0,
		MaxTokens:    oaiReq.MaxTokens,
	})

	// Use a client without timeout for streaming; context handles cancellation.
	streamClient := &http.Client{}
	start := time.Now()
	httpResp, err := streamClient.Do(httpReq)
	if err != nil {
		p.captureRawHTTP(ctx, "error", 0, nil, err)
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseStreamTransport,
			ResponseBodyText: err.Error(),
		})
		return nil, fmt.Errorf("http request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		defer func() { _ = httpResp.Body.Close() }()
		respBody, _ := io.ReadAll(httpResp.Body)
		p.captureRawHTTP(ctx, "response", httpResp.StatusCode, respBody, nil)
		emit.EmitLLMError(ctx, p.eventSink, emit.LLMErrorArgs{
			Phase:            phaseStreamHTTPStatus,
			StatusCode:       httpResp.StatusCode,
			ResponseBodyText: string(respBody),
		})
		return nil, fmt.Errorf("openai api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	stream := &oaiResponseStream{
		body:      httpResp.Body,
		scanner:   bufio.NewScanner(httpResp.Body),
		emitCtx:   ctx,
		eventSink: p.eventSink,
		startTime: start,
	}
	// Wire end-of-stream capture only when this session opted in. Sessions
	// without /debug get the lighter struct without the buffer.
	if _, _, ok := p.resolveDebug(ctx); ok {
		stream.accumulator = &streamAccumulator{}
		stream.onClose = func(rawBody []byte) {
			p.captureRawHTTP(ctx, "response", httpResp.StatusCode, rawBody, nil)
		}
	}
	return stream, nil
}

func (s *oaiResponseStream) Recv() (StreamChunk, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()
		// Capture every SSE line (data: payloads, blank separators, anything
		// the upstream sent) so /debug shows the wire-level stream verbatim.
		s.accumulator.append(line)

		// SSE format: lines prefixed with "data: "
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		// OpenAI signals end-of-stream with "data: [DONE]"
		if data == "[DONE]" {
			return StreamChunk{Done: true}, nil
		}

		var chunk oaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // skip malformed chunks
		}
		if chunk.Error != nil {
			err := fmt.Errorf("openai stream error [%s]: %s", chunk.Error.Type, chunk.Error.Message)
			if s.streamErr == nil {
				// First error wins: a caller that ignored the error
				// and re-entered Recv must not overwrite the original
				// cause that Close will surface as llm_error.
				s.streamErr = err
			}
			return StreamChunk{}, err
		}

		// Accumulate session-event state. These fields drive the single
		// llm_response / llm_refused / llm_error emission in Close. The
		// usage and finish_reason fields land on the final usage chunk in
		// most OpenAI deployments, so the last write wins by construction.
		s.lastChunkAt = time.Now()
		if chunk.ID != "" && s.responseID == "" {
			s.responseID = chunk.ID
		}
		if chunk.Usage != nil {
			s.tokensIn = chunk.Usage.PromptTokens
			s.tokensOut = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].FinishReason != nil {
				s.finishReason = *chunk.Choices[0].FinishReason
			}
			s.contentBuf.WriteString(chunk.Choices[0].Delta.Content)
		}

		// Usage-only chunk (sent as the final chunk when stream_options.include_usage is true).
		// It has no choices but carries the complete usage for the request.
		if chunk.Usage != nil && len(chunk.Choices) == 0 {
			return StreamChunk{
				Model: chunk.Model,
				Usage: Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				},
			}, nil
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			sc := StreamChunk{Content: chunk.Choices[0].Delta.Content, Model: chunk.Model}
			if chunk.Usage != nil {
				sc.Usage = Usage{
					InputTokens:  chunk.Usage.PromptTokens,
					OutputTokens: chunk.Usage.CompletionTokens,
				}
			}
			return sc, nil
		}
		// Empty delta (e.g. role-only chunk) — skip and read next line.
	}

	if err := s.scanner.Err(); err != nil {
		wrapped := fmt.Errorf("read stream: %w", err)
		if s.streamErr == nil {
			s.streamErr = wrapped
		}
		return StreamChunk{}, wrapped
	}
	// Scanner exhausted without [DONE] — treat as done.
	return StreamChunk{Done: true}, nil
}

// Close flushes the captured body (if any) to the debug sink and closes the
// underlying response body. Idempotent: a second Close is a no-op rather
// than re-running onClose or returning the "use of closed network
// connection" error from the body. The orchestrator calls Close via defer,
// so a panic-during-Recv path could in principle hit Close twice if the
// caller has its own defer too — guarding here costs one bool.
//
// Close is also the single emission site for the stream's structured
// llm_response / llm_refused / llm_error event: the per-chunk Recv path
// accumulates content, finish_reason, tokens and any stream error onto
// fields of s, and Close fires exactly one event per stream based on
// what landed. The closed guard above doubles as the "emitted-once"
// guard so a double Close cannot double-emit.
func (s *oaiResponseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.onClose != nil && s.accumulator != nil {
		s.onClose(s.accumulator.buf)
		s.onClose = nil
	}
	s.emitStreamEnd()
	return s.body.Close()
}

// emitStreamEnd writes the single llm_response / llm_refused / llm_error
// event that summarises this stream. Called once from Close.
func (s *oaiResponseStream) emitStreamEnd() {
	if s.eventSink == nil {
		// Defensive: streams constructed by tests outside Stream() may
		// leave the sink unset. Skip emission rather than panic.
		return
	}
	// Latency endpoint is the last chunk timestamp, not Close — so a
	// slow orchestrator that holds the stream open while processing
	// tool calls doesn't inflate the measured "LLM latency". When no
	// chunks were received (transport-error mid-stream or HTTP-status
	// error already routed above), fall back to time.Now() so the
	// number stays non-zero for analytics.
	endpoint := s.lastChunkAt
	if endpoint.IsZero() {
		endpoint = time.Now()
	}
	latencyMS := endpoint.Sub(s.startTime).Milliseconds()
	if s.streamErr != nil {
		emit.EmitLLMError(s.emitCtx, s.eventSink, emit.LLMErrorArgs{
			Phase:            phaseStreamRecv,
			ResponseBodyText: s.streamErr.Error(),
		})
		return
	}
	content := s.contentBuf.String()
	if s.finishReason == openAIFinishReasonContentFilter {
		emit.EmitLLMRefused(s.emitCtx, s.eventSink, emit.LLMRefusedArgs{
			RefusalText:      content,
			ContentSafetyHit: openAIFinishReasonContentFilter,
		})
		return
	}
	emit.EmitLLMResponse(s.emitCtx, s.eventSink, emit.LLMResponseArgs{
		RawContent:         content,
		FinishReason:       s.finishReason,
		TokensIn:           s.tokensIn,
		TokensOut:          s.tokensOut,
		LatencyMS:          latencyMS,
		ProviderResponseID: s.responseID,
	})
}

func (p *OpenAIProvider) toOAIRequest(req *CompletionRequest) (oaiRequest, error) {
	msgs := make([]oaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if len(m.Files) > 0 {
			return oaiRequest{}, fmt.Errorf("provider %s does not support file attachments", p.id)
		}
		// Skip malformed messages from old sessions:
		// - role=tool without tool_call_id (OpenAI API rejects these)
		// - role=assistant with empty content and no tool calls (orphans)
		if m.Role == RoleTool && m.ToolCallID == "" {
			continue
		}
		if m.Role == RoleAssistant && m.Content == "" && len(m.ToolCalls) == 0 {
			continue
		}
		oMsg := oaiMessage{Role: string(m.Role), Content: m.Content}
		// Native tool calling: pass tool_call_id for tool result messages.
		if m.Role == RoleTool && m.ToolCallID != "" {
			oMsg.ToolCallID = m.ToolCallID
		}
		// Native tool calling: pass tool_calls for assistant messages.
		for _, tc := range m.ToolCalls {
			args, _ := json.Marshal(tc.Arguments)
			oMsg.ToolCalls = append(oMsg.ToolCalls, oaiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: oaiToolCallFunction{
					Name:      tc.Name,
					Arguments: string(args),
				},
			})
		}
		msgs = append(msgs, oMsg)
	}
	oai := oaiRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	if req.Reasoning {
		oai.ReasoningEffort = req.ReasoningEffort
		if oai.ReasoningEffort == "" {
			oai.ReasoningEffort = "medium" // default effort
		}
	}
	// Native tool calling: pass tool definitions so the LLM returns
	// structured tool_calls instead of text-based [tool_call] blocks.
	for _, t := range req.Tools {
		oai.Tools = append(oai.Tools, oaiTool{
			Type:     "function",
			Function: oaiToolFunction(t),
		})
	}
	return oai, nil
}

func (p *OpenAIProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// rawJSONOrString returns body as json.RawMessage when the bytes are valid
// JSON (so the JSON log handler embeds it inline), or as the literal
// string otherwise (so an HTTP error page or a truncated chunk still
// surfaces in the log instead of vanishing). Used by the raw HTTP
// capture path on the response side, where upstream surprises are most
// likely.
func rawJSONOrString(body []byte) any {
	var probe json.RawMessage
	if json.Unmarshal(body, &probe) == nil {
		return json.RawMessage(body)
	}
	return string(body)
}

// captureRawHTTP is the single point where raw OpenAI HTTP exchanges are
// surfaced — both as a slog event (for live tailing on stderr) and, when
// /debug capture is wired and the session opted in, as a row in the
// ai_debug_events table.
//
// The slog call is unconditional at DebugContext level — the global handler
// chain decides what to emit: the session-debug wrapper promotes it to Info
// for sessions that toggled /debug, the underlying JSON handler still gates
// non-debug sessions at the configured LOG_LEVEL.
//
// Persistence is gated on resolveDebug() reporting enabled=true so non-debug
// sessions cost nothing beyond the slog call (which itself short-circuits
// when the level filters it out).
//
// captureErr is non-nil for transport / read failures (DNS, dial, body
// read aborts); in that case direction is forced to "error" and body is
// ignored — the failure class+message become the captured Body so /debug
// histories show the failure inline with successful exchanges.
func (p *OpenAIProvider) captureRawHTTP(ctx context.Context, direction string, status int, body []byte, captureErr error) {
	url := p.baseURL + openAICompletionsPath
	if captureErr == nil {
		slog.DebugContext(ctx, "openai raw http",
			"direction", direction,
			"url", url,
			"status", status,
			"body", rawJSONOrString(body),
		)
	}
	sessionID, traceID, ok := p.resolveDebug(ctx)
	if !ok {
		return
	}
	rec := DebugEvent{
		SessionID: sessionID,
		TraceID:   traceID,
		Direction: direction,
		Status:    status,
		URL:       url,
		Timestamp: time.Now().UTC(),
	}
	if captureErr != nil {
		rec.Direction = "error"
		rec.Body = fmt.Sprintf("%T: %s", captureErr, captureErr.Error())
	} else {
		rec.Body = string(body)
	}
	p.debugSink.Submit(ctx, rec)
}

// nativeArgToString converts a JSON-decoded value to a string suitable for
// the wire-level map[string]string in ToolCall.Arguments.
//
// nil values are normally filtered at the call site (LLMs emit null for
// optional params they don't intend to set). The guard here is defense-in-depth.
func nativeArgToString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprintf("%v", x)
		}
		if x == math.Trunc(x) && math.Abs(x) < 1e18 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return string(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}
