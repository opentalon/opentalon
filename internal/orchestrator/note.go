package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// StoreNote stores content as a hidden user message of sessionID under claim,
// the reservation of the note's key (ReserveNote), and runs no turn. It takes
// the session's turn lock as a turn does, so a turn or a summarisation in
// flight stores its rows first, the note never lands between a tool call and
// its result, and every turn that starts after it reads the note as model
// input. A note whose ctx ended while it waited for the lock stores nothing:
// ctx bounds the wait, not the write. The write marks the reservation stored
// in the same step, or stores nothing and answers state.ErrClaimLost when
// another call took the reservation over; store errors come back as they are.
//
// Storing emits note_stored and nothing a turn emits: no model call, no usage,
// no summarisation or title run, and a pending confirmation and the session's
// latest-user-message id stay as they are.
func (o *Orchestrator) StoreNote(ctx context.Context, sessionID, content string, claim state.TurnClaim) error {
	unlock, err := o.lockSessionTurn(ctx, sessionID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.sessions.AddMessageWithMetadata(sessionID, provider.Message{
		Role:       provider.RoleUser,
		Content:    content,
		Visibility: provider.VisibilityHidden,
	}, claim.Metadata()); err != nil {
		return err
	}
	emit.EmitNoteStored(actor.WithSessionID(ctx, sessionID), o.eventSink, claim.NoteKey, content)
	return nil
}
