package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"gopkg.in/yaml.v3"
)

// ErrSessionNotFound is the canonical sentinel returned by both the in-memory
// and DB-backed SessionStore.Get when the requested id is absent. Callers must
// use errors.Is to distinguish a genuine miss from an infrastructure failure
// (e.g. dropped DB connection): only the former should surface to the user as
// "session expired"; the latter is a transient internal error and must not
// cause the client to discard a valid conversation_id.
var ErrSessionNotFound = errors.New("session not found")

type Session struct {
	ID          string             `yaml:"id"`
	Messages    []provider.Message `yaml:"messages"`
	Summary     string             `yaml:"summary,omitempty"` // compressed history when summarization is used
	Title       string             `yaml:"title,omitempty"`   // short LLM-generated label; empty until maybeGenerateTitle has run
	ActiveModel provider.ModelRef  `yaml:"active_model,omitempty"`
	Metadata    map[string]string  `yaml:"metadata,omitempty"`
	EntityID    string             `yaml:"entity_id,omitempty"` // the owner the session was created for (SessionParams.EntityID); empty without a profile
	OpenedBy    string             `yaml:"opened_by,omitempty"` // origin of a session a server opened (OpenSession); empty otherwise
	OpenKey     string             `yaml:"open_key,omitempty"`  // the opener's idempotency key; empty otherwise
	CreatedAt   time.Time          `yaml:"created_at"`
	UpdatedAt   time.Time          `yaml:"updated_at"`
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	opened   map[openIdentity]string // OpenSession's create-or-find index: identity -> session id
	claims   map[string]*claim       // ClaimOpener's claim per session id
	notes    map[noteIdentity]*claim // ReserveNote's reservation per session and note key
	dir      string
}

// claim is an opener claim or a note reservation: when it was taken, and
// whether the turn's input has been stored under it since. Like the
// DB-backed markers, stored survives ClearMessages and SetSummary.
type claim struct {
	at     time.Time
	stored bool
}

// won reports whether a claim may be taken at now: there is none yet, or it
// is older than staleAfter and its turn's input was never stored.
func (c *claim) won(now time.Time, staleAfter time.Duration) bool {
	return c == nil || (!c.stored && c.at.Before(now.Add(-staleAfter)))
}

// openIdentity is what OpenSession is idempotent on: the DB-backed store's
// unique index over (entity_id, opened_by, open_key).
type openIdentity struct{ entityID, openedBy, openKey string }

// noteIdentity is what ReserveNote is idempotent on.
type noteIdentity struct{ sessionID, noteKey string }

func NewSessionStore(dir string) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session),
		opened:   make(map[openIdentity]string),
		claims:   make(map[string]*claim),
		notes:    make(map[noteIdentity]*claim),
		dir:      dir,
	}
}

// SessionParams is what a session store's Create needs to mint a row. ID is
// the packed session key; EntityID and GroupID scope it to an actor. Kind is
// the session's interaction_kind ("chat" | "system"; empty is stored as
// "chat") and SystemSource the per-feature label of the feature that opened a
// system session (empty is stored as NULL). Named fields on purpose: two
// labels of the same type can no longer be swapped and still compile.
//
// OpenedBy, OpenKey and ChannelID are read by OpenSession only, which opens a
// chat session for a user on a server's behalf: OpenedBy is the caller's
// origin label, OpenKey its idempotency key, ChannelID the channel the session
// belongs to. Create ignores all three; Kind and SystemSource are ignored by
// OpenSession, which always stamps "chat" with no source.
type SessionParams struct {
	ID           string
	EntityID     string
	GroupID      string
	Kind         string
	SystemSource string
	OpenedBy     string
	OpenKey      string
	ChannelID    string
}

// ErrOpenParams is returned by OpenSession when the origin or the key is empty:
// without both there is nothing to be idempotent on.
var ErrOpenParams = errors.New("open session: origin and key are required")

// ClaimResult is what a claim call (ClaimOpener, ReserveNote) found.
type ClaimResult int

const (
	// ClaimHeld: another run holds a fresh claim, and the turn's input was
	// not stored under it yet. Should that run die, the claim goes stale and
	// may be taken again.
	ClaimHeld ClaimResult = iota
	// ClaimWon: the call took the claim; its turn may run.
	ClaimWon
	// ClaimSettled: the turn's input was stored under a claim, or there is no
	// such session: nothing is left to run.
	ClaimSettled
)

// TurnClaim names the claim a turn runs under: the first turn of an opened
// session (ClaimOpener; NoteKey empty) or a keyed note (ReserveNote). At is
// the now the claim was taken with; it tells this claim from a later one that
// took the same turn over once the first went stale.
type TurnClaim struct {
	NoteKey string
	At      time.Time
}

