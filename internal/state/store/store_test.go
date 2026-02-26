package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
)

func TestOpenAndMigrations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var v int
	err = db.SQLDB().QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version = %d, want 1", v)
	}

	// Re-open: idempotent, no error
	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	defer db2.Close()
	err = db2.SQLDB().QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err != nil {
		t.Fatalf("read schema_version (second open): %v", err)
	}
	if v != 1 {
		t.Errorf("schema_version after re-open = %d, want 1", v)
	}
}

func TestMemoryStore_AddScopedAndMemoriesForContext(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	mem := NewMemoryStore(db)
	ctx := context.Background()

	// Add general (actorID empty)
	_, err = mem.AddScoped(ctx, "", "general rule", "rule")
	if err != nil {
		t.Fatalf("AddScoped general: %v", err)
	}
	// Add per-actor
	ctxA := actor.WithActor(ctx, "slack:U1")
	_, err = mem.AddScoped(ctxA, "slack:U1", "user one workflow", "workflow")
	if err != nil {
		t.Fatalf("AddScoped actor: %v", err)
	}
	ctxB := actor.WithActor(ctx, "slack:U2")
	_, err = mem.AddScoped(ctxB, "slack:U2", "user two workflow", "workflow")
	if err != nil {
		t.Fatalf("AddScoped actor 2: %v", err)
	}

	// MemoriesForContext(ctxA, "workflow") should return general + U1 only (general has no workflow tag here; we only added "rule")
	// So we need a general memory with workflow tag for this test to be meaningful. Add one.
	_, _ = mem.AddScoped(ctx, "", "shared workflow", "workflow")

	list, err := mem.MemoriesForContext(ctxA, "workflow")
	if err != nil {
		t.Fatalf("MemoriesForContext: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("MemoriesForContext(workflow) for U1: got %d, want 2 (general + U1)", len(list))
	}
	list, _ = mem.MemoriesForContext(ctxB, "workflow")
	if len(list) != 2 {
		t.Errorf("MemoriesForContext(workflow) for U2: got %d, want 2 (general + U2)", len(list))
	}
}

func TestSessionStore_PersistAndGet(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1")
	err = sessStore.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "hello"})
	if err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	err = sessStore.AddMessage("s1", provider.Message{Role: provider.RoleAssistant, Content: "hi"})
	if err != nil {
		t.Fatalf("AddMessage 2: %v", err)
	}

	sess, err := sessStore.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != 2 {
		t.Errorf("len(Messages) = %d, want 2", len(sess.Messages))
	}

	// Re-open DB and get again: should persist
	db.Close()
	db2, _ := Open(dir)
	defer db2.Close()
	sessStore2 := NewSessionStore(db2, 0, 0)
	sess2, err := sessStore2.Get("s1")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if len(sess2.Messages) != 2 {
		t.Errorf("after reopen len(Messages) = %d, want 2", len(sess2.Messages))
	}
}

func TestSessionStore_MaxMessagesTrim(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	sessStore := NewSessionStore(db, 3, 0) // keep last 3
	sessStore.Create("s1")
	for i := 0; i < 5; i++ {
		_ = sessStore.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "msg"})
	}

	sess, _ := sessStore.Get("s1")
	if len(sess.Messages) != 3 {
		t.Errorf("len(Messages) = %d, want 3 (trimmed)", len(sess.Messages))
	}
}

func TestSessionStore_SetSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1")
	_ = sessStore.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "a"})
	err = sessStore.SetSummary("s1", "Summary of past conversation.", []provider.Message{
		{Role: provider.RoleUser, Content: "last user"},
		{Role: provider.RoleAssistant, Content: "last assistant"},
	})
	if err != nil {
		t.Fatalf("SetSummary: %v", err)
	}

	sess, _ := sessStore.Get("s1")
	if sess.Summary != "Summary of past conversation." {
		t.Errorf("Summary = %q", sess.Summary)
	}
	if len(sess.Messages) != 2 {
		t.Errorf("len(Messages) = %d, want 2", len(sess.Messages))
	}
}

func TestRunPluginMigrations_NoMigrationsDir(t *testing.T) {
	dir := t.TempDir()
	err := RunPluginMigrations(dir, "myplugin", dir)
	if err != nil {
		t.Fatalf("RunPluginMigrations (no migrations dir): %v", err)
	}
	// Should create plugin_data/myplugin.db with schema_version if we had run any migration; with no dir it's no-op
}

func TestRunPluginMigrations_WithMigrations(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, "plugin")
	if err := os.MkdirAll(filepath.Join(pluginDir, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	// Write 001_initial.sql (runner creates schema_version; migration can add plugin tables)
	sql := `CREATE TABLE IF NOT EXISTS plugin_data (id TEXT PRIMARY KEY);`
	if err := os.WriteFile(filepath.Join(pluginDir, "migrations", "001_initial.sql"), []byte(sql), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	err := RunPluginMigrations(dataDir, "testplugin", pluginDir)
	if err != nil {
		t.Fatalf("RunPluginMigrations: %v", err)
	}
	dbPath := filepath.Join(dataDir, "plugin_data", "testplugin.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("plugin db not created: %v", err)
	}
}
