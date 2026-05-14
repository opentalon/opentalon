// Package emit is the producer side of the session_events log.
//
// Subsystems (provider wrapper, orchestrator, tool dispatcher, parser,
// planner, …) never marshal payloads or call the underlying writer
// directly. Instead each event type has a typed helper here —
// EmitLLMResponse, EmitToolCallExtracted, … — that takes a Sink and an
// args struct, then encapsulates:
//
//   - schema-version stamping (Header.V from the matching <Type>Version
//     constant in package events),
//   - UTF-8 sanitization (events.SanitizeUTF8) for free-form bytes,
//   - 4 KB excerpt capping (events.Excerpt) with the truncated flag,
//   - SHA256 of raw content where the payload demands it,
//   - JSON marshalling,
//   - session-id lookup from ctx via actor.SessionID,
//   - parent-event-id lookup from ctx via emit.ParentID.
//
// This is the compile-time + runtime gate that makes the producer side
// of session_events "automatic and stable" per the design memo: the only
// way to write an event is through these helpers, so the conventions
// can't drift.
//
// Always-on by design — unlike provider.DebugEventSink, which uses a
// DebugContextResolver callback to gate capture per-session via the
// /debug flag, session_events has no per-session disable. Structured
// events are the canonical audit trail for analytics, score worker, and
// the Rails review UI: gating it per-session would defeat the purpose.
// The runtime cost is a fixed Marshal + non-blocking channel send per
// event; the writer drops on overflow rather than back-pressuring the
// orchestrator hot path. A nil Sink (or NoOpSink, the default when no
// state DB is configured) is the only off-switch.
//
// Why an emit.Event struct separate from store.SessionEvent — the
// state/store package imports provider for message types, so any package
// that needs to be imported by provider (this one, eventually) cannot
// itself import store without creating a cycle. emit.Event is the
// in-flight DTO; an adapter in cmd/opentalon/main.go converts it to
// store.SessionEvent for the writer.
package emit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/opentalon/opentalon/internal/actor"
)

// Event is one structured session event before persistence.
//
// Producers don't build this directly — Emit<Type> helpers fill it from
// ctx + a typed args struct. The adapter in cmd/opentalon converts to
// store.SessionEvent before Submit.
type Event struct {
	SessionID  string
	EventType  string
	ParentID   string
	DurationMS int64
	Payload    json.RawMessage
}

// Sink receives one structured event from a producer. Implementations
// must not block: producers call Emit on the orchestrator hot path, and
// the production implementation is an async buffered writer that drops
// on overflow rather than back-pressuring.
type Sink interface {
	Emit(ctx context.Context, evt Event)
}

// NoOpSink discards every event. Use it as the zero-value default in
// tests or in code paths that have no state store configured.
type NoOpSink struct{}

// Emit satisfies Sink.
func (NoOpSink) Emit(context.Context, Event) {}

// ----- parent-event context carrier -----

type parentEventIDKey struct{}

// WithParent returns a context that carries parentEventID as the parent
// of any event emitted with it.
//
// Pattern: at a logical boundary (tool dispatch, planner step, …) the
// caller captures the event_id it just emitted via Emit<Type> and wraps
// the ctx with WithParent before passing it deeper. Helpers further down
// the stack auto-populate Event.ParentID from this slot.
//
// Empty parentEventID is a no-op — the original context is returned
// unchanged.
func WithParent(ctx context.Context, parentEventID string) context.Context {
	if parentEventID == "" {
		return ctx
	}
	return context.WithValue(ctx, parentEventIDKey{}, parentEventID)
}

// ParentID returns the parent event_id stored on ctx, or "" when none.
func ParentID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v := ctx.Value(parentEventIDKey{})
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// ----- internal helper shared by every Emit<Type> -----

// send marshals payload, stamps the canonical fields from ctx, and
// hands the resulting Event to sink.Emit. A nil sink is a silent no-op,
// so callers don't need to nil-check at every emission site.
//
// durationMS is split out from payload because the row-level column is
// what analytics queries index — payloads that carry a latency_ms field
// of their own pass the same value here so the column stays in sync.
func send(ctx context.Context, sink Sink, eventType string, payload any, durationMS int64) {
	if sink == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("session event emit: payload marshal failed",
			"event_type", eventType,
			"error", err,
		)
		return
	}
	sink.Emit(ctx, Event{
		SessionID:  actor.SessionID(ctx),
		EventType:  eventType,
		ParentID:   ParentID(ctx),
		DurationMS: durationMS,
		Payload:    raw,
	})
}

// sha256Hex returns the lowercase-hex SHA256 of s. Used by helpers whose
// payload declares a *_sha256 field — keeps the digest computation out
// of caller code so it cannot drift.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
