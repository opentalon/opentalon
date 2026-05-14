package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// SessionEvent is one row of the session_events table.
//
// Producers populate everything except ID (generated) and CreatedAt
// (defaults to now). Seq is assigned atomically at insert time so callers
// never need to coordinate counters — the (session_id, seq) UNIQUE index
// enforces order.
//
// Payload is the marshalled event-type-specific struct from
// internal/state/store/events. The store does no validation beyond UTF-8
// + JSON well-formedness (see SessionEventStore.Insert) — semantic checks
// live in the producer per the raw-capture rule.
type SessionEvent struct {
	ID         string
	SessionID  string
	Seq        int64
	Timestamp  time.Time
	EventType  string
	ParentID   string // empty = root
	DurationMS int64  // 0 = not applicable
	Payload    json.RawMessage
}

// SessionEventStore persists structured session_events rows. The table is
// append-only — only the retention worker (Prune) removes rows. Reads are
// served via ListForSession and CountForSession; the api-plugin layers a
// HTTP endpoint on top.
//
// Schema lives in migration 009_session_events.sql and is portable across
// SQLite and PostgreSQL (TEXT/INTEGER only).
type SessionEventStore struct {
	db *DB
}

// NewSessionEventStore returns a store backed by db.
func NewSessionEventStore(db *DB) *SessionEventStore {
	return &SessionEventStore{db: db}
}

