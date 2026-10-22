package provider

import (
	"context"
	"encoding/json"
)

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
	// Thinking holds the thinking and redacted_thinking blocks of the response
	// an assistant tool-call message came from, each as the JSON the provider
	// returned, so a replay sends them back unchanged. Anthropic only.
	Thinking []json.RawMessage `json:"thinking,omitempty"`
	// ThinkingFingerprint identifies the request prefix the Thinking blocks
	// are bound to (see CompletionResponse). Blocks without one are never
	// replayed.
	ThinkingFingerprint string `json:"thinking_fingerprint,omitempty"`
	// Visibility "hidden" marks a message that IS fed to the model (it rides in
	// role+content) but is dropped from the user-facing transcript — the inverse
	// of the per-message metadata column. Empty means visible. Applies to
	// role=user injected turns only; an assistant reply is always visible.
	Visibility string `json:"visibility,omitempty"`
	// PromptType mirrors the "prompt_type" marker of the persisted
	// messages.metadata column for rows the confirmation flow wrote (the
	// narrated tool-confirmation prompt and the button reply that resolved
	// it). Internal history-filtering signal only — never serialized toward
	// a provider (see orchestrator's stripApprovedConfirmationExchanges).
	PromptType string `json:"-"`
}

// VisibilityHidden is the Message.Visibility value marking a turn that is fed
// to the model but dropped from the user-facing transcript — a system-injected
// turn such as a background-job status note. Empty visibility means visible.
const VisibilityHidden = "hidden"

// Prompt-type markers persisted in messages.metadata by the confirmation flow
// and read back into Message.PromptType for history filtering. The strings are
// wire format shared with the frontend's transcript rebuild — do not rename.
const (
	PromptTypeToolConfirmation     = "tool_confirmation"
	PromptTypeConfirmationResponse = "confirmation_response"
)

// ToolDefinition describes a tool the LLM can call (native function calling).
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"` // JSON Schema object
}

// ToolCall represents a tool call returned by the LLM (native function calling).
type ToolCall struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`      // "plugin__action"
	Arguments map[string]string `json:"arguments"` // parsed args
}

// SystemSegments is the leading system message of a chat-loop request, split
// by how often each part changes: Fixed can be shared across conversations,
// Conversation across the rounds and turns of one conversation, and Round may
// change from one round to the next.
type SystemSegments struct {
	Fixed        string
	Conversation string
	Round        string
}

// Join returns the system message the segments make up: their concatenation
// in order.
func (s SystemSegments) Join() string {
	return s.Fixed + s.Conversation + s.Round
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
	// Segments describes Messages[0], whose content is Segments.Join(). Only
	// the chat loop sets it; nil on every other call.
	Segments *SystemSegments `json:"-"`
}

// Usage is a call's tokens and cost. InputTokens counts every input token,
// including the CacheWriteTokens written to and the CacheReadTokens read from
// the prompt cache. InputCost and OutputCost price the call at the model's
// configured rates; both are 0 for a model without a price.
type Usage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	InputCost        float64 `json:"input_cost,omitempty"`
	OutputCost       float64 `json:"output_cost,omitempty"`
}

type CompletionResponse struct {
	ID        string     `json:"id"`
	Model     string     `json:"model"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"` // native tool calls from LLM; nil = check Content for text-based calls
	// Thinking holds the response's thinking and redacted_thinking blocks as
	// returned (Anthropic only), for the message that stores its tool calls.
	Thinking []json.RawMessage `json:"thinking,omitempty"`
	// ThinkingFingerprint is a hash of what Anthropic binds the Thinking
	// blocks to: the system, the tools and the messages of the request, as
	// sent, without cache markers. Empty when the response has no blocks.
	ThinkingFingerprint string `json:"thinking_fingerprint,omitempty"`
	Usage               Usage  `json:"usage"`

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
