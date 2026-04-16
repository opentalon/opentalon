package channel

import (
	"context"
	"log/slog"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/profile"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ProfileVerifier is the subset of profile.Verifier used by the handler.
type ProfileVerifier interface {
	Verify(ctx context.Context, token string) (*profile.Profile, error)
}

// NewMessageHandler returns a MessageHandler that: ensures session, verifies profile token (if
// verifier is non-nil), runs channel-specific content preparer (if registered), then runs the
// message through the Runner and returns the response.
func NewMessageHandler(
	ensureSession pkg.EnsureSessionFunc,
	runner pkg.Runner,
	runAction pkg.RunActionFunc,
	hasAction pkg.HasActionFunc,
	verifier ProfileVerifier,
) pkg.MessageHandler {
	return func(ctx context.Context, sessionKey string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		// Profile verification: required when verifier is configured.
		if verifier != nil {
			token := msg.Metadata["profile_token"]
			if token == "" {
				return errorResponse(msg, "profile token required"), nil
			}
			p, err := verifier.Verify(ctx, token)
			if err != nil {
				slog.Warn("profile verification failed", "error", err, "channel", msg.ChannelID)
				return errorResponse(msg, "authentication failed"), nil
			}
			p.ChannelID = msg.ChannelID
			ctx = profile.WithProfile(ctx, p)
			// Scope session to entity so profiles cannot access each other's history.
			sessionKey = p.EntityID + ":" + sessionKey
			// Use entity ID as actor for memory scoping and permission checks.
			ctx = actor.WithActor(ctx, p.EntityID)
		} else {
			// No profile system: use the classic channel:sender actor.
			ctx = actor.WithActor(ctx, msg.ChannelID+":"+msg.SenderID)
		}

		// Carry the inbound conversation id so scheduler jobs (and anything
		// else creating deferred work) can deliver results back to this chat.
		ctx = actor.WithConversationID(ctx, msg.ConversationID)

		ensureSession(sessionKey)
		content := msg.Content
		if prep := pkg.GetContentPreparer(msg.ChannelID); prep != nil {
			content = prep(ctx, content, runAction, hasAction)
		}
		response, inputForDisplay, err := runner.Run(ctx, sessionKey, content, msg.Files...)
		if err != nil {
			logger.FromContext(ctx).Error("handler run failed", "error", err)
			return pkg.OutboundMessage{
				ConversationID: msg.ConversationID,
				ThreadID:       msg.ThreadID,
				Content:        friendlyError(err),
				Metadata:       safeMetadata(msg.Metadata),
			}, nil
		}
		outContent := response
		if outContent == "" {
			outContent = "(No response)"
		}
		if msg.ChannelID == "console" && inputForDisplay != "" && slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			outContent = "Input to LLM:\n" + inputForDisplay + "\n\n---\n\nResponse:\n" + response
		}
		return pkg.OutboundMessage{
			ConversationID: msg.ConversationID,
			ThreadID:       msg.ThreadID,
			Content:        outContent,
			Metadata:       safeMetadata(msg.Metadata),
		}, nil
	}
}

func errorResponse(msg pkg.InboundMessage, text string) pkg.OutboundMessage {
	return pkg.OutboundMessage{
		ConversationID: msg.ConversationID,
		ThreadID:       msg.ThreadID,
		Content:        text,
		Metadata:       safeMetadata(msg.Metadata),
	}
}

// safeMetadata returns a copy of m with sensitive keys removed.
func safeMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	delete(out, "profile_token")
	return out
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
