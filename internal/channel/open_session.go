package channel

import (
	"context"
	"errors"
	"maps"
	"slices"
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
// create-or-find, the first-turn claim, the note reservation, its take-over by
// a recovery and its release (see state.SessionStore.OpenSession), and the
// read of what a note's target is authorised by. These are the open path's store calls before its turn
// runs or its note is stored, and each takes the request's context (see
// storeContext). Both session stores implement it.
type SessionOpener interface {
	Identity(ctx context.Context, id string) (*state.Session, error)
	OpenSession(ctx context.Context, p state.SessionParams) (*state.Session, bool, error)
	ClaimOpener(ctx context.Context, sessionID string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	ReserveNote(ctx context.Context, sessionID, noteKey string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	ReclaimNote(ctx context.Context, sessionID, noteKey string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	FreeNote(ctx context.Context, sessionID, noteKey string, reservedAt time.Time) error
}

// A server-opened turn is claimed before it runs (ClaimOpener, ReserveNote),
// and the claim is marked stored when the runner stores the turn's message
// under it (state.TurnClaim). A claim older than openerStaleAfter without that
// mark counts as abandoned and may be taken again; the store then refuses the
// message of the run that lost the claim (state.ErrClaimLost), so of two runs
// of one turn only one stores it and goes on. The turn runs under
// openerTimeout and openerStaleAfter exceeds it, so a live claimant normally
// stores its message or gives up before its claim goes stale. A note without
// a reply waits for its place among the session's turns until openerTimeout
// after its claim, its deadline, for the same reason. Variables so tests can
// shorten them; NewMessageHandler reads them once.
var (
	openerTimeout    = 3 * time.Minute
	openerStaleAfter = openerTimeout + time.Minute
	// recoveryLeeway delays a scheduled recovery past the claim's expiry: the
	// DB-backed store keeps claim times at second precision.
	recoveryLeeway = time.Second
	// notePoll is how often a note without a reply whose key another attempt
	// holds looks again whether that attempt stored it (see claimAndStore).
	notePoll = 250 * time.Millisecond
)

// openSessions serves the handler's open-session path.
type openSessions struct {
	cfg        HandlerConfig
	timeout    time.Duration
	staleAfter time.Duration
	leeway     time.Duration
	poll       time.Duration

	mu         sync.Mutex
	recovering map[recoveryKey]bool // the claims a recovery is scheduled for in this process
}

// recoveryKey names a claim: a session's first turn (noteKey empty) or one of
// its notes.
type recoveryKey struct{ sessionID, noteKey string }

// openScope is what an open-session request may reach: the sessions of its
// verified entity on the channel it arrived on (see handle, reaches and
// takesNote).
type openScope struct{ entityID, channel string }

// reaches reports whether sess is in the scope: the open path opened it (it
// has an open key), the store records it for the scope's entity and channel,
// and its key names a conversation of theirs. The key proves neither the
// opening nor the owner: an ordinary session's key has the same form, also
// one keyed without a profile ("<channel>:<conversation>:<thread>"), and the
// store may have backfilled such a session's owner and channel.
func (s openScope) reaches(sess *state.Session) bool {
	_, ok := s.conversation(sess.ID)
	return ok && sess.OpenKey != "" && sess.EntityID == s.entityID && sess.ChannelID == s.channel
}

// takesNote reports whether sess takes a note of the scope under groupID, the
// account of the request's verified profile: its key names a conversation of
// the scope, and it is the user's under that account on the scope's channel. A
// session the open path opened is that by what the open path recorded when it
// opened it (reaches, and the account it was opened for); an ordinary session
// by its verified-owner mark, which only the user's own chat turns set (see
// NewMessageHandler), as neither its key nor its recorded owner proves it.
func (s openScope) takesNote(sess *state.Session, groupID string) bool {
	if sess.OpenKey != "" {
		return s.reaches(sess) && sess.GroupID == groupID
	}
	_, ok := s.conversation(sess.ID)
	return ok && sess.VerifiedOwner != nil &&
		*sess.VerifiedOwner == state.VerifiedOwner{EntityID: s.entityID, GroupID: groupID, ChannelID: s.channel}
}

// conversation returns the conversation of sessionID, a session in the scope:
// the handler keys a conversation's session "<entity>:<channel>:<conversation>",
// and the open path takes conversations that are key parts only (see handle).
func (s openScope) conversation(sessionID string) (string, bool) {
	conv, ok := strings.CutPrefix(sessionID, s.entityID+":"+s.channel+":")
	return conv, ok && keyPart(conv)
}

// keyPart reports whether id can be one part of a session key the open path
// reads back: not empty, and without the colon the parts are joined with.
func keyPart(id string) bool {
	return id != "" && !strings.Contains(id, ":")
}

func newOpenSessions(cfg HandlerConfig) *openSessions {
	return &openSessions{cfg: cfg, timeout: openerTimeout, staleAfter: openerStaleAfter, leeway: recoveryLeeway,
		poll: notePoll, recovering: make(map[recoveryKey]bool)}
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
	// A session is addressed by its key, "<entity>:<channel>:<conversation>",
	// which names one user, channel and conversation only while each of the
	// three is a key part and no thread follows: otherwise another user's
	// session may have the same key ("u1:ws:a:c1" is u1's conversation "a:c1"
	// on channel ws, and u1:ws's conversation "c1" on channel a), and a turn
	// authorised for one would run in the other. So the request's user,
	// channel and conversation ids, and a note's target, must be such parts
	// and such a key, and neither a channel with threads nor a request in a
	// thread can open sessions.
	for _, part := range []struct{ field, id string }{
		{"entity id", entityID}, {"channel id", msg.ChannelID}, {"conversation id", msg.ConversationID},
	} {
		if !keyPart(part.id) {
			return openRejected(msg, "The "+part.field+" of an open-session request must not be empty or hold a colon.", "bad_request")
		}
	}
	target := msg.Metadata[pkg.OpenSessionIDMetadataKey]
	if parts := strings.Split(target, ":"); target != "" && (len(parts) != 3 || slices.Contains(parts, "")) {
		return openRejected(msg, "The session id of a note must read <entity>:<channel>:<conversation>, each part not empty and without a colon.", "bad_request")
	}
	// A request reaches sessions on the channel it arrived on only, which its
	// own key names and its channel id must name alike: a session it opens
	// records that id.
	channel, ok := strings.CutSuffix(strings.TrimPrefix(sessionKey, entityID+":"), ":"+msg.ConversationID)
	if !ok || channel != msg.ChannelID || msg.ThreadID != "" || pkg.CapabilitiesFromContext(ctx).Threads {
		return openRejected(msg, "This channel cannot open a session.", "bad_request")
	}
	scope := openScope{entityID: entityID, channel: channel}
	ctx = actor.WithVisibility(ctx, provider.VisibilityHidden)
	if target != "" {
		return o.continueSession(ctx, msg, target, scope, groupID)
	}
	return o.open(ctx, sessionKey, msg, scope, groupID)
}

// unavailable answers a request for a session it may not reach, like one for a
// session that is gone.
const unavailable = "This conversation is not available."

// open creates or finds the user's session for the origin and key and claims
// its first turn (see claimAndRun).
func (o *openSessions) open(ctx context.Context, sessionKey string, msg pkg.InboundMessage, scope openScope, groupID string) pkg.OutboundMessage {
	openedBy, openKey := msg.Metadata[pkg.OpenedByMetadataKey], msg.Metadata[pkg.OpenKeyMetadataKey]
	if openedBy == "" || openKey == "" {
		return openRejected(msg, "An open-session request needs an origin and a key.", "bad_request")
	}
	if _, set := msg.Metadata[pkg.NoteReplyMetadataKey]; set {
		return openRejected(msg, "Only a note can be sent without a reply.", "bad_request")
	}
	openCtx, cancel := o.storeContext(ctx)
	sess, _, err := o.cfg.Opener.OpenSession(openCtx, state.SessionParams{
		ID: sessionKey, EntityID: scope.entityID, GroupID: groupID,
		OpenedBy: openedBy, OpenKey: openKey, ChannelID: scope.channel,
	})
	cancel()
	if err != nil {
		logger.FromContext(ctx).Error("open session failed", "session", sessionKey, "opened_by", openedBy, "error", err)
		return openRejected(msg, "Something went wrong opening the session. Please try again.", "internal_error")
	}
	if !scope.reaches(sess) {
		// The user's session for this origin and key is on another channel.
		return openRejected(msg, unavailable, "not_found")
	}
	return o.claimAndRun(ctx, msg, scope, groupID, recoveryKey{sessionID: sess.ID})
}

// continueSession posts the content as a hidden note into target, a session of
// the user under groupID, the verified profile's account, on the request's
// channel (see takesNote), once per note key: as a hidden turn (see
// claimAndRun), or without a reply (see claimAndStore).
func (o *openSessions) continueSession(ctx context.Context, msg pkg.InboundMessage, target string, scope openScope, groupID string) pkg.OutboundMessage {
	noteKey := msg.Metadata[pkg.NoteKeyMetadataKey]
	if noteKey == "" {
		return openRejected(msg, "A note needs a note key.", "bad_request")
	}
	reply, ok := noteReply(msg)
	if !ok {
		return openRejected(msg, "A note's reply switch must be true or false.", "bad_request")
	}
	reached, err := o.noteReaches(ctx, target, scope, groupID)
	if err != nil {
		logger.FromContext(ctx).Error("note session lookup failed", "session", target, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	if !reached {
		// Gone, not proven the user's, or another user's, account's or
		// channel's: alike to the caller.
		return openRejected(msg, unavailable, "not_found")
	}
	key := recoveryKey{sessionID: target, noteKey: noteKey}
	if !reply {
		return o.claimAndStore(ctx, msg, scope, groupID, key)
	}
	return o.claimAndRun(ctx, msg, scope, groupID, key)
}

// noteReply reads a note's reply switch (pkg.NoteReplyMetadataKey): true
// without the key; ok is false for a value other than "true" and "false".
func noteReply(msg pkg.InboundMessage) (reply, ok bool) {
	switch value, set := msg.Metadata[pkg.NoteReplyMetadataKey]; {
	case !set, value == "true":
		return true, true
	case value == "false":
		return false, true
	default:
		return false, false
	}
}

// noteReaches reads target and reports whether a note of scope under groupID
// reaches it (see openScope.takesNote); a session that is gone is out of reach.
func (o *openSessions) noteReaches(ctx context.Context, target string, scope openScope, groupID string) (bool, error) {
	lookupCtx, cancel := o.storeContext(ctx)
	sess, err := o.cfg.Opener.Identity(lookupCtx, target)
	cancel()
	if errors.Is(err, state.ErrSessionNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return scope.takesNote(sess, groupID), nil
}

// claimAndRun takes key's claim, the first turn's or the note's, and answers
// the attempt, before any model run, with created=true exactly when it won the
// claim: this attempt then runs the turn. When another run holds the claim,
// the turn is recovered once that claim is stale, should its claimant have
// died before it stored the turn's input; a settled turn needs nothing. A
// claim the store cannot answer refuses the attempt, which the caller may
// retry with the same key, and a note's session that is gone meanwhile
// answers not_found.
func (o *openSessions) claimAndRun(ctx context.Context, msg pkg.InboundMessage, scope openScope, groupID string, key recoveryKey) pkg.OutboundMessage {
	now := time.Now()
	result, err := o.claim(ctx, key, now)
	if err != nil {
		logger.FromContext(ctx).Error("open session claim failed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
	}
	if result == state.ClaimSessionNotFound {
		return openRejected(msg, unavailable, "not_found")
	}
	o.acknowledge(ctx, msg, scope, key.sessionID, result == state.ClaimWon)
	switch result {
	case state.ClaimWon:
		o.run(ctx, key.sessionID, msg, scope, state.TurnClaim{NoteKey: key.noteKey, At: now})
	case state.ClaimHeld:
		o.scheduleRecovery(ctx, key, msg, scope, groupID)
	}
	return pkg.OutboundMessage{}
}

// claimAndStore takes key's note reservation for msg, a note without a reply,
// and answers the attempt only once the note is stored: created=true when this
// attempt stored it (see storeNote), created=false when another had. While
// another attempt holds the key, this one looks again every poll and takes the
// key should that attempt give it up or its reservation go stale; the note's
// recovery is scheduled as for a held note with a reply. A key taken after
// such a wait is stored under only while the note still reaches its session
// (see stillReaches): the user's chat turns may have marked the session for
// another account meanwhile. Waiting for the key and for the note's place
// among the session's turns ends at the note's deadline, the timeout after the
// first claim: an attempt whose note is not stored by then gets no answer, so
// the caller's wait for one ends as for any unanswered attempt, and a key it
// took is freed (see storeNote). A claim the store cannot answer refuses the
// attempt, and a session that is gone meanwhile answers not_found.
func (o *openSessions) claimAndStore(ctx context.Context, msg pkg.InboundMessage, scope openScope, groupID string, key recoveryKey) pkg.OutboundMessage {
	start := time.Now()
	deadline := start.Add(o.timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for now, held := start, false; ; now = time.Now() {
		result, err := o.claim(waitCtx, key, now)
		if err != nil {
			if waitCtx.Err() != nil {
				return pkg.OutboundMessage{}
			}
			logger.FromContext(ctx).Error("open session claim failed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
			return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error")
		}
		switch result {
		case state.ClaimWon:
			claim := state.TurnClaim{NoteKey: key.noteKey, At: now}
			if held {
				if refused, ok := o.stillReaches(ctx, msg, scope, groupID, key, claim); !ok {
					return refused
				}
			}
			err := o.storeNote(ctx, key, msg.Content, claim, deadline)
			return o.answerStored(ctx, msg, scope, groupID, key, err)
		case state.ClaimSettled:
			o.acknowledge(ctx, msg, scope, key.sessionID, false)
			return pkg.OutboundMessage{}
		case state.ClaimSessionNotFound:
			return openRejected(msg, unavailable, "not_found")
		}
		if !held {
			held = true
			o.scheduleRecovery(ctx, key, msg, scope, groupID)
		}
		select {
		case <-waitCtx.Done():
			logger.FromContext(ctx).Info("note not stored by its deadline: another attempt holds its key",
				"session", key.sessionID, "note_key", key.noteKey)
			return pkg.OutboundMessage{}
		case <-time.After(o.poll):
		}
	}
}

// stillReaches reports whether the note msg, whose attempt took key's
// reservation under claim after waiting for it, still reaches its session
// (see noteReaches). When it does not, or the lookup fails, it frees the
// reservation and returns the refusal to answer: not_found, or internal_error.
func (o *openSessions) stillReaches(ctx context.Context, msg pkg.InboundMessage, scope openScope, groupID string, key recoveryKey, claim state.TurnClaim) (pkg.OutboundMessage, bool) {
	reached, err := o.noteReaches(ctx, key.sessionID, scope, groupID)
	if err == nil && reached {
		return pkg.OutboundMessage{}, true
	}
	o.freeNote(ctx, key, claim.At)
	if err != nil {
		logger.FromContext(ctx).Error("note session lookup failed", "session", key.sessionID, "error", err)
		return openRejected(msg, "Something went wrong loading the conversation. Please try again.", "internal_error"), false
	}
	return openRejected(msg, unavailable, "not_found"), false
}

// storeNote stores content as a note without a reply in key's session under
// claim, waiting for its place among the session's turns until deadline (see
// HandlerConfig.StoreNote). A note it did not store frees the claim's
// reservation (see freeNote), so the same attempt again takes the key at once.
func (o *openSessions) storeNote(ctx context.Context, key recoveryKey, content string, claim state.TurnClaim, deadline time.Time) error {
	noteCtx, cancel := context.WithDeadline(ctx, deadline)
	err := o.cfg.StoreNote(noteCtx, key.sessionID, content, claim)
	cancel()
	if err != nil {
		o.freeNote(ctx, key, claim.At)
	}
	return err
}

// freeNote frees key's reservation taken at reservedAt, whose note this
// attempt did not store. The store frees nothing once a note is stored under
// the reservation, also when the error came after the write committed, nor a
// reservation another attempt took over; a release the store cannot make
// leaves the reservation to go stale.
func (o *openSessions) freeNote(ctx context.Context, key recoveryKey, reservedAt time.Time) {
	freeCtx, cancel := o.storeContext(ctx)
	defer cancel()
	if err := o.cfg.Opener.FreeNote(freeCtx, key.sessionID, key.noteKey, reservedAt); err != nil {
		logger.FromContext(ctx).Warn("note reservation not freed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
	}
}

// answerStored answers the attempt that took key's reservation by err, what
// storeNote returned: created=true once the note is stored. A note past its
// deadline, and one whose reservation another attempt took over, which then
// stores it, get no answer; a session that is gone or out of reach answers
// not_found, any other failure internal_error.
func (o *openSessions) answerStored(ctx context.Context, msg pkg.InboundMessage, scope openScope, groupID string, key recoveryKey, err error) pkg.OutboundMessage {
	log := logger.FromContext(ctx)
	if err == nil {
		o.acknowledge(ctx, msg, scope, key.sessionID, true)
		return pkg.OutboundMessage{}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		log.Info("note not stored: its wait for the session's turn ended", "session", key.sessionID, "note_key", key.noteKey)
		return pkg.OutboundMessage{}
	}
	if reached, lerr := o.noteReaches(ctx, key.sessionID, scope, groupID); lerr == nil && !reached {
		return openRejected(msg, unavailable, "not_found")
	}
	if errors.Is(err, state.ErrClaimLost) {
		log.Info("note taken over by another attempt", "session", key.sessionID, "note_key", key.noteKey)
		return pkg.OutboundMessage{}
	}
	log.Error("note not stored", "session", key.sessionID, "note_key", key.noteKey, "error", err)
	return openRejected(msg, "Something went wrong storing the note. Please try again.", "internal_error")
}

// acknowledge answers the attempt with its session, through the inbound
// message's own conversation: before any turn runs, and for a note without a
// reply once it is stored.
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
	// error frame is the channel's ordinary one (see errorFrame), and none of
	// its frames carries the request's keys.
	turn := msg
	turn.Metadata = maps.Clone(msg.Metadata)
	for _, key := range []string{pkg.OpenSessionMetadataKey, pkg.OpenedByMetadataKey, pkg.OpenKeyMetadataKey,
		pkg.OpenSessionIDMetadataKey, pkg.NoteKeyMetadataKey, pkg.NoteReplyMetadataKey} {
		delete(turn.Metadata, key)
	}
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
// is stale, and runs the turn with msg when that claim is won, or stores it
// when it is a note without a reply: its claimant died before the turn stored
// its input. A note is recovered only while it still reaches its session
// under groupID: the user's chat turns may have marked the session for another
// account since. One recovery per claim is scheduled in this process, however
// many requests find it held; it looks again while the claim is held by yet
// another run or the store fails to answer, and ends when the claim is
// settled, the note's reservation was freed (see reclaim), the note no longer
// reaches its session, or the process's context ends.
// The timer lives in this process; a restart before it fires leaves the
// recovery to the next identical request.
func (o *openSessions) scheduleRecovery(ctx context.Context, key recoveryKey, msg pkg.InboundMessage, scope openScope, groupID string) {
	o.mu.Lock()
	scheduled := o.recovering[key]
	o.recovering[key] = true
	o.mu.Unlock()
	if !scheduled {
		o.recoverWhenStale(ctx, key, msg, scope, groupID)
	}
}

func (o *openSessions) recoverWhenStale(ctx context.Context, key recoveryKey, msg pkg.InboundMessage, scope openScope, groupID string) {
	time.AfterFunc(o.staleAfter+o.leeway, func() {
		if ctx.Err() != nil {
			o.forget(key)
			return
		}
		log := logger.FromContext(ctx)
		if key.noteKey != "" {
			switch reached, err := o.noteReaches(ctx, key.sessionID, scope, groupID); {
			case err != nil:
				log.Warn("recovery lookup failed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
				o.recoverWhenStale(ctx, key, msg, scope, groupID)
				return
			case !reached:
				o.forget(key)
				log.Info("recovery dropped: the note no longer reaches its session", "session", key.sessionID, "note_key", key.noteKey)
				return
			}
		}
		now := time.Now()
		result, err := o.reclaim(ctx, key, now)
		switch {
		case err != nil || result == state.ClaimHeld:
			// The store could not say, or yet another run took the claim
			// meanwhile: look again later. The caller was told the turn is
			// there, so a failed lookup must not end the recovery.
			if err != nil {
				log.Warn("recovery claim failed", "session", key.sessionID, "note_key", key.noteKey, "error", err)
			}
			o.recoverWhenStale(ctx, key, msg, scope, groupID)
		case result == state.ClaimWon:
			o.forget(key)
			log.Info("recovering an interrupted turn", "session", key.sessionID, "note_key", key.noteKey)
			claim := state.TurnClaim{NoteKey: key.noteKey, At: now}
			if reply, _ := noteReply(msg); reply {
				o.run(ctx, key.sessionID, msg, scope, claim)
			} else if err := o.storeNote(ctx, key, msg.Content, claim, now.Add(o.timeout)); err != nil {
				log.Warn("recovered note not stored", "session", key.sessionID, "note_key", key.noteKey, "error", err)
			}
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
func (o *openSessions) claim(ctx context.Context, key recoveryKey, now time.Time) (state.ClaimResult, error) {
	ctx, cancel := o.storeContext(ctx)
	defer cancel()
	if key.noteKey == "" {
		return o.cfg.Opener.ClaimOpener(ctx, key.sessionID, now, o.staleAfter)
	}
	return o.cfg.Opener.ReserveNote(ctx, key.sessionID, key.noteKey, now, o.staleAfter)
}

// reclaim takes key's claim at now for its recovery: the first turn's as claim
// does, and a note's only by taking over the reservation there is
// (ReclaimNote). A note's attempt that ended frees its reservation, and the
// key is then its caller's for the next attempt: a recovery that reserved it
// anew would store a note nobody sent again.
func (o *openSessions) reclaim(ctx context.Context, key recoveryKey, now time.Time) (state.ClaimResult, error) {
	if key.noteKey == "" {
		return o.claim(ctx, key, now)
	}
	ctx, cancel := o.storeContext(ctx)
	defer cancel()
	return o.cfg.Opener.ReclaimNote(ctx, key.sessionID, key.noteKey, now, o.staleAfter)
}

// storeContext bounds the open path's store calls outside its turn: the open,
// a note's lookup, the claims and a note's release, which may wait for another
// transaction, a table lock or a free connection. Each waits no longer than
// the turn it is for may run, and not past ctx, so neither another
// transaction nor a shutdown keeps it and its database connection. The turn's
// own store calls (Runner.Run) take no context, as every turn's, nor does the
// write of a note without a reply (HandlerConfig.StoreNote).
func (o *openSessions) storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, o.timeout)
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
