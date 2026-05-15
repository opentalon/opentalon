package provider

import "context"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // tool result message (native function calling)
)

// MessageFile is a binary file (image, document, etc.) attached to a message.
// Only the MimeType and raw Data are required; the provider decides how to encode it.
type MessageFile struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

type Message struct {
	Role       Role          `json:"role"`
	Content    string        `json:"content"`
	Files      []MessageFile `json:"files,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"` // for role=tool messages (native function calling)
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`   // for role=assistant messages with native tool calls
}

// ToolDefinition describes a tool the LLM can call (native function calling).
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"` // JSON Schema object
}

// ToolCall represents a tool call returned by the LLM (native function calling).
type ToolCall struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`      // "plugin.action"
	Arguments map[string]string `json:"arguments"` // parsed args
}

type CompletionRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"` // native tool definitions; nil = text-based tool calling
	MaxTokens       int              `json:"max_tokens,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	Stream          bool             `json:"stream,omitempty"`
	Reasoning       bool             `json:"reasoning,omitempty"`        // enable extended thinking / reasoning
	BudgetTokens    int              `json:"budget_tokens,omitempty"`    // Anthropic: max tokens for thinking (0 = provider default)
	ReasoningEffort string           `json:"reasoning_effort,omitempty"` // OpenAI: "low", "medium", "high" (0 = "medium")
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type CompletionResponse struct {
	ID        string     `json:"id"`
	Model     string     `json:"model"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // native tool calls from LLM; nil = check Content for text-based calls
	Usage     Usage      `json:"usage"`

	// EventID is the session-event id of the llm_response event the
	// provider emitted for this completion (empty when no event sink is
	// configured, or when the response carried a refusal — see EmitLLMRefused).
	// The orchestrator uses this as parent_id on subsequent
	// tool_call_extracted events so the analytics graph links each tool
	// dispatch back to the LLM round that produced it.
	EventID string `json:"-"`
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
