package provider

import (
	"context"
	"time"
)

// DebugEvent is a captured request/response/error from a provider's HTTP
// path, suitable for persistence by a /debug-aware sink. URL is the actual
// endpoint hit; Body is JSON text for request and non-streaming response
// payloads, "Class: message" for error rows.
type DebugEvent struct {
	SessionID string
	TraceID   string
	Direction string // "request" | "response" | "error"
	Status    int
	URL       string
	Body      string
	Timestamp time.Time
}

// DebugEventSink is implemented by anything that wants to persist captured
// LLM-endpoint exchanges. The state-store's async writer (in
// internal/state/store) is the only production implementation; tests use
// in-memory recorders.
//
// The sink lives in package provider rather than in state/store to avoid an
// import cycle and to keep the provider package self-contained — it knows
// about HTTP captures, not about postgres tables.
type DebugEventSink interface {
	Submit(ctx context.Context, e DebugEvent)
}

// DebugContextResolver is consulted before any debug-capture work runs in
// the provider. When enabled is false the provider skips capture entirely
// (zero cost on the LLM hot path for sessions without /debug). When true,
// sessionID and traceID are tagged on every emitted DebugEvent so they
// correlate cleanly in postgres queries and stderr logs.
//
// Wired in main.go from actor.SessionID + logger.TraceID + logger.IsSession-
// Debug. Keeping this a single callback means the provider stays decoupled
// from those packages — no imports, no risk of cycles.
type DebugContextResolver func(ctx context.Context) (sessionID, traceID string, enabled bool)
