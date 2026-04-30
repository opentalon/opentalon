package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// SessionStore is the database-backed session store.
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

// Get loads a session by id. Messages are loaded from the messages table.
func (s *SessionStore) Get(id string) (*state.Session, error) {
	d := s.db.Dialect()
	var summary, activeModel, metadataJSON, createdAt, updatedAt string
	err := s.db.SQLDB().QueryRow(
		d.Rebind(`SELECT COALESCE(summary,''), active_model, metadata, created_at, updated_at FROM sessions WHERE id = ?`),
		id,
	).Scan(&summary, &activeModel, &metadataJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("session %q not found", id)
	}

	messages, err := s.loadMessages(id)
	if err != nil {
		return nil, err
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
func (s *SessionStore) Create(id, entityID, groupID string) *state.Session {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().Exec(
		s.db.Dialect().Rebind(`INSERT INTO sessions (id, summary, active_model, metadata, entity_id, group_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		id, "", "", "{}", entityID, groupID, now, now)
	if err != nil {
		if existing, e := s.Get(id); e == nil {
			return existing
		}
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

// AddMessage inserts a message into the messages table and updates the session timestamp.
func (s *SessionStore) AddMessage(id string, msg provider.Message) error {
	ctx := context.Background()
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339)

	// Atomically assign next seq in a single statement to avoid race conditions
	// between concurrent AddMessage calls.
	_, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`INSERT INTO messages (session_id, seq, role, content, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM messages WHERE session_id = ?`),
		id, string(msg.Role), msg.Content, now, id)
	if err != nil {
		return fmt.Errorf("add message insert: %w", err)
	}

	// Trim if maxMessages is set.
	if s.maxMessages > 0 {
		_, _ = s.db.SQLDB().ExecContext(ctx,
			d.Rebind(`DELETE FROM messages WHERE session_id = ? AND seq <= (SELECT MAX(seq) - ? FROM messages WHERE session_id = ?)`),
			id, s.maxMessages, id)
	}

	// Touch session updated_at.
	_, _ = s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET updated_at = ? WHERE id = ?`), now, id)

	return nil
}

// SetModel updates the active model and persists.
func (s *SessionStore) SetModel(id string, model provider.ModelRef) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().Exec(
		s.db.Dialect().Rebind(`UPDATE sessions SET active_model = ?, updated_at = ? WHERE id = ?`),
		string(model), updatedAt, id)
	if err != nil {
		return fmt.Errorf("set model: %w", err)
	}
	return nil
}

// SetSummary updates the session summary and replaces messages with the given slice.
func (s *SessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	ctx := context.Background()
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.SQLDB().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set summary begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Delete all existing messages for this session.
	if _, err := tx.ExecContext(ctx,
		d.Rebind(`DELETE FROM messages WHERE session_id = ?`), id); err != nil {
		return fmt.Errorf("set summary delete: %w", err)
	}

	// Insert the kept messages with fresh seq numbers.
	for i, msg := range messages {
		if _, err := tx.ExecContext(ctx,
			d.Rebind(`INSERT INTO messages (session_id, seq, role, content, created_at) VALUES (?, ?, ?, ?, ?)`),
			id, i+1, string(msg.Role), msg.Content, now); err != nil {
			return fmt.Errorf("set summary insert: %w", err)
		}
	}

	// Update session summary and timestamp.
	if _, err := tx.ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET summary = ?, updated_at = ? WHERE id = ?`),
		summary, now, id); err != nil {
		return fmt.Errorf("set summary update: %w", err)
	}
	return tx.Commit()
}

// List returns all session ids.
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

// Delete removes a session and its messages.
func (s *SessionStore) Delete(id string) error {
	d := s.db.Dialect()
	// Delete messages first (FK may not cascade in all configs).
	_, _ = s.db.SQLDB().Exec(d.Rebind(`DELETE FROM messages WHERE session_id = ?`), id)
	_, err := s.db.SQLDB().Exec(d.Rebind(`DELETE FROM sessions WHERE id = ?`), id)
	return err
}

// PruneIdleSessions deletes sessions not updated in the last maxIdleDays days.
func (s *SessionStore) PruneIdleSessions() error {
	if s.maxIdleDays <= 0 {
		return nil
	}
	d := s.db.Dialect()
	cutoff := time.Now().AddDate(0, 0, -s.maxIdleDays).UTC().Format(time.RFC3339)
	// Delete messages for pruned sessions.
	_, _ = s.db.SQLDB().Exec(d.Rebind(`DELETE FROM messages WHERE session_id IN (SELECT id FROM sessions WHERE updated_at < ?)`), cutoff)
	_, err := s.db.SQLDB().Exec(d.Rebind(`DELETE FROM sessions WHERE updated_at < ?`), cutoff)
	return err
}

// loadMessages reads all messages for a session from the messages table, ordered by seq.
func (s *SessionStore) loadMessages(sessionID string) ([]provider.Message, error) {
	rows, err := s.db.SQLDB().Query(
		s.db.Dialect().Rebind(`SELECT role, content FROM messages WHERE session_id = ? ORDER BY seq`),
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []provider.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, fmt.Errorf("load messages scan: %w", err)
		}
		messages = append(messages, provider.Message{
			Role:    provider.Role(role),
			Content: content,
		})
	}
	if messages == nil {
		messages = []provider.Message{}
	}
	return messages, rows.Err()
}
