package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// toolCallsAsNullString marshals a non-empty ToolCalls slice into JSON; an
// empty slice persists as SQL NULL (not "[]") so consumers can cheaply filter
// for rows that carry structured tool-call data.
func toolCallsAsNullString(calls []provider.ToolCall) (sql.NullString, error) {
	if len(calls) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(calls)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal tool_calls: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// storedThinking is the messages.thinking column: a message's thinking blocks
// with the fingerprint of the request prefix they are bound to.
type storedThinking struct {
	Fingerprint string            `json:"fingerprint"`
	Blocks      []json.RawMessage `json:"blocks"`
}

// thinkingAsNullString marshals a message's thinking blocks and their
// fingerprint; no blocks persist as SQL NULL, like tool_calls.
func thinkingAsNullString(msg provider.Message) (sql.NullString, error) {
	if len(msg.Thinking) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(storedThinking{Fingerprint: msg.ThinkingFingerprint, Blocks: msg.Thinking})
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal thinking: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// loadThinking reads the messages.thinking column into msg. A plain array of
// blocks, written before the fingerprint existed, loads without one, so it is
// never replayed.
func loadThinking(column string, msg *provider.Message) error {
	if strings.HasPrefix(strings.TrimSpace(column), "[") {
		return json.Unmarshal([]byte(column), &msg.Thinking)
	}
	var t storedThinking
	if err := json.Unmarshal([]byte(column), &t); err != nil {
		return err
	}
	msg.Thinking, msg.ThinkingFingerprint = t.Blocks, t.Fingerprint
	return nil
}

// metadataAsNullString marshals a non-empty per-message metadata map into JSON;
// an empty/nil map persists as SQL NULL (not "{}") so an ordinary chat turn's
// row stays byte-identical to the pre-013 shape and readers can cheaply filter
// for rows that actually carry metadata.
func metadataAsNullString(metadata map[string]string) (sql.NullString, error) {
	if len(metadata) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(metadata)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal metadata: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

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
// Returns a wrapped state.ErrSessionNotFound when the row is genuinely absent
// (sql.ErrNoRows), and a wrapped infrastructure error otherwise — preserving
// that distinction is what lets the channel handler decide between "tell the
// user their conversation expired" and "tell the user to retry".
func (s *SessionStore) Get(id string) (*state.Session, error) {
	d := s.db.Dialect()
	var summary, title, activeModel, metadataJSON, createdAt, updatedAt string
	err := s.db.SQLDB().QueryRow(
		d.Rebind(`SELECT COALESCE(summary,''), COALESCE(title,''), active_model, metadata, created_at, updated_at FROM sessions WHERE id = ?`),
		id,
	).Scan(&summary, &title, &activeModel, &metadataJSON, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session %q: %w", id, state.ErrSessionNotFound)
		}
		return nil, fmt.Errorf("session %q load: %w", id, err)
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
		Title:       title,
		ActiveModel: provider.ModelRef(activeModel),
		Metadata:    metadata,
		CreatedAt:   ca,
		UpdatedAt:   ua,
	}, nil
}

// Create inserts a new session. If id already exists (e.g. race), returns existing session from DB.
//
// p.Kind is the session's interaction_kind ("chat" | "system"); an empty kind is
// stored as "chat". p.SystemSource is the per-feature label of the in-app feature
// that opened the session (migration 016); empty is stored as NULL, which is
// what an ordinary human chat leaves behind.
//
// Both labels are written here and nowhere else: an id that already exists
// keeps the labels it was created with, because the INSERT conflicts and the
// existing row is returned untouched. That is what makes a chat session which
// later receives an injected system turn keep its NULL source while that turn's
// own usage row carries the feature label.
func (s *SessionStore) Create(p state.SessionParams) *state.Session {
	now := time.Now().UTC().Format(time.RFC3339)
	kind := p.Kind
	if kind == "" {
		kind = "chat"
	}
	source := sql.NullString{String: p.SystemSource, Valid: p.SystemSource != ""}
	_, err := s.db.SQLDB().Exec(
		s.db.Dialect().Rebind(`INSERT INTO sessions (id, summary, active_model, metadata, entity_id, group_id, interaction_kind, system_source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		p.ID, "", "", "{}", p.EntityID, p.GroupID, kind, source, now, now)
	if err != nil {
		if existing, e := s.Get(p.ID); e == nil {
			return existing
		}
		// Not the id race this path exists for: nothing was written and the
		// caller cannot tell. Say so, or the first symptom is a message write
		// failing later against a row that never existed.
		slog.Warn("session insert failed with no existing row; continuing with a transient session",
			"id", p.ID, "error", err)
		return &state.Session{
			ID:        p.ID,
			Messages:  []provider.Message{},
			Metadata:  map[string]string{},
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}
	return &state.Session{
		ID:        p.ID,
		Messages:  []provider.Message{},
		Metadata:  map[string]string{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

// sessionRowLocker is the slice of a transaction lockSessionRow needs; both
// *sql.Tx and the dialect's ExclusiveTx satisfy it.
type sessionRowLocker interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// lockSessionRow serialises every writer of one session's transcript behind
// its sessions row. It is the FIRST statement of each such transaction, so
// they all take locks in the same order (sessions row, then messages rows)
// and can never wait on each other in a cycle. On Postgres it is FOR UPDATE;
// on SQLite the suffix is empty and BEGIN IMMEDIATE already serialises
// writers. A session with no row (nothing to lock) is not an error: the
// caller then keeps its single-statement behaviour.
func lockSessionRow(ctx context.Context, tx sessionRowLocker, d Dialect, id string) error {
	var one int
	err := tx.QueryRowContext(ctx, d.Rebind(`SELECT 1 FROM sessions WHERE id = ?`+d.ForUpdate()), id).Scan(&one)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// AddMessage inserts a message into the messages table and updates the session timestamp.
// All statements run in a single transaction to reduce network roundtrips to PostgreSQL.
func (s *SessionStore) AddMessage(id string, msg provider.Message) error {
	return s.AddMessageWithMetadata(id, msg, nil)
}

// AddMessageWithMetadata is AddMessage plus a small JSON map persisted in the
// messages.metadata column (migration 013). The metadata is presentation/UI
// state (e.g. tool-confirmation markers) surfaced by the transcript reader.
// loadMessages reads back exactly ONE key — "prompt_type", into
// Message.PromptType — as the signal the orchestrator uses to keep resolved
// confirmation exchanges out of the LLM history; everything else in the map
// never reaches the model. A nil/empty map persists as SQL NULL, leaving
// ordinary chat turns byte-identical to the pre-013 shape.
func (s *SessionStore) AddMessageWithMetadata(id string, msg provider.Message, metadata map[string]string) error {
	ctx := context.Background()
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339)

	toolCallsJSON, err := toolCallsAsNullString(msg.ToolCalls)
	if err != nil {
		return err
	}
	thinkingJSON, err := thinkingAsNullString(msg)
	if err != nil {
		return err
	}
	toolCallID := sql.NullString{String: msg.ToolCallID, Valid: msg.ToolCallID != ""}
	visibility := sql.NullString{String: msg.Visibility, Valid: msg.Visibility != ""}
	metadataJSON, err := metadataAsNullString(metadata)
	if err != nil {
		return err
	}

	tx, cleanup, err := d.BeginExclusive(ctx, s.db.SQLDB())
	if err != nil {
		return fmt.Errorf("add message begin: %w", err)
	}
	defer cleanup()
	defer func() { _ = tx.Rollback() }()

	// Serialise the writers of one session before the seq is assigned. The
	// INSERT below computes MAX(seq)+1 in a single statement, which is atomic
	// on SQLite but not on Postgres: two read-committed transactions both
	// read the same MAX and the second INSERT hits idx_messages_session_seq.
	// The row lock makes the second writer wait and recompute.
	if err := lockSessionRow(ctx, tx, d, id); err != nil {
		return fmt.Errorf("add message lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		d.Rebind(`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, thinking, metadata, visibility, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ?, ?, ?, ? FROM messages WHERE session_id = ?`),
		id, string(msg.Role), msg.Content, toolCallsJSON, toolCallID, thinkingJSON, metadataJSON, visibility, now, id); err != nil {
		return fmt.Errorf("add message insert: %w", err)
	}

	// Trim if maxMessages is set.
	if s.maxMessages > 0 {
		_, _ = tx.ExecContext(ctx,
			d.Rebind(`DELETE FROM messages WHERE session_id = ? AND seq <= (SELECT MAX(seq) - ? FROM messages WHERE session_id = ?)`),
			id, s.maxMessages, id)
	}

	// Touch session updated_at.
	_, _ = tx.ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET updated_at = ? WHERE id = ?`), now, id)

	return tx.Commit()
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

// SetMetadata upserts a single key in the session's metadata JSON column. An
// empty value removes the key. The whole map is read, mutated in memory, and
// written back in one transaction — debug-toggle traffic is rare (one row per
// /debug command), so the simpler read-modify-write loses nothing here and
// avoids a JSON-merge UDF that would only work on one dialect.
func (s *SessionStore) SetMetadata(id, key, value string) error {
	ctx := context.Background()
	d := s.db.Dialect()

	tx, cleanup, err := d.BeginExclusive(ctx, s.db.SQLDB())
	if err != nil {
		return fmt.Errorf("set metadata begin: %w", err)
	}
	defer cleanup()
	defer func() { _ = tx.Rollback() }()

	var mdJSON string
	err = tx.QueryRowContext(ctx,
		d.Rebind(`SELECT COALESCE(metadata,'{}') FROM sessions WHERE id = ?`+d.ForUpdate()),
		id,
	).Scan(&mdJSON)
	if err != nil {
		return fmt.Errorf("set metadata select: %w", err)
	}
	md := map[string]string{}
	if mdJSON != "" {
		_ = json.Unmarshal([]byte(mdJSON), &md)
	}
	if value == "" {
		delete(md, key)
	} else {
		md[key] = value
	}
	updated, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("set metadata marshal: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET metadata = ?, updated_at = ? WHERE id = ?`),
		string(updated), now, id); err != nil {
		return fmt.Errorf("set metadata update: %w", err)
	}
	return tx.Commit()
}

// SetTitle fills the session's short label, but ONLY while it is still empty:
// the UPDATE matches only a NULL or empty title. Auto-labelling
// (Orchestrator.maybeGenerateTitle) is a one-shot fill that runs once after the
// first assistant turn — it must never overwrite a title set through another
// path (e.g. a rename via a REST endpoint reading the same sessions table),
// even if that rename lands during the title-generation LLM call. The
// conditional WHERE closes that race at the DB level: the auto write only ever
// wins an empty slot, the other (unconditional) writer always wins. Missing id
// is a no-op (race against Delete).
func (s *SessionStore) SetTitle(id, title string) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.SQLDB().Exec(
		s.db.Dialect().Rebind(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ? AND (title IS NULL OR title = '')`),
		title, updatedAt, id)
	if err != nil {
		return fmt.Errorf("set title: %w", err)
	}
	return nil
}

// SetSummary updates the session summary and replaces messages with the given slice.
func (s *SessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	ctx := context.Background()
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, cleanup, err := d.BeginExclusive(ctx, s.db.SQLDB())
	if err != nil {
		return fmt.Errorf("set summary begin: %w", err)
	}
	defer cleanup()
	defer func() { _ = tx.Rollback() }()

	// Same lock order as AddMessage (sessions row first): this transaction
	// also touches messages and then the sessions row, and taking them the
	// other way round deadlocks against a concurrent AddMessage that trims
	// old rows. The lock also keeps the fresh seq numbering below from
	// colliding with a message appended in between.
	if err := lockSessionRow(ctx, tx, d, id); err != nil {
		return fmt.Errorf("set summary lock: %w", err)
	}

	// Delete all existing messages for this session.
	if _, err := tx.ExecContext(ctx,
		d.Rebind(`DELETE FROM messages WHERE session_id = ?`), id); err != nil {
		return fmt.Errorf("set summary delete: %w", err)
	}

	// Insert the kept messages with fresh seq numbers.
	for i, msg := range messages {
		toolCallsJSON, err := toolCallsAsNullString(msg.ToolCalls)
		if err != nil {
			return err
		}
		thinkingJSON, err := thinkingAsNullString(msg)
		if err != nil {
			return err
		}
		toolCallID := sql.NullString{String: msg.ToolCallID, Valid: msg.ToolCallID != ""}
		visibility := sql.NullString{String: msg.Visibility, Valid: msg.Visibility != ""}
		if _, err := tx.ExecContext(ctx,
			d.Rebind(`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, thinking, visibility, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, i+1, string(msg.Role), msg.Content, toolCallsJSON, toolCallID, thinkingJSON, visibility, now); err != nil {
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

// ClearMessages drops the conversation history of a session — messages and
// the derived summary — atomically. The session row itself stays (entity_id,
// group_id, active_model, metadata, title, created_at all preserved), and
// the session_events audit log is untouched. Used by the clear_session
// command so /clear resets LLM context without orphaning the session's
// identity or erasing telemetry. Title is preserved on purpose (same as
// the in-memory variant): it labels the session in the picker dropdown,
// not the transcript that just got wiped.
func (s *SessionStore) ClearMessages(id string) error {
	ctx := context.Background()
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, cleanup, err := d.BeginExclusive(ctx, s.db.SQLDB())
	if err != nil {
		return fmt.Errorf("clear messages begin: %w", err)
	}
	defer cleanup()
	defer func() { _ = tx.Rollback() }()

	// Sessions row first — the one lock order every transcript writer uses.
	if err := lockSessionRow(ctx, tx, d, id); err != nil {
		return fmt.Errorf("clear messages lock: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		d.Rebind(`DELETE FROM messages WHERE session_id = ?`), id); err != nil {
		return fmt.Errorf("clear messages delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		d.Rebind(`UPDATE sessions SET summary = '', updated_at = ? WHERE id = ?`),
		now, id); err != nil {
		return fmt.Errorf("clear messages update: %w", err)
	}
	return tx.Commit()
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
		s.db.Dialect().Rebind(`SELECT role, content, tool_calls, tool_call_id, thinking, visibility, metadata FROM messages WHERE session_id = ? ORDER BY seq`),
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []provider.Message
	for rows.Next() {
		var role, content string
		var toolCallsJSON, toolCallID, thinkingJSON, visibility, metadataJSON sql.NullString
		if err := rows.Scan(&role, &content, &toolCallsJSON, &toolCallID, &thinkingJSON, &visibility, &metadataJSON); err != nil {
			return nil, fmt.Errorf("load messages scan: %w", err)
		}
		msg := provider.Message{
			Role:       provider.Role(role),
			Content:    content,
			ToolCallID: toolCallID.String, // zero value "" when NULL — safe
			Visibility: visibility.String, // "" when NULL = visible
		}
		if toolCallsJSON.Valid {
			if err := json.Unmarshal([]byte(toolCallsJSON.String), &msg.ToolCalls); err != nil {
				return nil, fmt.Errorf("load messages unmarshal tool_calls: %w", err)
			}
		}
		if thinkingJSON.Valid {
			if err := loadThinking(thinkingJSON.String, &msg); err != nil {
				return nil, fmt.Errorf("load messages unmarshal thinking: %w", err)
			}
		}
		// Only the prompt_type marker is read back — the orchestrator uses it
		// to keep resolved confirmation exchanges out of the LLM history. The
		// rest of the metadata map stays presentation state. Tolerant parse: a
		// broken blob on one row must not brick the whole session's history.
		if metadataJSON.Valid && metadataJSON.String != "" {
			var meta map[string]string
			if err := json.Unmarshal([]byte(metadataJSON.String), &meta); err == nil {
				msg.PromptType = meta["prompt_type"]
			}
		}
		messages = append(messages, msg)
	}
	if messages == nil {
		messages = []provider.Message{}
	}
	return messages, rows.Err()
}
