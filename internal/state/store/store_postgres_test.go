//go:build postgres

package store

import (
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
	store.Create("concurrent-test")

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
