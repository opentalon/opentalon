package actor

import "context"

type contextKey struct{}
type sessionKey struct{}
type conversationKey struct{}
type confirmationKey struct{}
type groupKey struct{}
type visibilityKey struct{}

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

// WithGroupID returns a context that carries the caller's group id — the
// tenant/account scope the actor belongs to, as resolved by the profile
// verifier (Profile.Group). It sits alongside the actor id: the actor
// identifies who is talking, the group identifies which account they are
// acting in. Emitted session events carry it so an out-of-process consumer
// can scope per-account state (e.g. a UI activity indicator) without having
// to re-resolve the actor's account itself. When empty, the original context
// is returned unchanged.
func WithGroupID(ctx context.Context, groupID string) context.Context {
	if groupID == "" {
		return ctx
	}
	return context.WithValue(ctx, groupKey{}, groupID)
}

// GroupID returns the group id from the context, or empty string if not set.
func GroupID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(groupKey{})
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

// WithVisibility attaches a per-message visibility ("hidden") to the context so
// the orchestrator can stamp it on the inbound user turn before persisting. A
// hidden turn is fed to the model but dropped from the user-facing transcript;
// it is set from a channel's inbound metadata["visibility"].
func WithVisibility(ctx context.Context, visibility string) context.Context {
	if visibility == "" {
		return ctx
	}
	return context.WithValue(ctx, visibilityKey{}, visibility)
}

// Visibility returns the per-message visibility from the context, or empty
// string if not set.
func Visibility(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(visibilityKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
