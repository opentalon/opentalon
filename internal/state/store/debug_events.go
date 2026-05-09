package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// DebugEvent is one captured LLM-endpoint exchange.
//
// Direction is one of "request", "response", "error". Body is JSON text when
// the captured payload was JSON (typically request and non-streaming response
// bodies); for streaming responses the orchestrator writes a single
// accumulated row at end-of-stream. Error rows carry the failure reason in
// Body and Status=0.
//
// URL is informational — debug capture is OpenAI-endpoint-scoped so URL is
// always the configured OpenAI completions URL; storing it makes audit
// dashboards self-explanatory and allows pointing the same plumbing at
// additional providers later without a schema change.
type DebugEvent struct {
	SessionID string
	TraceID   string
	Direction string // "request" | "response" | "error"
	Status    int    // HTTP status when known; 0 for request and error rows
	URL       string
	Body      string // raw JSON text or, for error rows, "Class: message"
	Timestamp time.Time
}

// DebugEventStore persists per-session LLM-endpoint debug events. The table
// is intentionally append-only (no updates); retention is handled by Prune.
type DebugEventStore struct {
	db *DB
}

// NewDebugEventStore returns a store backed by db. The schema is created by
// migration 007 and supports both SQLite and PostgreSQL.
func NewDebugEventStore(db *DB) *DebugEventStore {
	return &DebugEventStore{db: db}
}

// Insert appends one event. Caller is expected to fan in writes through the
// async-writer goroutine (see debug_writer.go) so the LLM hot-path is never
// blocked on the database.
func (s *DebugEventStore) Insert(ctx context.Context, e DebugEvent) error {
	id, err := newDebugEventID()
	if err != nil {
		return fmt.Errorf("debug event id: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ts := e.Timestamp.UTC().Format(time.RFC3339Nano)
	if e.Timestamp.IsZero() {
		ts = now
	}
	d := s.db.Dialect()
	_, err = s.db.SQLDB().ExecContext(ctx, d.Rebind(
		`INSERT INTO ai_debug_events (id, session_id, trace_id, ts, direction, status, url, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		id, e.SessionID, e.TraceID, ts, e.Direction, e.Status, e.URL, e.Body, now)
	if err != nil {
		return fmt.Errorf("debug event insert: %w", err)
	}
	return nil
}

// Prune deletes rows older than maxAge. Returns the number of rows removed.
// Safe to call concurrently with Insert; both run on independent connections
// from the pool.
func (s *DebugEventStore) Prune(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-maxAge).UTC().Format(time.RFC3339Nano)
	d := s.db.Dialect()
	res, err := s.db.SQLDB().ExecContext(ctx,
		d.Rebind(`DELETE FROM ai_debug_events WHERE ts < ?`), cutoff)
	if err != nil {
		return 0, fmt.Errorf("debug event prune: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountForSession returns how many events were captured for a session.
// Used by the set_debug_mode "status" reply so users can see the table is
// actually filling.
func (s *DebugEventStore) CountForSession(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.db.SQLDB().QueryRowContext(ctx,
		s.db.Dialect().Rebind(`SELECT COUNT(*) FROM ai_debug_events WHERE session_id = ?`),
		sessionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("debug event count: %w", err)
	}
	return n, nil
}

// newDebugEventID generates a 32-char hex id. We don't use bigserial because
// the schema is portable across SQLite and PostgreSQL — see migration 007.
func newDebugEventID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
