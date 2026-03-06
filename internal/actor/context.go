package actor

import "context"

type contextKey struct{}
type sessionKey struct{}

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
