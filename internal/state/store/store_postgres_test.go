//go:build postgres

package store

import (
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/provider"
)

// Run with: go test -tags postgres -run TestPostgres ./internal/state/store/
// Requires DATABASE_URL pointing at a Postgres instance (e.g. "postgres://localhost/opentalon_test?sslmode=disable").

func pgDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	db, err := Open(config.DBConfig{Driver: "postgres", DSN: dsn}, "")
	if err != nil {
		t.Fatalf("Open postgres: %v", err)
	}
	t.Cleanup(func() {
		// Drop tables so each test starts clean.
		db.SQLDB().Exec("DROP TABLE IF EXISTS sessions, memories, schema_version")
		db.Close()
	})
	return db
}

func TestPostgres_OpenAndMigrations(t *testing.T) {
	db := pgDB(t)
	var v int
	if err := db.SQLDB().QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v != 2 {
		t.Errorf("schema_version = %d, want 2", v)
	}
}

func TestPostgres_AddMessageConcurrent(t *testing.T) {
	db := pgDB(t)
	store := NewSessionStore(db, 0, 0)
	store.Create("concurrent-test", "", "")

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := store.AddMessage("concurrent-test", provider.Message{
				Role:    provider.RoleUser,
				Content: "msg",
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("AddMessage error: %v", err)
	}

	sess, err := store.Get("concurrent-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != n {
		t.Errorf("got %d messages, want %d", len(sess.Messages), n)
	}
}

func TestPostgres_NativeToolCallsRoundTrip(t *testing.T) {
	db := pgDB(t)
	store := NewSessionStore(db, 0, 0)
	store.Create("tool-call-test", "", "")

	calls := []provider.ToolCall{{
		ID: "call_pg_1", Name: "tickets.show", Arguments: map[string]string{"id": "42"},
	}}
	if err := store.AddMessage("tool-call-test", provider.Message{
		Role: provider.RoleAssistant, ToolCalls: calls,
	}); err != nil {
		t.Fatalf("AddMessage assistant: %v", err)
	}
	if err := store.AddMessage("tool-call-test", provider.Message{
		Role: provider.RoleTool, Content: `{"status":"open"}`, ToolCallID: "call_pg_1",
	}); err != nil {
		t.Fatalf("AddMessage tool: %v", err)
	}

	sess, err := store.Get("tool-call-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Fatalf("len(Messages) = %d, want 2", len(sess.Messages))
	}
	if got := sess.Messages[0].ToolCalls; len(got) != 1 || got[0].ID != "call_pg_1" {
		t.Errorf("assistant ToolCalls mismatch on postgres: %+v", got)
	}
	if sess.Messages[1].ToolCallID != "call_pg_1" {
		t.Errorf("tool ToolCallID mismatch on postgres: %q", sess.Messages[1].ToolCallID)
	}

	// Empty slice must persist as NULL on Postgres as well (no "[]" sentinel).
	store.Create("empty-tool-calls", "", "")
	if err := store.AddMessage("empty-tool-calls", provider.Message{
		Role: provider.RoleAssistant, Content: "no tools", ToolCalls: []provider.ToolCall{},
	}); err != nil {
		t.Fatalf("AddMessage empty: %v", err)
	}
	var toolCalls sql.NullString
	err = db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT tool_calls FROM messages WHERE session_id = ? AND seq = ?`),
		"empty-tool-calls", 1,
	).Scan(&toolCalls)
	if err != nil {
		t.Fatalf("read empty row: %v", err)
	}
	if toolCalls.Valid {
		t.Errorf("empty-slice tool_calls = %q on postgres, want NULL", toolCalls.String)
	}
}
