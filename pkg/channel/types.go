package channel

import (
	"context"
	"fmt"
	"time"
)

// InboundMessage is a message from a channel (user → core).
type InboundMessage struct {
	ChannelID      string            `yaml:"channel_id" json:"channel_id"`
	ConversationID string            `yaml:"conversation_id" json:"conversation_id"`
	ThreadID       string            `yaml:"thread_id" json:"thread_id"`
	SenderID       string            `yaml:"sender_id" json:"sender_id"`
	SenderName     string            `yaml:"sender_name" json:"sender_name"`
	Content        string            `yaml:"content" json:"content"`
	Files          []FileAttachment  `yaml:"files,omitempty" json:"files,omitempty"`
	Metadata       map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Timestamp      time.Time         `yaml:"timestamp" json:"timestamp"`
}

// OutboundMessage is a message from core to a channel.
type OutboundMessage struct {
	ConversationID string            `yaml:"conversation_id" json:"conversation_id"`
	ThreadID       string            `yaml:"thread_id" json:"thread_id"`
	Content        string            `yaml:"content" json:"content"`
	Files          []FileAttachment  `yaml:"files,omitempty" json:"files,omitempty"`
	Metadata       map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// FileAttachment describes a file sent with a message.
type FileAttachment struct {
	Name     string `yaml:"name" json:"name"`
	MimeType string `yaml:"mime_type" json:"mime_type"`
	Data     []byte `yaml:"data,omitempty" json:"data,omitempty"`
	Size     int64  `yaml:"size" json:"size"`
}

// Capabilities declares what a channel supports.
type Capabilities struct {
	ID               string `yaml:"id" json:"id"`
	Name             string `yaml:"name" json:"name"`
	Threads          bool   `yaml:"threads" json:"threads"`
	Files            bool   `yaml:"files" json:"files"`
	Reactions        bool   `yaml:"reactions" json:"reactions"`
	Edits            bool   `yaml:"edits" json:"edits"`
	MaxMessageLength int64  `yaml:"max_message_length" json:"max_message_length"`
}

// Channel is the interface that external channel plugins implement.
// The core uses this interface regardless of the underlying transport
// (binary subprocess, remote gRPC, Docker, webhook, or WebSocket).
type Channel interface {
	ID() string
	Capabilities() Capabilities
	Start(ctx context.Context, inbox chan<- InboundMessage) error
	Send(ctx context.Context, msg OutboundMessage) error
	Stop() error
}

// PluginMode identifies how the core connects to a channel plugin.
type PluginMode int

const (
	ModeBinary    PluginMode = iota // local subprocess
	ModeGRPC                        // remote gRPC address
	ModeDocker                      // Docker container
	ModeWebhook                     // HTTP webhook
	ModeWebSocket                   // WebSocket connection
)

func (m PluginMode) String() string {
	switch m {
	case ModeBinary:
		return "binary"
	case ModeGRPC:
		return "grpc"
	case ModeDocker:
		return "docker"
	case ModeWebhook:
		return "webhook"
	case ModeWebSocket:
		return "websocket"
	default:
		return "unknown"
	}
}

// Runner runs a user message through the orchestrator and returns the response.
// InputForDisplay is optional (e.g. what was sent to the LLM); channels may use it for display.
type Runner interface {
	Run(ctx context.Context, sessionKey, content string) (response string, inputForDisplay string, err error)
}

// RunActionFunc runs a single plugin action. Used by channel-specific preparers.
type RunActionFunc func(ctx context.Context, plugin, action string, args map[string]string) (string, error)

// HasActionFunc reports whether a plugin action is available.
type HasActionFunc func(plugin, action string) bool

// ContentPreparer is channel-specific pre-processing: it can transform user content
// before it is sent to the Runner. Channels register their preparer via RegisterContentPreparer in init().
type ContentPreparer func(ctx context.Context, content string, runAction RunActionFunc, hasAction HasActionFunc) string

// EnsureSessionFunc is called to ensure a session exists for the given key before running.
type EnsureSessionFunc func(sessionKey string)

// MessageHandler is called when an inbound message arrives. The implementation
// feeds the message to the orchestrator and returns the response.
type MessageHandler func(ctx context.Context, sessionKey string, msg InboundMessage) (OutboundMessage, error)

// SessionKey builds a deterministic session identifier from the
// channel, conversation, and thread triple.
func SessionKey(channelID, conversationID, threadID string) string {
	if threadID == "" {
		return fmt.Sprintf("%s:%s", channelID, conversationID)
	}
	return fmt.Sprintf("%s:%s:%s", channelID, conversationID, threadID)
}
