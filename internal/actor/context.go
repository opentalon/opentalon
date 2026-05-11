package actor

import "context"

type contextKey struct{}
type sessionKey struct{}
type conversationKey struct{}
type confirmationKey struct{}

// WithActor returns a context that carries the given actor ID (e.g. channel_id:sender_id).
// Use Actor(ctx) to retrieve it. When the request has no actor, do not call WithActor.
func WithActor(ctx context.Context, actorID string) context.Context {
	if actorID == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, actorID)
}

// Actor returns the actor ID from the context, or empty string if not set.
func Actor(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(contextKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// WithSessionID returns a context that carries the current session ID (e.g. for clear_session).
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, sessionID)
}

// SessionID returns the session ID from the context, or empty string if not set.
func SessionID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(sessionKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// WithConversationID returns a context that carries the inbound message's
// conversation ID (e.g. a Telegram chat_id or Slack channel ID). Scheduler jobs
// use this to deliver results back to the specific conversation where they
// were created. When empty, the original context is returned unchanged.
func WithConversationID(ctx context.Context, conversationID string) context.Context {
	if conversationID == "" {
		return ctx
	}
	return context.WithValue(ctx, conversationKey{}, conversationID)
}

// ConversationID returns the conversation ID from the context, or empty string if not set.
func ConversationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(conversationKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// WithConfirmationDecision attaches the frontend's explicit confirmation
// decision ("approve" or "reject") to the context. The orchestrator reads
// this to bypass LLM-based classification when the frontend sends a
// structured signal via inbound metadata["confirmation"].
func WithConfirmationDecision(ctx context.Context, decision string) context.Context {
	if decision == "" {
		return ctx
	}
	return context.WithValue(ctx, confirmationKey{}, decision)
}

// ConfirmationDecision returns the explicit confirmation decision from
// the context, or empty string if not set.
func ConfirmationDecision(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(confirmationKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
