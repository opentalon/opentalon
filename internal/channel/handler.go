package channel

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// NewMessageHandler returns a MessageHandler that: ensures session, runs channel-specific
// content preparer (if registered), then runs the message through the Runner and returns the response.
// All handler logic lives in the channel package; main only passes dependencies.
func NewMessageHandler(
	ensureSession pkg.EnsureSessionFunc,
	runner pkg.Runner,
	runAction pkg.RunActionFunc,
	hasAction pkg.HasActionFunc,
) pkg.MessageHandler {
	return func(ctx context.Context, sessionKey string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		ensureSession(sessionKey)
		content := msg.Content
		if prep := pkg.GetContentPreparer(msg.ChannelID); prep != nil {
			content = prep(ctx, content, runAction, hasAction)
		}
		actorID := msg.ChannelID + ":" + msg.SenderID
		ctx = actor.WithActor(ctx, actorID)
		response, inputForDisplay, err := runner.Run(ctx, sessionKey, content, msg.Files...)
		if err != nil {
			log.Printf("handler: run: %v", err)
			return pkg.OutboundMessage{
				ConversationID: msg.ConversationID,
				ThreadID:       msg.ThreadID,
				Content:        friendlyError(err),
				Metadata:       msg.Metadata,
			}, nil
		}
		outContent := response
		if outContent == "" {
			outContent = "(No response)"
		}
		if msg.ChannelID == "console" && inputForDisplay != "" && os.Getenv("LOG_LEVEL") == "debug" {
			outContent = "Input to LLM:\n" + inputForDisplay + "\n\n---\n\nResponse:\n" + response
		}
		return pkg.OutboundMessage{
			ConversationID: msg.ConversationID,
			ThreadID:       msg.ThreadID,
			Content:        outContent,
			Metadata:       msg.Metadata,
		}, nil
	}
}

// friendlyError returns a user-facing message for known error conditions.
func friendlyError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "maximum context length") || strings.Contains(msg, "context_length_exceeded"):
		return "Sorry, this conversation has grown too long for the model to process. Please start a new conversation or clear the session."
	case strings.Contains(msg, "rate_limit") || strings.Contains(msg, "429"):
		return "I'm being rate-limited right now. Please try again in a moment."
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "The request timed out. Please try again."
	default:
		return "Something went wrong processing your message. Please try again or start a new conversation."
	}
}
