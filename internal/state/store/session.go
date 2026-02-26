package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// SessionStore is the SQLite-backed session store.
type SessionStore struct {
	db          *DB
	maxMessages int // 0 = no cap
	maxIdleDays int // 0 = don't prune
}

// NewSessionStore returns a session store that uses the given DB.
// maxMessages caps messages per session (0 = no cap); maxIdleDays enables pruning of idle sessions (0 = off).
func NewSessionStore(db *DB, maxMessages, maxIdleDays int) *SessionStore {
	return &SessionStore{db: db, maxMessages: maxMessages, maxIdleDays: maxIdleDays}
}

// Get loads a session by id. Returns error if not found.
func (s *SessionStore) Get(id string) (*state.Session, error) {
	var messagesJSON, summary, activeModel, metadataJSON, createdAt, updatedAt string
	err := s.db.SQLDB().QueryRow(
		`SELECT messages, summary, active_model, metadata, created_at, updated_at FROM sessions WHERE id = ?`,
		id,
	).Scan(&messagesJSON, &summary, &activeModel, &metadataJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("session %q not found", id)
	}
	var messages []provider.Message
	if messagesJSON != "" {
		_ = json.Unmarshal([]byte(messagesJSON), &messages)
	}
	var metadata map[string]string
	if metadataJSON != "" {
		_ = json.Unmarshal([]byte(metadataJSON), &metadata)
	}
	ca, _ := time.Parse(time.RFC3339, createdAt)
	ua, _ := time.Parse(time.RFC3339, updatedAt)
	return &state.Session{
		ID:          id,
		Messages:    messages,
		Summary:     summary,
		ActiveModel: provider.ModelRef(activeModel),
		Metadata:    metadata,
		CreatedAt:   ca,
		UpdatedAt:   ua,
	}, nil
}

// Create inserts a new session. If id already exists (e.g. race), returns existing session from DB.
func (s *SessionStore) Create(id string) *state.Session {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().Exec(
		`INSERT INTO sessions (id, messages, summary, active_model, metadata, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, "[]", "", "", "{}", now, now)
	if err != nil {
		if existing, e := s.Get(id); e == nil {
			return existing
		}
		// Return in-memory session so caller can continue; next AddMessage will fail or overwrite.
		return &state.Session{
			ID:        id,
			Messages:  []provider.Message{},
			Metadata:  map[string]string{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}
	return &state.Session{
		ID:        id,
		Messages:  []provider.Message{},
		Metadata:  map[string]string{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// AddMessage appends a message and persists. If maxMessages > 0, trims to last maxMessages.
func (s *SessionStore) AddMessage(id string, msg provider.Message) error {
	sess, err := s.Get(id)
	if err != nil {
		return err
	}
	sess.Messages = append(sess.Messages, msg)
	sess.UpdatedAt = time.Now()
	if s.maxMessages > 0 && len(sess.Messages) > s.maxMessages {
		keep := len(sess.Messages) - s.maxMessages
		sess.Messages = sess.Messages[keep:]
	}
	return s.persist(sess)
}

// SetModel updates the active model and persists.
func (s *SessionStore) SetModel(id string, model provider.ModelRef) error {
	sess, err := s.Get(id)
	if err != nil {
		return err
	}
	sess.ActiveModel = model
	sess.UpdatedAt = time.Now()
	return s.persist(sess)
}

// SetSummary updates the session summary (for summarization feature) and persists.
func (s *SessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	sess, err := s.Get(id)
	if err != nil {
		return err
	}
	sess.Summary = summary
	sess.Messages = messages
	sess.UpdatedAt = time.Now()
	return s.persist(sess)
}

func (s *SessionStore) persist(sess *state.Session) error {
	messagesJSON, err := json.Marshal(sess.Messages)
	if err != nil {
		return fmt.Errorf("session persist: marshal messages: %w", err)
	}
	metadataJSON, _ := json.Marshal(sess.Metadata)
	if metadataJSON == nil {
		metadataJSON = []byte("{}")
	}
	updatedAt := sess.UpdatedAt.UTC().Format(time.RFC3339)
	_, err = s.db.SQLDB().Exec(
		`UPDATE sessions SET messages = ?, summary = ?, active_model = ?, metadata = ?, updated_at = ? WHERE id = ?`,
		string(messagesJSON), sess.Summary, string(sess.ActiveModel), string(metadataJSON), updatedAt, sess.ID)
	if err != nil {
		return fmt.Errorf("session persist: %w", err)
	}
	return nil
}

// List is not used by orchestrator but may be needed for idle prune. Returns all session ids.
func (s *SessionStore) List() ([]string, error) {
	rows, err := s.db.SQLDB().Query(`SELECT id FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Delete removes a session (for tests or admin).
func (s *SessionStore) Delete(id string) error {
	_, err := s.db.SQLDB().Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// PruneIdleSessions deletes sessions not updated in the last maxIdleDays days.
// No-op if maxIdleDays <= 0. Call on startup or periodically.
func (s *SessionStore) PruneIdleSessions() error {
	if s.maxIdleDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -s.maxIdleDays).UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().Exec(`DELETE FROM sessions WHERE updated_at < ?`, cutoff)
	return err
}