// The message metadata a claimed turn's input is stored with
// (TurnClaim.Metadata); AddMessageWithMetadata reads the claim from it.
const (
	NoteKeyMetadataKey   = "note_key"
	ClaimedAtMetadataKey = "claimed_at"
)

// ErrClaimLost is returned by AddMessageWithMetadata, which then stores
// nothing, when the claim a turn's input names is no longer the claim held:
// another run took the turn over after the claim went stale, or the input was
// stored already. Exactly one run stores a claimed turn's input.
var ErrClaimLost = errors.New("turn claim lost: another run took the turn over or stored its input")

// Metadata is the message metadata that stores a turn's input under c.
func (c TurnClaim) Metadata() map[string]string {
	m := map[string]string{ClaimedAtMetadataKey: c.At.UTC().Format(time.RFC3339Nano)}
	if c.NoteKey != "" {
		m[NoteKeyMetadataKey] = c.NoteKey
	}
	return m
}

// TurnClaimOf reads the claim a message's metadata stores it under; claimed is
// false for an ordinary message. A note key without a claim is an error: a note
// is stored only under its reservation.
func TurnClaimOf(metadata map[string]string) (c TurnClaim, claimed bool, err error) {
	at, claimed := metadata[ClaimedAtMetadataKey]
	if !claimed {
		if key := metadata[NoteKeyMetadataKey]; key != "" {
			return TurnClaim{}, false, fmt.Errorf("note %q without its reservation", key)
		}
		return TurnClaim{}, false, nil
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return TurnClaim{}, false, fmt.Errorf("claim time %q: %w", at, err)
	}
	return TurnClaim{NoteKey: metadata[NoteKeyMetadataKey], At: t}, true, nil
}

// Create returns the session for p.ID, minting a fresh empty record if absent.
// Idempotent on existing ids so a "fresh-intent" call from a reconnecting
// channel can never wipe a live session's messages — matches the DB-backed
// store's on-conflict behaviour (see store/session.go Create) and removes
// a silent-data-loss footgun that surfaced when channels routed a stale
// conversation_id into the create path instead of the load path.
func (s *SessionStore) Create(p SessionParams) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.sessions[p.ID]; ok {
		return existing
	}

	now := time.Now()
	sess := &Session{
		ID:        p.ID,
		Messages:  make([]provider.Message, 0),
		Metadata:  make(map[string]string),
		EntityID:  p.EntityID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[p.ID] = sess
	return sess
}

// OpenSession is the in-memory mirror of the DB-backed store's create-or-find
// (see store/session.go OpenSession): the first call for an entity, origin and
// key creates the session and answers created=true; every later call answers
// the stored session with created=false, whatever id it minted.
func (s *SessionStore) OpenSession(p SessionParams) (*Session, bool, error) {
	if p.OpenedBy == "" || p.OpenKey == "" {
		return nil, false, ErrOpenParams
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := openIdentity{entityID: p.EntityID, openedBy: p.OpenedBy, openKey: p.OpenKey}
	if id, ok := s.opened[key]; ok {
		return s.sessions[id], false, nil
	}
	if _, taken := s.sessions[p.ID]; taken {
		return nil, false, fmt.Errorf("open session %q: id already taken by another session", p.ID)
	}
	now := time.Now()
	sess := &Session{
		ID:        p.ID,
		Messages:  make([]provider.Message, 0),
		Metadata:  make(map[string]string),
		EntityID:  p.EntityID,
		OpenedBy:  p.OpenedBy,
		OpenKey:   p.OpenKey,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.sessions[p.ID] = sess
	s.opened[key] = p.ID
	return sess, true, nil
}

// ClaimOpener mirrors the DB-backed compare-and-set: the claim is won when the
// session has none, or when its claim is older than staleAfter and the first
// turn's input was not stored under it. A fresh claim is held, a stored one
// settled; an unknown session is settled, never won.
func (s *SessionStore) ClaimOpener(sessionID string, now time.Time, staleAfter time.Duration) (ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionID]; !ok {
		return ClaimSettled, nil
	}
	return take(s.claims, sessionID, now, staleAfter), nil
}

// ReserveNote mirrors the DB-backed reservation of a note key: the first call
// for a session and key wins, a fresh reservation is held, and a stale one is
// won again only while the note was not stored under it; a stored one is
// settled.
func (s *SessionStore) ReserveNote(sessionID, noteKey string, now time.Time, staleAfter time.Duration) (ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return take(s.notes, noteIdentity{sessionID: sessionID, noteKey: noteKey}, now, staleAfter), nil
}

// take takes claims[key] at now when it may be taken (claim.won) and reports
// what it found. The caller holds the store's lock.
func take[K comparable](claims map[K]*claim, key K, now time.Time, staleAfter time.Duration) ClaimResult {
	c := claims[key]
	switch {
	case c.won(now, staleAfter):
		claims[key] = &claim{at: now}
		return ClaimWon
	case c.stored:
		return ClaimSettled
	default:
		return ClaimHeld
	}
}

func (s *SessionStore) Get(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q: %w", id, ErrSessionNotFound)
	}
	return sess, nil
}

