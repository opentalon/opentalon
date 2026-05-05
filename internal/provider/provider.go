package provider

import "context"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// MessageFile is a binary file (image, document, etc.) attached to a message.
// Only the MimeType and raw Data are required; the provider decides how to encode it.
type MessageFile struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

type Message struct {
	Role    Role          `json:"role"`
	Content string        `json:"content"`
	Files   []MessageFile `json:"files,omitempty"`
}

type CompletionRequest struct {
	Model        string    `json:"model"`
	Messages     []Message `json:"messages"`
	MaxTokens    int       `json:"max_tokens,omitempty"`
	Temperature  *float64  `json:"temperature,omitempty"`
	Stream       bool      `json:"stream,omitempty"`
	Reasoning    bool      `json:"reasoning,omitempty"`     // enable extended thinking / reasoning
	BudgetTokens int       `json:"budget_tokens,omitempty"` // max tokens for thinking (0 = provider default)
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content string `json:"content"`
	Usage   Usage  `json:"usage"`
}

type StreamChunk struct {
	Content string `json:"content"`
	Done    bool   `json:"done"`
	Model   string `json:"model,omitempty"`
	Usage   Usage  `json:"usage,omitempty"`
}

type ResponseStream interface {
	Recv() (StreamChunk, error)
	Close() error
}

type Provider interface {
	ID() string
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
	Stream(ctx context.Context, req *CompletionRequest) (ResponseStream, error)
	Models() []ModelInfo
	SupportsFeature(feature Feature) bool
}
