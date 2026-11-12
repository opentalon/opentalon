package channel

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// SessionOpener is the store side of a server-opened session: the
// create-or-find, the first-turn claim and the note reservation (see
// state.SessionStore.OpenSession), and the lookup a note's target is
// authorised by. Both session stores implement it.
type SessionOpener interface {
	Get(id string) (*state.Session, error)
	OpenSession(p state.SessionParams) (*state.Session, bool, error)
	ClaimOpener(sessionID string, now time.Time, staleAfter time.Duration) (bool, error)
	ReserveNote(sessionID, noteKey string, now time.Time, staleAfter time.Duration) (bool, error)
}

// A server-opened turn is claimed before it runs (ClaimOpener, ReserveNote),
// and the claim is marked stored when the runner stores the turn's message
// under it (state.TurnClaim). A claim older than openerStaleAfter without that
// mark counts as abandoned and may be taken again; the store then refuses the
// message of the run that lost the claim (state.ErrClaimLost), so of two runs
// of one turn only one stores it and goes on. The turn runs under
// openerTimeout and openerStaleAfter exceeds it, so a live claimant normally
// stores its message or gives up before its claim goes stale. Variables so
// tests can shorten them; NewMessageHandler reads them once.
var (
	openerTimeout    = 3 * time.Minute
	openerStaleAfter = openerTimeout + time.Minute
	// recoveryLeeway delays a scheduled recovery past the claim's expiry: the
	// DB-backed store keeps claim times at second precision.
	recoveryLeeway = time.Second
)

// openSessions serves the handler's open-session path.
type openSessions struct {
	cfg        HandlerConfig
	timeout    time.Duration
	staleAfter time.Duration
	leeway     time.Duration
}

func newOpenSessions(cfg HandlerConfig) *openSessions {
	return &openSessions{cfg: cfg, timeout: openerTimeout, staleAfter: openerStaleAfter, leeway: recoveryLeeway}
}

// handle answers an open-session message. kind, entityID and groupID are the
// verified profile's; sessionKey is the inbound message's entity-scoped key,
// which a newly opened session takes. It returns a rejection for the registry
// to send, or the empty frame when it delivered the answer and the turn itself.
func (o *openSessions) handle(ctx context.Context, sessionKey string, msg pkg.InboundMessage, kind, entityID, groupID string) pkg.OutboundMessage {
	// Opening a session for a user with hidden content is a server privilege,
	// like a hidden turn: an ordinary chat token never gets past this.
	if kind != profile.KindSystem {
		logger.FromContext(ctx).Warn("open session refused: not a system invocation",
			"channel", msg.ChannelID, "entity_id", entityID)
		return openRejected(msg, "This credential cannot open a session.", "forbidden")
	}
	if msg.Metadata[pkg.OpenRequestIDMetadataKey] == "" || strings.TrimSpace(msg.Content) == "" {
		return openRejected(msg, "An open-session request needs a request id and content.", "bad_request")
	}
	ctx = actor.WithVisibility(ctx, provider.VisibilityHidden)
	if target := msg.Metadata[pkg.OpenSessionIDMetadataKey]; target != "" {
		return o.continueSession(ctx, sessionKey, msg, target, entityID)
	}
	return o.open(ctx, sessionKey, msg, entityID, groupID)
}

// open creates or finds the user's session for the origin and key, answers the
// caller, and runs the first turn when this attempt wins the session's claim.
func (o *openSessions) open(ctx context.Context, sessionKey string, msg pkg.InboundMessage, entityID, groupID string) pkg.OutboundMessage {
	openedBy, openKey := msg.Metadata[pkg.OpenedByMetadataKey], msg.Metadata[pkg.OpenKeyMetadataKey]
	if openedBy == "" || openKey == "" {
		return openRejected(msg, "An open-session request needs an origin and a key.", "bad_request")
	}
	sess, created, err := o.cfg.Opener.OpenSession(state.SessionParams{
		ID: sessionKey, EntityID: entityID, GroupID: groupID,
		OpenedBy: openedBy, OpenKey: openKey, ChannelID: msg.ChannelID,
	})
	if err != nil {
		logger.FromContext(ctx).Error("open session failed", "session", sessionKey, "opened_by", openedBy, "error", err)
		return openRejected(msg, "Something went wrong opening the session. Please try again.", "internal_error")
	}
	o.acknowledge(ctx, sessionKey, msg, sess.ID, created)

	now := time.Now()
	won, err := o.cfg.Opener.ClaimOpener(sess.ID, now, o.staleAfter)
	if err != nil {
		logger.FromContext(ctx).Warn("opener claim failed", "session", sess.ID, "error", err)
	}
	if won {
		o.run(ctx, sess.ID, msg, entityID, state.TurnClaim{At: now})
		return pkg.OutboundMessage{}
	}
	// Another attempt holds the claim, or the first turn ran. A claimant that
	// died before its turn stored a message leaves a claim that goes stale, so
	// a recovery is scheduled for every lost claim; ClaimOpener refuses it for
	// a turn that ran (the transcript is no proof: clearing rewrites it).
	o.scheduleRecovery(ctx, sess.ID, msg, entityID)
	return pkg.OutboundMessage{}
}