func (s *SessionStore) AddMessage(id string, msg provider.Message) error {
	return s.AddMessageWithMetadata(id, msg, nil)
}

// AddMessageWithMetadata mirrors the persistent store's signature and its
// reading of the metadata: "prompt_type", which the orchestrator's
// stripApprovedConfirmationExchanges uses to keep resolved confirmation
// exchanges out of the LLM history, is mirrored onto the stored message; a
// TurnClaim (TurnClaimOf) marks that claim stored, or fails with ErrClaimLost
// and stores nothing when it is not the claim held. The rest of the map is
// presentation state and is dropped.
func (s *SessionStore) AddMessageWithMetadata(id string, msg provider.Message, metadata map[string]string) error {
	msg.PromptType = metadata["prompt_type"]
	tc, claimed, err := TurnClaimOf(metadata)
	if err != nil {
		return fmt.Errorf("add message: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	if claimed {
		held := s.claims[id]
		if tc.NoteKey != "" {
			held = s.notes[noteIdentity{sessionID: id, noteKey: tc.NoteKey}]
		}
		if held == nil || held.stored || !held.at.Equal(tc.At) {
			return ErrClaimLost
		}
		held.stored = true
	}
	sess.Messages = append(sess.Messages, msg)
	sess.UpdatedAt = time.Now()
	return nil
}

func (s *SessionStore) SetModel(id string, model provider.ModelRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.ActiveModel = model
	sess.UpdatedAt = time.Now()
	return nil
}

// SetMetadata upserts a single metadata key on the session. An empty value
// removes the key so callers can both set and clear flags through one path.
// Used today by the set_debug_mode command to persist `debug=true` and the
// orchestrator's per-session debug-context flag reads it on every Run().
func (s *SessionStore) SetMetadata(id, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	if sess.Metadata == nil {
		sess.Metadata = make(map[string]string)
	}
	if value == "" {
		delete(sess.Metadata, key)
	} else {
		sess.Metadata[key] = value
	}
	sess.UpdatedAt = time.Now()
	return nil
}

// SetTitle fills the session's short label, but ONLY while it is still empty —
// auto-labelling (Orchestrator.maybeGenerateTitle) is a one-shot fill that must
// never overwrite a title set through another path. Matches the DB-backed
// store's guarded UPDATE (which matches only a NULL or empty title). No-op when
// the session is unknown so callers don't have to special-case races against Delete().
func (s *SessionStore) SetTitle(id, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	if sess.Title != "" {
		return nil
	}
	sess.Title = title
	sess.UpdatedAt = time.Now()
	return nil
}

// SetSummary updates the session summary and trims messages to the given slice (for summarization).
func (s *SessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Summary = summary
	sess.Messages = messages
	sess.UpdatedAt = time.Now()
	return nil
}

// ClearMessages drops the conversation history of a session — messages and
// the derived summary — while leaving the session row intact (ID, identity
// fields, ActiveModel, Metadata, Title, CreatedAt). Used by the clear_session
// command to reset the LLM context without losing entity/group association
// or audit events. Title is preserved intentionally: it labels the session
// in the picker dropdown (identity-shaped, like ID/Metadata), not the
// transcript that just got wiped. No-op if the session is not present.
func (s *SessionStore) ClearMessages(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	sess.Messages = make([]provider.Message, 0)
	sess.Summary = ""
	sess.UpdatedAt = time.Now()
	return nil
}

func (s *SessionStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	delete(s.claims, id)
	for key, sid := range s.opened {
		if sid == id {
			delete(s.opened, key)
		}
	}
	for key := range s.notes {
		if key.sessionID == id {
			delete(s.notes, key)
		}
	}
	return nil
}

func (s *SessionStore) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		result = append(result, sess)
	}
	return result
}

func (s *SessionStore) Save(id string) error {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session %q not found", id)
	}

	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("creating sessions dir: %w", err)
	}

	data, err := yaml.Marshal(sess)
	if err != nil {
		return fmt.Errorf("marshaling session: %w", err)
	}

	path := filepath.Join(s.dir, id+".yaml")
	return os.WriteFile(path, data, 0600)
}

func (s *SessionStore) Load(id string) error {
	path := filepath.Join(s.dir, id+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading session file: %w", err)
	}

	var sess Session
	if err := yaml.Unmarshal(data, &sess); err != nil {
		return fmt.Errorf("parsing session: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &sess
	return nil
}
