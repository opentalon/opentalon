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
	Thinking  *anthThinking `json:"thinking,omitempty"`
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
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Model   string             `json:"model"`
	Content []anthContentBlock `json:"content"`
	Usage   anthUsage          `json:"usage"`
	Error   *anthError         `json:"error,omitempty"`
}

type anthContentBlock struct {
	Type   string           `json:"type"`
	Text   string           `json:"text,omitempty"`
	Source *anthImageSource `json:"source,omitempty"`
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

	content, thinking := p.extractContentAndThinking(anthResp.Content)

	// Log thinking blocks when present.
	if thinking != "" {
		slog.DebugContext(ctx, "anthropic reasoning response",
			"model", anthResp.Model,
			"thinking_len", len(thinking),
			"thinking", thinking,
			"content_len", len(content),
		)
	}

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

// toAnthMessage converts a provider.Message to an anthMessage.
// When the message has file attachments, the content becomes a JSON array of
// content blocks (files first, then the text block); otherwise it is a plain string.
// Image mime types map to "image" blocks, application/pdf maps to "document" block with a
// base64 source, text-like types (text/*, application/json, application/xml) map to
// "document" blocks with a text source, and all other types return an error.
func (p *AnthropicProvider) toAnthMessage(m Message) (anthMessage, error) {
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

func (p *AnthropicProvider) extractContentAndThinking(blocks []anthContentBlock) (content, thinking string) {
	var textParts, thinkParts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking":
			thinkParts = append(thinkParts, b.Text)
		}
	}
	return joinStrings(textParts), joinStrings(thinkParts)
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
