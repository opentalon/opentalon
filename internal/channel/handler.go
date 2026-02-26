package channel

import (
	"context"
	"log"

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
		response, inputForDisplay, err := runner.Run(ctx, sessionKey, content)
		if err != nil {
			log.Printf("handler: run: %v", err)
			return pkg.OutboundMessage{Content: "Error: " + err.Error()}, nil
		}
		outContent := response
		if outContent == "" {
			outContent = "(No response)"
		}
		if msg.ChannelID == "console" && inputForDisplay != "" {
			outContent = "Input to LLM:\n" + inputForDisplay + "\n\n---\n\nResponse:\n" + response
		}
		return pkg.OutboundMessage{
			ConversationID: msg.ConversationID,
			ThreadID:       msg.ThreadID,
			Content:        outContent,
		}, nil
	}
}
