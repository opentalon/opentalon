package channel

import (
	"context"
	"log"
	"sync"
)

// Runner runs a user message through the orchestrator and returns the response.
// inputForDisplay is optional (e.g. what was sent to the LLM); channels may use it for display.
type Runner interface {
	Run(ctx context.Context, sessionKey, content string) (response string, inputForDisplay string, err error)
}

// RunAction runs a single plugin action. Used by channel-specific preparers.
type RunActionFunc func(ctx context.Context, plugin, action string, args map[string]string) (string, error)

// HasActionFunc reports whether a plugin action is available.
type HasActionFunc func(plugin, action string) bool

// ContentPreparer is channel-specific pre-processing: it can transform user content
// before it is sent to the Runner. Channels register their preparer via RegisterContentPreparer in init().
type ContentPreparer func(ctx context.Context, content string, runAction RunActionFunc, hasAction HasActionFunc) string

var (
	contentPreparersMu sync.RWMutex
	contentPreparers   = make(map[string]ContentPreparer)
)

// RegisterContentPreparer registers a content preparer for a channel ID.
// Called from channel packages in init().
func RegisterContentPreparer(channelID string, p ContentPreparer) {
	contentPreparersMu.Lock()
	defer contentPreparersMu.Unlock()
	contentPreparers[channelID] = p
}

func getContentPreparer(channelID string) ContentPreparer {
	contentPreparersMu.RLock()
	defer contentPreparersMu.RUnlock()
	return contentPreparers[channelID]
}

// EnsureSessionFunc is called to ensure a session exists for the given key before running.
type EnsureSessionFunc func(sessionKey string)

// NewMessageHandler returns a MessageHandler that: ensures session, runs channel-specific
// content preparer (if registered), then runs the message through the Runner and returns the response.
// All handler logic lives in the channel package; main only passes dependencies.
func NewMessageHandler(
	ensureSession EnsureSessionFunc,
	runner Runner,
	runAction RunActionFunc,
	hasAction HasActionFunc,
) MessageHandler {
	return func(ctx context.Context, sessionKey string, msg InboundMessage) (OutboundMessage, error) {
		ensureSession(sessionKey)
		content := msg.Content
		if prep := getContentPreparer(msg.ChannelID); prep != nil {
			content = prep(ctx, content, runAction, hasAction)
		}
		response, inputForDisplay, err := runner.Run(ctx, sessionKey, content)
		if err != nil {
			log.Printf("handler: run: %v", err)
			return OutboundMessage{Content: "Error: " + err.Error()}, nil
		}
		outContent := response
		if outContent == "" {
			outContent = "(No response)"
		}
		if msg.ChannelID == "console" && inputForDisplay != "" {
			outContent = "Input to LLM:\n" + inputForDisplay + "\n\n---\n\nResponse:\n" + response
		}
		return OutboundMessage{
			ConversationID: msg.ConversationID,
			ThreadID:       msg.ThreadID,
			Content:        outContent,
		}, nil
	}
}
