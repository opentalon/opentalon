package orchestrator

import (
	"context"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// StoreNote takes the session's turn lock, so a note never lands between a tool call and its result; ctx bounds the wait, not the write.
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
