package logger

import (
	"context"
	"log/slog"
)

// sessionDebugKey carries a per-session flag in ctx that, when set, promotes
// any Debug-level log on that ctx through the slog handler regardless of the
// global level. The promoted record is also tagged so `jq` can split out
// "regular debug" (when LOG_LEVEL=debug) from "this session asked for it
// explicitly via /debug".
type sessionDebugKey struct{}

// WithSessionDebug returns a derived ctx with the session-debug flag set.
// All log calls using this ctx route through the sessionDebugHandler that
// the global slog.Default carries (installed by Setup), so Debug events
// emitted via slog.DebugContext become Info-level for this ctx only.
func WithSessionDebug(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionDebugKey{}, true)
}

// IsSessionDebug returns true when the session-debug flag is present in ctx.
// Used by the OpenAI provider to decide whether to capture raw HTTP bodies
// and to fan them into the persistent ai_debug_events table.
func IsSessionDebug(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(sessionDebugKey{}).(bool)
	return v
}

// sessionDebugHandler wraps a base slog.Handler so Debug-level records on a
// session-debug ctx are emitted at Info-level (and tagged session_debug=true)
// while every other record is delegated unchanged to the base handler. The
// design keeps two properties:
//
//  1. Sessions without /debug see *no* additional verbosity — the global
//     LOG_LEVEL still gates everything else.
//  2. Sessions with /debug see the full LLM traffic on stderr at Info,
//     so `kubectl logs -f` (which defaults to Info-or-above) tails the
//     captured bodies live without needing to flip global LOG_LEVEL.
//
// We deliberately do not implement WithGroup/WithAttrs as a passthrough +
// re-wrap pair: slog calls those before passing the handler down, so the
// base handler must receive them so attrs survive. The wrapper around it
// only needs to inspect ctx + level on Handle().
type sessionDebugHandler struct {
	base slog.Handler
}

// NewSessionDebugHandler wraps base. Only meaningful when base.Enabled is
// gated at Info or above; below that, Debug records flow through anyway and
// the handler is a no-op.
func NewSessionDebugHandler(base slog.Handler) slog.Handler {
	return &sessionDebugHandler{base: base}
}

func (h *sessionDebugHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	if h.base.Enabled(ctx, lvl) {
		return true
	}
	if lvl == slog.LevelDebug && IsSessionDebug(ctx) {
		// Re-evaluate at Info — this is the level the record will be
		// rewritten to in Handle().
		return h.base.Enabled(ctx, slog.LevelInfo)
	}
	return false
}

func (h *sessionDebugHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelDebug && IsSessionDebug(ctx) {
		// Promote to Info and mark the record so log filters can pick out
		// session-debug traffic specifically.
		promoted := slog.NewRecord(r.Time, slog.LevelInfo, r.Message, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			promoted.AddAttrs(a)
			return true
		})
		promoted.AddAttrs(slog.Bool("session_debug", true))
		return h.base.Handle(ctx, promoted)
	}
	return h.base.Handle(ctx, r)
}

func (h *sessionDebugHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sessionDebugHandler{base: h.base.WithAttrs(attrs)}
}

func (h *sessionDebugHandler) WithGroup(name string) slog.Handler {
	return &sessionDebugHandler{base: h.base.WithGroup(name)}
}
