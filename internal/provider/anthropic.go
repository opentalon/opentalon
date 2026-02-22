package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	anthropicDefaultBaseURL = "https://api.anthropic.com"
	anthropicMessagesPath   = "/v1/messages"
	anthropicAPIVersion     = "2023-06-01"
)

// AnthropicProvider implements the Provider interface for the
// Anthropic Messages API.
type AnthropicProvider struct {
	id      string
	baseURL string
	apiKey  string
	models  []ModelInfo
	client  *http.Client
}

// AnthropicOption configures an AnthropicProvider.
type AnthropicOption func(*AnthropicProvider)

// WithAnthropicHTTPClient sets a custom HTTP client.
func WithAnthropicHTTPClient(c *http.Client) AnthropicOption {
	return func(p *AnthropicProvider) { p.client = c }
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
}

type anthMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthResponse struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Model   string             `json:"model"`
	Content []anthContentBlock `json:"content"`
	Usage   anthUsage          `json:"usage"`
	Error   *anthError         `json:"error,omitempty"`
}

type anthContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
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
	anthReq := p.toAnthRequest(req)

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
		return nil, fmt.Errorf("anthropic api error (status %d): %s", httpResp.StatusCode, string(respBody))
	}

	var anthResp anthResponse
	if err := json.Unmarshal(respBody, &anthResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if anthResp.Error != nil {
		return nil, fmt.Errorf("anthropic error [%s]: %s", anthResp.Error.Type, anthResp.Error.Message)
	}

	content := p.extractContent(anthResp.Content)

	return &CompletionResponse{
		ID:      anthResp.ID,
		Model:   anthResp.Model,
		Content: content,
		Usage: Usage{
			InputTokens:  anthResp.Usage.InputTokens,
			OutputTokens: anthResp.Usage.OutputTokens,
		},
	}, nil
}

// Stream is not yet implemented; returns an error.
func (p *AnthropicProvider) Stream(_ context.Context, _ *CompletionRequest) (ResponseStream, error) {
	return nil, fmt.Errorf("streaming not yet implemented for provider %s", p.id)
}

func (p *AnthropicProvider) toAnthRequest(req *CompletionRequest) anthRequest {
	var system string
	msgs := make([]anthMessage, 0, len(req.Messages))

	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			system = m.Content
			continue
		}
		msgs = append(msgs, anthMessage{Role: string(m.Role), Content: m.Content})
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	return anthRequest{
		Model:     req.Model,
		System:    system,
		Messages:  msgs,
		MaxTokens: maxTokens,
	}
}

func (p *AnthropicProvider) extractContent(blocks []anthContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return joinStrings(parts)
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
