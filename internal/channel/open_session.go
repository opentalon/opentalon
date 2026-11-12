package channel

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
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
	ClaimOpener(sessionID string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	ReserveNote(sessionID, noteKey string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
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

	mu         sync.Mutex
	recovering map[recoveryKey]bool // the claims a recovery is scheduled for in this process
}

// recoveryKey names a claim: a session's first turn (noteKey empty) or one of
// its notes.
type recoveryKey struct{ sessionID, noteKey string }

// openScope is what an open-session request may reach: the sessions of its
// verified entity on the channel it arrived on (see handle).
type openScope struct{ entityID, channel string }

// reaches reports whether sess is in the scope: the store records it for the
// scope's entity and channel. Its key proves neither, since an entity id and a
// channel id may each hold a colon ("u1:ws:admin:c1" may be u1's on channel
// ws:admin). Only a session the open path opened records a channel.
func (s openScope) reaches(sess *state.Session) bool {
	_, ok := s.conversation(sess.ID)
	return ok && sess.EntityID == s.entityID && sess.ChannelID == s.channel
}

// conversation returns the conversation of sessionID, a session in the scope:
// the handler keys a conversation's session "<entity>:<channel>:<conversation>".
func (s openScope) conversation(sessionID string) (string, bool) {
	conv, ok := strings.CutPrefix(sessionID, s.entityID+":"+s.channel+":")
	return conv, ok && conv != ""
}

func newOpenSessions(cfg HandlerConfig) *openSessions {
	return &openSessions{cfg: cfg, timeout: openerTimeout, staleAfter: openerStaleAfter, leeway: recoveryLeeway,
		recovering: make(map[recoveryKey]bool)}
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
	// A request reaches sessions on the channel it arrived on only, which its
	// own key names and its channel id must name alike: a session it opens
	// records that id. A session is addressed by its key, and a key reads
	// "<entity>:<channel>:<conversation>" only where no thread follows the
	// conversation: a thread could not be told from the end of a conversation
	// id holding a colon, so neither a channel with threads nor a request in
	// a thread can open sessions.
	channel, ok := strings.CutSuffix(strings.TrimPrefix(sessionKey, entityID+":"), ":"+msg.ConversationID)
	if !ok || channel != msg.ChannelID || msg.ConversationID == "" || msg.ThreadID != "" ||
		pkg.CapabilitiesFromContext(ctx).Threads {
		return openRejected(msg, "This channel cannot open a session.", "bad_request")
	}
	scope := openScope{entityID: entityID, channel: channel}
	ctx = actor.WithVisibility(ctx, provider.VisibilityHidden)
	if target := msg.Metadata[pkg.OpenSessionIDMetadataKey]; target != "" {
		return o.continueSession(ctx, msg, target, scope)
	}
	return o.open(ctx, sessionKey, msg, scope, groupID)
}

// unavailable answers a request for a session it may not reach, like one for a
// session that is gone.
const unavailable = "This conversation is not available."

// open creates or finds the user's session for the origin and key, answers the
// caller, and runs the first turn when this attempt wins the session's claim.
func (o *openSessions) open(ctx context.Context, sessionKey string, msg pkg.InboundMessage, scope openScope, groupID string) pkg.OutboundMessage {
	openedBy, openKey := msg.Metadata[pkg.OpenedByMetadataKey], msg.Metadata[pkg.OpenKeyMetadataKey]
	if openedBy == "" || openKey == "" {
		return openRejected(msg, "An open-session request needs an origin and a key.", "bad_request")
	}
	sess, created, err := o.cfg.Opener.OpenSession(state.SessionParams{
		ID: sessionKey, EntityID: scope.entityID, GroupID: groupID,
		OpenedBy: openedBy, OpenKey: openKey, ChannelID: scope.channel,
	})
	if err != nil {
		logger.FromContext(ctx).Error("open session failed", "session", sessionKey, "opened_by", openedBy, "error", err)
		return openRejected(msg, "Something went wrong opening the session. Please try again.", "internal_error")
	}
	if !scope.reaches(sess) {
		// The user's session for this origin and key is on another channel.
		return openRejected(msg, unavailable, "not_found")
	}
	o.acknowledge(ctx, msg, scope, sess.ID, created)

	now := time.Now()
	result, err := o.cfg.Opener.ClaimOpener(sess.ID, now, o.staleAfter)
	if err != nil {
		logger.FromContext(ctx).Warn("opener claim failed", "session", sess.ID, "error", err)
	}
	switch {
	case err == nil && result == state.ClaimWon:
		o.run(ctx, sess.ID, msg, scope, state.TurnClaim{At: now})
	case err != nil || result == state.ClaimHeld:
		// Another attempt holds the claim (or the store could not say). A
		// claimant that dies before its turn stored a message leaves a claim
		// that goes stale, so the turn is recovered then. A first turn that ran
		// is settled and needs nothing.
		o.scheduleRecovery(ctx, recoveryKey{sessionID: sess.ID}, msg, scope)
	}
	return pkg.OutboundMessage{}
}

// continueSession posts the content as a hidden note into target, a session of
// the user on the request's channel, stored and answered once per note key.
func (o *openSessions) continueSession(ctx context.Context, msg pkg.InboundMessage, target string, scope openScope) pkg.OutboundMessage {
	noteKey := msg.Metadata[pkg.NoteKeyMetadataKey]
	if noteKey == "" {
		return openRejected(msg, "A note needs a note key.", "bad_request")
	}
	sess, err := o.cfg.Opener.Get(target)
	if err != nil && !errors.Is(err, state.ErrSessionNotFound) {
		logger.FromContext(ctx).Error("note session lookup failed", "session", target, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	if err != nil || !scope.reaches(sess) {
		// Gone, or another user's or channel's: alike to the caller.
		return openRejected(msg, unavailable, "not_found")
	}
	now := time.Now()
	result, err := o.cfg.Opener.ReserveNote(target, noteKey, now, o.staleAfter)
	if err != nil {
		logger.FromContext(ctx).Error("note reservation failed", "session", target, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	o.acknowledge(ctx, msg, scope, target, result == state.ClaimWon)
	switch result {
	case state.ClaimWon:
		o.run(ctx, target, msg, scope, state.TurnClaim{NoteKey: noteKey, At: now})
	case state.ClaimHeld:
		// As for the first turn: the attempt holding the reservation may have
		// died before it stored the note, which is then recovered once the
		// reservation is stale.
		o.scheduleRecovery(ctx, recoveryKey{sessionID: target, noteKey: noteKey}, msg, scope)
	}
	return pkg.OutboundMessage{}
}

// acknowledge answers the attempt with its session, through the inbound
// message's own conversation, before any turn runs.
func (o *openSessions) acknowledge(ctx context.Context, msg pkg.InboundMessage, scope openScope, sessionID string, created bool) {
	ack := openFrame(msg, "", map[string]string{
		"type":                       pkg.FrameSessionOpened,
		pkg.OpenSessionIDMetadataKey: sessionID,
		"created":                    strconv.FormatBool(created),
	})
	o.deliver(ctx, scope, ack, "open session answer not delivered")
}

// deliver sends frame to its conversation on the request's channel, logging
// what if it cannot be delivered.
func (o *openSessions) deliver(ctx context.Context, scope openScope, frame pkg.OutboundMessage, what string) {
	if err := o.cfg.SendToConversation(ctx, scope.channel, frame.ConversationID, frame); err != nil {
		logger.FromContext(ctx).Warn(what, "channel", scope.channel, "conversation", frame.ConversationID, "error", err)
	}
}

// run runs the content as a hidden turn of sessionID, a session in scope, under
// claim and the turn timeout, and delivers the reply, or its error frame, to
// that session's conversation. The session need not be the inbound message's
// (a found or continued session, a recovery), so the reply goes whole to the
// session's conversation and the registry's stream writer, bound to the
// inbound conversation, is dropped. A run whose claim another run took over
// delivers nothing: that run answers the turn.
func (o *openSessions) run(ctx context.Context, sessionID string, msg pkg.InboundMessage, scope openScope, claim state.TurnClaim) {
	// The turn's frames belong to the conversation, not to the request: its
	// error frame is the channel's ordinary one (see errorFrame).
	turn := msg
	turn.Metadata = maps.Clone(msg.Metadata)
	delete(turn.Metadata, pkg.OpenSessionMetadataKey)
	turn.ConversationID, _ = scope.conversation(sessionID)
	ctx = actor.WithConversationID(ctx, turn.ConversationID)
	runCtx, cancel := context.WithTimeout(orchestrator.WithTurnClaim(pkg.WithStreamWriter(ctx, nil), claim), o.timeout)
	reply, err := runTurn(runCtx, o.cfg, sessionID, msg.Content, turn)
	cancel()
	if errors.Is(err, state.ErrClaimLost) {
		logger.FromContext(ctx).Info("turn taken over by another run", "session", sessionID, "note_key", claim.NoteKey)
		return
	}
	o.deliver(ctx, scope, reply, "open session reply not delivered")
}

// scheduleRecovery takes the claim key again once the claim another run holds
// is stale, and runs the turn with msg when that claim is won: its claimant
// died before the turn stored its input. One recovery per claim is scheduled
// in this process, however many requests find it held; it looks again while
// the claim is held by yet another run or the store fails to answer, and ends
// when the claim is settled or the process's context ends.
// The timer lives in this process; a restart before it fires leaves the
// recovery to the next identical request.
func (o *openSessions) scheduleRecovery(ctx context.Context, key recoveryKey, msg pkg.InboundMessage, scope openScope) {
	o.mu.Lock()
	scheduled := o.recovering[key]
	o.recovering[key] = true
	o.mu.Unlock()
	if !scheduled {
		o.recoverWhenStale(ctx, key, msg, scope)
	}
}

func (o *openSessions) recoverWhenStale(ctx context.Context, key recoveryKey, msg pkg.InboundMessage, scope openScope) {
	time.AfterFunc(o.staleAfter+o.leeway, func() {
		if ctx.Err() != nil {
			o.forget(key)
			return
		}
		now := time.Now()
		result, err := o.claim(key, now)
		switch {
		case err != nil || result == state.ClaimHeld:
			// The store could not say, or yet another run took the claim
			// meanwhile: look again later. The caller was told the turn is
			// there, so a failed lookup must not end the recovery.
			if err != nil {
				logger.FromContext(ctx).Warn("recovery claim failed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
			}
			o.recoverWhenStale(ctx, key, msg, scope)
		case result == state.ClaimWon:
			o.forget(key)
			logger.FromContext(ctx).Info("recovering an interrupted turn", "session", key.sessionID, "note_key", key.noteKey)
			o.run(ctx, key.sessionID, msg, scope, state.TurnClaim{NoteKey: key.noteKey, At: now})
		default:
			o.forget(key)
		}
	})
}

// forget ends the scheduled recovery of key.
func (o *openSessions) forget(key recoveryKey) {
	o.mu.Lock()
	delete(o.recovering, key)
	o.mu.Unlock()
}

// claim takes key's claim at now: the first turn's or the note's.
func (o *openSessions) claim(key recoveryKey, now time.Time) (state.ClaimResult, error) {
	if key.noteKey == "" {
		return o.cfg.Opener.ClaimOpener(key.sessionID, now, o.staleAfter)
	}
	return o.cfg.Opener.ReserveNote(key.sessionID, key.noteKey, now, o.staleAfter)
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