// Insert appends one event. Seq is auto-assigned per session via
// COALESCE(MAX(seq), 0) + 1 inside the INSERT, mirroring messages.seq.
// In-process ordering relies on the single-goroutine writer in
// session_events_writer.go — every event for a given session passes
// through one drain, so the MAX(seq) read and INSERT are effectively
// serialised. The UNIQUE (session_id, seq) index remains as a safety
// net for the cross-process case (multi-pod K8s with session affinity
// drifting under failover); a duplicate-key error there is currently
// logged and the event is dropped, which is acceptable for alpha-phase
// debug data.
//
// Validation is intentionally minimal:
//   - empty EventType / SessionID / Payload → error,
//   - Payload must be valid JSON (json.Valid check),
//   - UTF-8 invalid bytes in Payload are not rewritten here — producers
//     run their excerpts through events.SanitizeUTF8 before marshalling.
//
// Nothing else is touched. The raw-capture rule lives at the producer.
func (s *SessionEventStore) Insert(ctx context.Context, e SessionEvent) error {
	if e.SessionID == "" {
		return fmt.Errorf("session event: session_id required")
	}
	if e.EventType == "" {
		return fmt.Errorf("session event: event_type required")
	}
	if len(e.Payload) == 0 {
		return fmt.Errorf("session event: payload required")
	}
	if !json.Valid(e.Payload) {
		return fmt.Errorf("session event: payload is not valid JSON")
	}

	id := e.ID
	if id == "" {
		generated, err := newSessionEventID()
		if err != nil {
			return fmt.Errorf("session event id: %w", err)
		}
		id = generated
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ts := now
	if !e.Timestamp.IsZero() {
		ts = e.Timestamp.UTC().Format(time.RFC3339Nano)
	}

	parentID := sql.NullString{String: e.ParentID, Valid: e.ParentID != ""}
	duration := sql.NullInt64{Int64: e.DurationMS, Valid: e.DurationMS != 0}

	d := s.db.Dialect()
	_, err := s.db.SQLDB().ExecContext(ctx, d.Rebind(
		`INSERT INTO session_events (id, session_id, seq, ts, event_type, parent_id, duration_ms, payload, created_at)
		 SELECT ?, ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ?, ?, ?, ? FROM session_events WHERE session_id = ?`),
		id, e.SessionID, ts, e.EventType, parentID, duration, string(e.Payload), now, e.SessionID,
	)
	if err != nil {
		return fmt.Errorf("session event insert: %w", err)
	}
	return nil
}

// ListForSession returns events for a session in seq order. sinceSeq is
// exclusive (pass 0 for full history). limit <= 0 means "no limit".
//
// Callers should normally bound the read with a sensible limit since
// alpha-phase verbose capture can put hundreds of rows into a single
// session.
func (s *SessionEventStore) ListForSession(ctx context.Context, sessionID string, sinceSeq, limit int64) ([]SessionEvent, error) {
	d := s.db.Dialect()
	query := `SELECT id, session_id, seq, ts, event_type, parent_id, duration_ms, payload
	          FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq`
	args := []any{sessionID, sinceSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.SQLDB().QueryContext(ctx, d.Rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("session event list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionEvent
	for rows.Next() {
		var (
			ev       SessionEvent
			tsStr    string
			parent   sql.NullString
			duration sql.NullInt64
			payload  string
		)
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.Seq, &tsStr, &ev.EventType, &parent, &duration, &payload); err != nil {
			return nil, fmt.Errorf("session event scan: %w", err)
		}
		ev.Timestamp, _ = time.Parse(time.RFC3339Nano, tsStr)
		ev.ParentID = parent.String
		ev.DurationMS = duration.Int64
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session event iterate: %w", err)
	}
	return out, nil
}

// CountForSession returns how many events were captured for a session.
// Cheap on both engines thanks to idx_session_events_session_seq.
func (s *SessionEventStore) CountForSession(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.db.SQLDB().QueryRowContext(ctx,
		s.db.Dialect().Rebind(`SELECT COUNT(*) FROM session_events WHERE session_id = ?`),
		sessionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("session event count: %w", err)
	}
	return n, nil
}

// Prune deletes rows older than maxAge. Returns the number of rows
// removed. Mirrors DebugEventStore.Prune so the retention worker is a
// near-clone of the existing debug retention worker.
func (s *SessionEventStore) Prune(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339Nano)
	d := s.db.Dialect()
	res, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`DELETE FROM session_events WHERE ts < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("session event prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpsertPromptSnapshot inserts a content-addressed prompt body. Idempotent:
// repeated calls with the same sha256 are no-ops.
//
// Kind must be one of the events.PromptKind* constants. Caller is
// responsible for computing sha256 over the canonical content bytes —
// the store does not re-hash on insert (keeps the write hot-path cheap).
func (s *SessionEventStore) UpsertPromptSnapshot(ctx context.Context, sha256, kind, content string) error {
	if sha256 == "" || kind == "" {
		return fmt.Errorf("prompt snapshot: sha256 and kind required")
	}
	d := s.db.Dialect()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// ON CONFLICT DO NOTHING is portable across SQLite (>= 3.24, 2018) and
	// PostgreSQL — no dialect branch needed. Conflict target is the primary
	// key column. Same-sha repeat-writes are no-ops by construction
	// (content-addressed: same sha implies same body bytes).
	_, err := s.db.SQLDB().ExecContext(ctx, d.Rebind(
		`INSERT INTO prompt_snapshots (sha256, kind, content, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (sha256) DO NOTHING`),
		sha256, kind, content, now,
	)
	if err != nil {
		return fmt.Errorf("prompt snapshot upsert: %w", err)
	}
	return nil
}

// GetPromptSnapshot returns the stored body for a sha256. Returns
// (empty, "", nil) when the snapshot is unknown (caller treats as "body
// not retained" rather than an error).
func (s *SessionEventStore) GetPromptSnapshot(ctx context.Context, sha256 string) (content, kind string, err error) {
	row := s.db.SQLDB().QueryRowContext(ctx,
		s.db.Dialect().Rebind(`SELECT content, kind FROM prompt_snapshots WHERE sha256 = ?`), sha256)
	err = row.Scan(&content, &kind)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("prompt snapshot get: %w", err)
	}
	return content, kind, nil
}

// newSessionEventID returns a 32-char hex id, same shape as the debug
// event id — see the rationale in debug_events.go.
func newSessionEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
