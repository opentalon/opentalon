package store

import (
	"context"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDebugEventStore_InsertAndCount(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		err := store.Insert(ctx, DebugEvent{
			SessionID: "sess-A",
			TraceID:   "trace-A",
			Direction: "request",
			URL:       "https://example.invalid/v1/chat/completions",
			Body:      `{"model":"x"}`,
			Timestamp: time.Now(),
		})
		if err != nil {
			t.Fatalf("Insert[%d]: %v", i, err)
		}
	}
	// One event for a different session — must not be counted.
	_ = store.Insert(ctx, DebugEvent{SessionID: "sess-B", Direction: "response", Body: "{}"})

	n, err := store.CountForSession(ctx, "sess-A")
	if err != nil {
		t.Fatalf("CountForSession: %v", err)
	}
	if n != 3 {
		t.Errorf("CountForSession(sess-A) = %d, want 3", n)
	}
	n, _ = store.CountForSession(ctx, "sess-B")
	if n != 1 {
		t.Errorf("CountForSession(sess-B) = %d, want 1", n)
	}
	n, _ = store.CountForSession(ctx, "nonexistent")
	if n != 0 {
		t.Errorf("CountForSession(nonexistent) = %d, want 0", n)
	}
}

func TestDebugEventStore_Prune(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	ctx := context.Background()

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-1 * time.Minute)

	// Insert one old, one fresh.
	if err := store.Insert(ctx, DebugEvent{SessionID: "s", Direction: "request", Body: "{}", Timestamp: old}); err != nil {
		t.Fatalf("Insert old: %v", err)
	}
	if err := store.Insert(ctx, DebugEvent{SessionID: "s", Direction: "response", Body: "{}", Timestamp: fresh}); err != nil {
		t.Fatalf("Insert fresh: %v", err)
	}

	// Prune everything older than 24h.
	deleted, err := store.Prune(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if deleted != 1 {
		t.Errorf("Prune deleted %d, want 1", deleted)
	}

	n, _ := store.CountForSession(ctx, "s")
	if n != 1 {
		t.Errorf("post-prune count = %d, want 1", n)
	}

	// Prune with retention=0 is a no-op (caller signals "disabled").
	deleted, err = store.Prune(ctx, 0)
	if err != nil {
		t.Fatalf("Prune(0): %v", err)
	}
	if deleted != 0 {
		t.Errorf("Prune(0) deleted %d, want 0 (retention disabled)", deleted)
	}
}

func TestDebugEventStore_DefaultsTimestamp(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	ctx := context.Background()

	// Insert without explicit Timestamp — store should fill in now.
	if err := store.Insert(ctx, DebugEvent{SessionID: "s", Direction: "request", Body: "{}"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	n, _ := store.CountForSession(ctx, "s")
	if n != 1 {
		t.Errorf("count = %d, want 1", n)
	}
}
