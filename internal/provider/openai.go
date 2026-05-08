package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	openAIDefaultBaseURL  = "https://api.openai.com/v1"
	openAICompletionsPath = "/chat/completions"
)

// OpenAIProvider implements the Provider interface for any
// OpenAI-compatible API (OpenAI, Azure, Ollama, vLLM, Groq,
// Together, OVH, etc.).
type OpenAIProvider struct {
	id      string
	baseURL string
	apiKey  string
	models  []ModelInfo
	client  *http.Client
}

// OpenAIOption configures an OpenAIProvider.
type OpenAIOption func(*OpenAIProvider)

// WithOpenAIHTTPClient sets a custom HTTP client.
func WithOpenAIHTTPClient(c *http.Client) OpenAIOption {
	return func(p *OpenAIProvider) { p.client = c }
}

// NewOpenAIProvider creates a provider for any OpenAI-compatible endpoint.
func NewOpenAIProvider(id, baseURL, apiKey string, models []ModelInfo, opts ...OpenAIOption) *OpenAIProvider {
	if baseURL == "" {
		baseURL = openAIDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	p := &OpenAIProvider{
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
	Index   int        `json:"index"`
	Message oaiMessage `json:"message"`
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
type oaiResponseStream struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
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

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+openAICompletionsPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	p.setHeaders(httpReq)

	httpResp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var oaiResp oaiResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if oaiResp.Error != nil {
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
			slog.DebugContext(ctx, "openai native tool calls",
				"model", oaiResp.Model,
				"count", len(msg.ToolCalls),
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
					args[k] = fmt.Sprintf("%v", v)
				}
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			})
		}
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

	// Use a client without timeout for streaming; context handles cancellation.
	streamClient := &http.Client{}
	httpResp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		defer func() { _ = httpResp.Body.Close() }()
		respBody, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("openai api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	return &oaiResponseStream{
		body:    httpResp.Body,
		scanner: bufio.NewScanner(httpResp.Body),
	}, nil
}

func (s *oaiResponseStream) Recv() (StreamChunk, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()

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
			return StreamChunk{}, fmt.Errorf("openai stream error [%s]: %s", chunk.Error.Type, chunk.Error.Message)
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
		return StreamChunk{}, fmt.Errorf("read stream: %w", err)
	}
	// Scanner exhausted without [DONE] — treat as done.
	return StreamChunk{Done: true}, nil
}

func (s *oaiResponseStream) Close() error {
	return s.body.Close()
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
