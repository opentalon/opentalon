package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
)

// lastUserMessageIDMetaKey is the session-metadata key that holds the id
// handed to plugins as contextargs.LastUserMessageID. Session metadata is
// the right home for it: it survives restarts, every instance reads the same
// row, and summarisation (SetSummary) and clear_session (ClearMessages)
// rewrite only the messages. Message rows cannot carry it — they have no
// stable id, and SetSummary renumbers them and drops their metadata.
const lastUserMessageIDMetaKey = "last_user_message_id"

// withStoredLastUserMessageID puts the session's stored latest-user-message
// id on ctx, creating and saving one first when the session has none yet
// (its first turn, or a session from before the key existed). Whatever ctx
// carried before is replaced.
//
// It has to come from the session, not from the turn: a tool call the user
// approves runs on the turn that carries the approval, so reading the stored
// value here is what makes that call carry the id of the message that led to
// the proposal.
//
// An id whose save reported an error is never trusted as is; see
// reconcileLastUserMessageID.
func (o *Orchestrator) withStoredLastUserMessageID(ctx context.Context, sessions SessionStoreInterface, sess *state.Session, sessionID string) context.Context {
	if sess != nil {
		if id := sess.Metadata[lastUserMessageIDMetaKey]; id != "" {
			return actor.WithLastUserMessageID(ctx, id)
		}
	}
	id, err := newLastUserMessageID()
	if err != nil {
		logger.FromContext(ctx).Warn("could not create the session's latest-user-message id; plugins get none this turn",
			"session", sessionID, "error", err)
		return actor.WithLastUserMessageID(ctx, "")
	}
	if err := sessions.SetMetadata(sessionID, lastUserMessageIDMetaKey, id); err != nil {
		logger.FromContext(ctx).Warn("could not save the session's first latest-user-message id",
			"session", sessionID, "error", err)
		return o.reconcileLastUserMessageID(ctx, sessionID)
	}
	return actor.WithLastUserMessageID(ctx, id)
}

// rotateLastUserMessageID gives the session a new latest-user-message id and
// puts it on ctx. Run calls it only for a message the user wrote while
// nothing was waiting for their approval. The new id is the turn's
// user_message event id when one was emitted (so the value joins the audit
// log), else a random one.
//
// When the save reports an error the turn keeps what is stored — normally
// the previous id, so a plugin that waits for the user to write again keeps
// waiting; see reconcileLastUserMessageID.
func (o *Orchestrator) rotateLastUserMessageID(ctx context.Context, sessions SessionStoreInterface, sessionID, userMessageEventID string) context.Context {
	id := userMessageEventID
	if id == "" {
		var err error
		if id, err = newLastUserMessageID(); err != nil {
			// Nothing was written, so ctx still holds the stored value.
			logger.FromContext(ctx).Warn("could not create a new latest-user-message id; keeping the previous one",
				"session", sessionID, "error", err)
			return ctx
		}
	}
	if err := sessions.SetMetadata(sessionID, lastUserMessageIDMetaKey, id); err != nil {
		logger.FromContext(ctx).Warn("could not save the new latest-user-message id; using what is stored",
			"session", sessionID, "error", err)
		return o.reconcileLastUserMessageID(ctx, sessionID)
	}
	return actor.WithLastUserMessageID(ctx, id)
}

// reconcileLastUserMessageID puts on ctx whatever id the session store holds
// after a save reported an error. An error does not prove the write was lost
// (a commit can apply and still fail to report success), and ctx must match
// what the next turn will read: a call of this turn carrying the old id while
// an approval turn reads the new one would look like a fresh user message to
// a plugin. The read bypasses the turn's cached copy, which a failed write
// leaves as it was. When the store cannot be read, the argument is withheld
// for the rest of the turn and a plugin that needs it fails closed.
func (o *Orchestrator) reconcileLastUserMessageID(ctx context.Context, sessionID string) context.Context {
	sess, err := o.sessions.Get(sessionID)
	if err != nil || sess == nil {
		logger.FromContext(ctx).Warn("could not read back the latest-user-message id; plugins get none this turn",
			"session", sessionID, "error", err)
		return actor.WithLastUserMessageID(ctx, "")
	}
	return actor.WithLastUserMessageID(ctx, sess.Metadata[lastUserMessageIDMetaKey])
}

// isSystemRun reports whether the turn was opened by a backend feature (a
// verified profile of kind "system") rather than by a person. Its message is
// not the user's writing, hidden or not.
func isSystemRun(ctx context.Context) bool {
	p := profile.FromContext(ctx)
	return p != nil && p.Kind == profile.KindSystem
}

// newLastUserMessageID returns a random 32-character hex id, the same shape
// as a session event id.
func newLastUserMessageID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
