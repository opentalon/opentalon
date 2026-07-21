package store

import (
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
)

// TestSQLiteBusyTimeoutAppliesToPooledConns reproduces issue #313: a second
// writer landing on a freshly-opened pool connection must inherit busy_timeout
// and wait for the lock, not fail immediately with SQLITE_BUSY.
//
// It holds a write lock on one connection, then issues a write from the pool.
// database/sql opens a *different* connection for that write (the first is busy
// in a tx). If busy_timeout only lived on the first connection, the second one
// returns "database is locked (5) (SQLITE_BUSY)" instantly.
func TestSQLiteBusyTimeoutAppliesToPooledConns(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sqldb := db.SQLDB()
	if _, err := sqldb.Exec("CREATE TABLE busy_probe (x INTEGER)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}

	// Acquire a write lock on connection A and keep it held.
	txA, err := sqldb.Begin()
	if err != nil {
		t.Fatalf("begin txA: %v", err)
	}
	if _, err := txA.Exec("INSERT INTO busy_probe VALUES (1)"); err != nil {
		t.Fatalf("txA write: %v", err)
	}

	// Connection B writes concurrently; must block on the lock, then succeed
	// once txA commits — not error out immediately.
	done := make(chan error, 1)
	go func() {
		_, err := sqldb.Exec("INSERT INTO busy_probe VALUES (2)")
		done <- err
	}()

	// Give B time to contend and (buggy path) fail fast.
	time.Sleep(300 * time.Millisecond)
	if err := txA.Commit(); err != nil {
		t.Fatalf("commit txA: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second writer failed instead of waiting: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("second writer never completed")
	}
}

func TestVerifySQLitePragmas(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var journal string
	if err := db.SQLDB().QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(journal, "wal") {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
	var busy int
	if err := db.SQLDB().QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busy != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", busy)
	}
}
