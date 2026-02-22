package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature *float64     `json:"temperature,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

// Complete sends a non-streaming completion request.
func (p *OpenAIProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	oaiReq := p.toOAIRequest(req)
	oaiReq.Stream = false

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
	if len(oaiResp.Choices) > 0 {
		content = oaiResp.Choices[0].Message.Content
	}

	return &CompletionResponse{
		ID:      oaiResp.ID,
		Model:   oaiResp.Model,
		Content: content,
		Usage: Usage{
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
		},
	}, nil
}

// Stream is not yet implemented; returns an error.
func (p *OpenAIProvider) Stream(_ context.Context, _ *CompletionRequest) (ResponseStream, error) {
	return nil, fmt.Errorf("streaming not yet implemented for provider %s", p.id)
}

func (p *OpenAIProvider) toOAIRequest(req *CompletionRequest) oaiRequest {
	msgs := make([]oaiMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = oaiMessage{Role: string(m.Role), Content: m.Content}
	}
	return oaiRequest{
		Model:       req.Model,
		Messages:    msgs,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
}

func (p *OpenAIProvider) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}
