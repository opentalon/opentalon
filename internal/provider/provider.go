package provider

import "context"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type CompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CompletionResponse struct {
	ID      string  `json:"id"`
	Model   string  `json:"model"`
	Content string  `json:"content"`
	Usage   Usage   `json:"usage"`
}

type StreamChunk struct {
	Content string `json:"content"`
	Done    bool   `json:"done"`
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
