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
// whether the message it was taken for has been stored since. Like the
// DB-backed markers, stored survives ClearMessages and SetSummary.
type claim struct {
	at     time.Time
	stored bool
}

// won reports whether a claim may be taken at now: there is none yet, or it
// is older than staleAfter and its message was never stored.
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
// session has none, or when its claim is older than staleAfter and no message
// was stored since it was taken. An unknown session is never won.
func (s *SessionStore) ClaimOpener(sessionID string, now time.Time, staleAfter time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sessions[sessionID]; !ok {
		return false, nil
	}
	if !s.claims[sessionID].won(now, staleAfter) {
		return false, nil
	}
	s.claims[sessionID] = &claim{at: now}
	return true, nil
}

// ReserveNote mirrors the DB-backed reservation of a note key: the first call
// for a session and key wins, a fresh reservation refuses, and a stale one is
// won again only while no message with that note key was stored.
func (s *SessionStore) ReserveNote(sessionID, noteKey string, now time.Time, staleAfter time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := noteIdentity{sessionID: sessionID, noteKey: noteKey}
	if !s.notes[key].won(now, staleAfter) {
		return false, nil
	}
	s.notes[key] = &claim{at: now}
	return true, nil
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

// AddMessageWithMetadata mirrors the persistent store's signature. Two
// metadata keys are consumed, as in the DB-backed store: "prompt_type", which
// the orchestrator's stripApprovedConfirmationExchanges uses to keep resolved
// confirmation exchanges out of the LLM history, is mirrored onto the stored
// message; "note_key" marks the session's reservation of that key as stored.
// Any message marks a pending opener claim as stored. The rest of the map is
// presentation state and is dropped.
func (s *SessionStore) AddMessageWithMetadata(id string, msg provider.Message, metadata map[string]string) error {
	msg.PromptType = metadata["prompt_type"]
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Messages = append(sess.Messages, msg)
	sess.UpdatedAt = time.Now()
	if c := s.claims[id]; c != nil {
		c.stored = true
	}
	if k := metadata["note_key"]; k != "" {
		if c := s.notes[noteIdentity{sessionID: id, noteKey: k}]; c != nil {
			c.stored = true
		}
	}
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