// continueSession posts the content as a hidden note into the user's session
// target, stored and answered once per note key.
func (o *openSessions) continueSession(ctx context.Context, sessionKey string, msg pkg.InboundMessage, target, entityID string) pkg.OutboundMessage {
	noteKey := msg.Metadata[pkg.NoteKeyMetadataKey]
	if noteKey == "" {
		return openRejected(msg, "A note needs a note key.", "bad_request")
	}
	const gone = "This conversation is no longer available."
	// The target is the user's only if the store says so: the key's prefix
	// is no proof, since an entity id may itself contain a colon ("u1" is a
	// prefix of the keys of "u1:other"). Another user's session answers like
	// a missing one.
	sess, err := o.cfg.Opener.Get(target)
	if err != nil && !errors.Is(err, state.ErrSessionNotFound) {
		logger.FromContext(ctx).Error("note session lookup failed", "session", target, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	if err != nil || sess.EntityID != entityID {
		return openRejected(msg, gone, "not_found")
	}
	now := time.Now()
	reserved, err := o.cfg.Opener.ReserveNote(target, noteKey, now, o.staleAfter)
	if err != nil {
		logger.FromContext(ctx).Error("note reservation failed", "session", target, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	o.acknowledge(ctx, sessionKey, msg, target, reserved)
	if reserved {
		o.run(ctx, target, msg, entityID, state.TurnClaim{NoteKey: noteKey, At: now})
	}
	return pkg.OutboundMessage{}
}

// acknowledge answers the attempt with its session, through the inbound
// message's own conversation, before any turn runs.
func (o *openSessions) acknowledge(ctx context.Context, sessionKey string, msg pkg.InboundMessage, sessionID string, created bool) {
	ack := openFrame(msg, "", map[string]string{
		"type":                       pkg.FrameSessionOpened,
		pkg.OpenSessionIDMetadataKey: sessionID,
		"created":                    strconv.FormatBool(created),
	})
	if err := o.cfg.SendToSession(ctx, sessionKey, ack); err != nil {
		logger.FromContext(ctx).Warn("open session answer not delivered", "session", sessionID, "error", err)
	}
}

// run runs the content as a hidden turn of sessionID under claim and the turn
// timeout, and delivers the reply, or its error frame, to that session's
// conversation. The session need not be the inbound message's (a found or
// continued session, a recovery), so the reply goes whole through
// SendToSession and the registry's stream writer, bound to the inbound
// conversation, is dropped. A run whose claim another run took over delivers
// nothing: that run answers the turn.
func (o *openSessions) run(ctx context.Context, sessionID string, msg pkg.InboundMessage, entityID string, claim state.TurnClaim) {
	// The turn's frames belong to the conversation, not to the request: its
	// error frame is the channel's ordinary one (see errorFrame).
	turn := msg
	turn.Metadata = maps.Clone(msg.Metadata)
	delete(turn.Metadata, pkg.OpenSessionMetadataKey)
	turn.ConversationID, turn.ThreadID = "", ""
	if conv, thread, ok := sessionConversation(sessionID, entityID); ok {
		turn.ConversationID, turn.ThreadID = conv, thread
		ctx = actor.WithConversationID(ctx, conv)
	}
	runCtx, cancel := context.WithTimeout(orchestrator.WithTurnClaim(pkg.WithStreamWriter(ctx, nil), claim), o.timeout)
	reply, err := runTurn(runCtx, o.cfg, sessionID, msg.Content, turn)
	cancel()
	if errors.Is(err, state.ErrClaimLost) {
		logger.FromContext(ctx).Info("turn taken over by another run", "session", sessionID, "note_key", claim.NoteKey)
		return
	}
	if err := o.cfg.SendToSession(ctx, sessionID, reply); err != nil {
		logger.FromContext(ctx).Warn("open session reply not delivered", "session", sessionID, "error", err)
	}
}

// scheduleRecovery claims sessionID's first turn again once the lost claim is
// stale and runs the turn when that claim is won: its claimant died before the
// turn stored a message. The timer lives in this process; a restart before it
// fires leaves the recovery to the next identical request.
func (o *openSessions) scheduleRecovery(ctx context.Context, sessionID string, msg pkg.InboundMessage, entityID string) {
	time.AfterFunc(o.staleAfter+o.leeway, func() {
		if ctx.Err() != nil {
			return
		}
		now := time.Now()
		won, err := o.cfg.Opener.ClaimOpener(sessionID, now, o.staleAfter)
		if err != nil {
			logger.FromContext(ctx).Warn("opener recovery claim failed", "session", sessionID, "error", err)
			return
		}
		if won {
			logger.FromContext(ctx).Info("recovering an interrupted first turn", "session", sessionID)
			o.run(ctx, sessionID, msg, entityID, state.TurnClaim{At: now})
		}
	})
}

// openRejected refuses the open attempt msg with a typed error frame.
func openRejected(msg pkg.InboundMessage, text, errorCode string) pkg.OutboundMessage {
	return openFrame(msg, text, map[string]string{"type": pkg.FrameOpenRejected, "error_code": errorCode})
}

// openFrame is a frame answering the open attempt msg: the inbound metadata as
// on every frame, meta, and the attempt's correlation id, which safeMetadata
// keeps off every other frame.
func openFrame(msg pkg.InboundMessage, text string, meta map[string]string) pkg.OutboundMessage {
	if id := msg.Metadata[pkg.OpenRequestIDMetadataKey]; id != "" {
		meta[pkg.OpenRequestIDMetadataKey] = id
	}
	return errorResponse(msg, text, meta)
}

// sessionConversation returns the conversation and thread a session key of
// entityID addresses, in the form the handler builds:
// "<entity>:<channel>:<conversation>[:<thread>]". ok is false for a key of
// another entity or of another shape.
func sessionConversation(sessionID, entityID string) (conversation, thread string, ok bool) {
	rest, found := strings.CutPrefix(sessionID, entityID+":")
	if entityID == "" || !found {
		return "", "", false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	if len(parts) == 3 {
		thread = parts[2]
	}
	return parts[1], thread, true
}
