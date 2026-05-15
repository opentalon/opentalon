package emit

import (
	"context"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// EmitError writes one generic error event. Prefer a typed variant
// (EmitLLMError, EmitToolCallArgsInvalid, …) when one exists for the
// failure mode in question; reach for EmitError only when no typed
// variant fits.
//
// where is a short, stable, grep-friendly location tag
// ("orchestrator.turn", "planner.dispatch", …); message is the
// sanitized error text.
func EmitError(ctx context.Context, sink Sink, where, message string) string {
	return send(ctx, sink, events.TypeError, events.ErrorPayload{
		Header:  events.Header{V: events.ErrorVersion},
		Where:   where,
		Message: events.SanitizeUTF8(message),
	}, 0)
}
