package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/provider"
)

func TestOpenAndMigrations(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var v int
	err = db.SQLDB().QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	if v != 8 {
		t.Errorf("schema_version = %d, want 8", v)
	}

	// Re-open: idempotent, no error
	db2, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open again: %v", err)
	}
	defer func() { _ = db2.Close() }()
	err = db2.SQLDB().QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&v)
	if err != nil {
		t.Fatalf("read schema_version (second open): %v", err)
	}
	if v != 8 {
		t.Errorf("schema_version after re-open = %d, want 8", v)
	}
}

func TestMemoryStore_AddScopedAndMemoriesForContext(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

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
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1", "", "")
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
	_ = db.Close()
	db2, _ := Open(config.DBConfig{}, dir)
	defer func() { _ = db2.Close() }()
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
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 3, 0) // keep last 3
	sessStore.Create("s1", "", "")
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
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1", "", "")
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

func TestSessionStore_NativeToolCallsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1", "", "")

	// Plain user turn — neither column should be populated.
	if err := sessStore.AddMessage("s1", provider.Message{
		Role: provider.RoleUser, Content: "find ticket 42",
	}); err != nil {
		t.Fatalf("AddMessage user: %v", err)
	}

	// Assistant turn with a native tool call.
	assistantCalls := []provider.ToolCall{{
		ID:        "call_abc123",
		Name:      "tickets.show",
		Arguments: map[string]string{"id": "42"},
	}}
	if err := sessStore.AddMessage("s1", provider.Message{
		Role: provider.RoleAssistant, ToolCalls: assistantCalls,
	}); err != nil {
		t.Fatalf("AddMessage assistant tool call: %v", err)
	}

	// Tool reply referencing that call.
	if err := sessStore.AddMessage("s1", provider.Message{
		Role: provider.RoleTool, Content: `{"status":"open"}`, ToolCallID: "call_abc123",
	}); err != nil {
		t.Fatalf("AddMessage tool reply: %v", err)
	}

	sess, err := sessStore.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(sess.Messages))
	}
	if got := sess.Messages[1].ToolCalls; len(got) != 1 || got[0].ID != "call_abc123" ||
		got[0].Name != "tickets.show" || got[0].Arguments["id"] != "42" {
		t.Errorf("assistant ToolCalls round-trip mismatch: %+v", got)
	}
	if sess.Messages[2].ToolCallID != "call_abc123" {
		t.Errorf("tool reply ToolCallID = %q, want %q", sess.Messages[2].ToolCallID, "call_abc123")
	}
	if sess.Messages[2].Content != `{"status":"open"}` {
		t.Errorf("tool reply Content = %q", sess.Messages[2].Content)
	}

	// Direct SQL check: the user turn must have NULL in both new columns
	// (text-based path stays untouched, no "[]" sentinel).
	var toolCalls, toolCallID sql.NullString
	err = db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT tool_calls, tool_call_id FROM messages WHERE session_id = ? AND seq = ?`),
		"s1", 1,
	).Scan(&toolCalls, &toolCallID)
	if err != nil {
		t.Fatalf("read user row: %v", err)
	}
	if toolCalls.Valid {
		t.Errorf("user-turn tool_calls = %q, want NULL", toolCalls.String)
	}
	if toolCallID.Valid {
		t.Errorf("user-turn tool_call_id = %q, want NULL", toolCallID.String)
	}

	// The assistant turn carries tool_calls but not tool_call_id.
	err = db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT tool_calls, tool_call_id FROM messages WHERE session_id = ? AND seq = ?`),
		"s1", 2,
	).Scan(&toolCalls, &toolCallID)
	if err != nil {
		t.Fatalf("read assistant row: %v", err)
	}
	if !toolCalls.Valid {
		t.Error("assistant-turn tool_calls = NULL, want JSON")
	}
	if toolCallID.Valid {
		t.Errorf("assistant-turn tool_call_id = %q, want NULL", toolCallID.String)
	}
}

func TestSessionStore_EmptyToolCallsSlicePersistsAsNull(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1", "", "")

	// Explicit empty (not nil) slice — must still write NULL, not "[]",
	// so consumers can filter for rows with structured tool data via IS NOT NULL.
	if err := sessStore.AddMessage("s1", provider.Message{
		Role: provider.RoleAssistant, Content: "no tools here", ToolCalls: []provider.ToolCall{},
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	var toolCalls sql.NullString
	err = db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT tool_calls FROM messages WHERE session_id = ? AND seq = ?`),
		"s1", 1,
	).Scan(&toolCalls)
	if err != nil {
		t.Fatalf("read row: %v", err)
	}
	if toolCalls.Valid {
		t.Errorf("empty-slice tool_calls = %q, want NULL", toolCalls.String)
	}
}

func TestSessionStore_SetSummaryPreservesToolCalls(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sessStore := NewSessionStore(db, 0, 0)
	sessStore.Create("s1", "", "")

	calls := []provider.ToolCall{{
		ID: "call_x", Name: "items.list", Arguments: map[string]string{"q": "drone"},
	}}
	err = sessStore.SetSummary("s1", "summary", []provider.Message{
		{Role: provider.RoleUser, Content: "find a drone"},
		{Role: provider.RoleAssistant, ToolCalls: calls},
		{Role: provider.RoleTool, Content: `{"items":[]}`, ToolCallID: "call_x"},
	})
	if err != nil {
		t.Fatalf("SetSummary: %v", err)
	}

	sess, err := sessStore.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(sess.Messages))
	}
	if got := sess.Messages[1].ToolCalls; len(got) != 1 || got[0].Name != "items.list" ||
		got[0].Arguments["q"] != "drone" {
		t.Errorf("ToolCalls after SetSummary mismatch: %+v", got)
	}
	if sess.Messages[2].ToolCallID != "call_x" {
		t.Errorf("ToolCallID after SetSummary = %q", sess.Messages[2].ToolCallID)
	}
}

// TestRunPluginMigrations_BinaryPath ensures that passing the directory of the
// plugin binary (filepath.Dir(binaryPath)) finds migrations correctly. Previously
// the binary path itself was passed, causing migrations to be looked up under
// e.g. /plugins/myplugin/myplugin/migrations instead of /plugins/myplugin/migrations.
func TestRunPluginMigrations_BinaryPath(t *testing.T) {
	pluginDir := t.TempDir()
	// Simulate a plugin binary sitting inside pluginDir
	binaryPath := filepath.Join(pluginDir, "opentalon-mcp")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	sql := `CREATE TABLE IF NOT EXISTS mcp_data (id TEXT PRIMARY KEY);`
	if err := os.WriteFile(filepath.Join(pluginDir, "migrations", "001_init.sql"), []byte(sql), 0600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	// Must use filepath.Dir(binaryPath), not binaryPath itself
	if err := RunPluginMigrations(dataDir, "opentalon-mcp", filepath.Dir(binaryPath)); err != nil {
		t.Fatalf("RunPluginMigrations with Dir(binaryPath): %v", err)
	}
	dbPath := filepath.Join(dataDir, "plugin_data", "opentalon-mcp.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("plugin db not created: %v", err)
	}
}

func TestRunPluginMigrations_NoMigrationsDir(t *testing.T) {
	pluginDir := t.TempDir()
	binaryPath := filepath.Join(pluginDir, "myplugin")
	if err := os.WriteFile(binaryPath, []byte("fake binary"), 0700); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	if err := RunPluginMigrations(dataDir, "myplugin", filepath.Dir(binaryPath)); err != nil {
		t.Fatalf("RunPluginMigrations (no migrations dir): %v", err)
	}
	// No migrations dir → no-op: DB must not be created
	dbPath := filepath.Join(dataDir, "plugin_data", "myplugin.db")
	if _, err := os.Stat(dbPath); err == nil {
		t.Error("plugin db should not be created when there are no migrations")
	}
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

func TestUsageStore_TotalTokensSince(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	usage := NewUsageStore(db)
	ctx := context.Background()
	now := time.Now()

	// Record two entries for entity-1 within the window.
	if err := usage.Record(ctx, UsageRecord{
		EntityID: "entity-1", GroupID: "g1", ChannelID: "slack",
		SessionID: "s1", ModelID: "m1", InputTokens: 500, OutputTokens: 300,
	}); err != nil {
		t.Fatalf("Record 1: %v", err)
	}
	if err := usage.Record(ctx, UsageRecord{
		EntityID: "entity-1", GroupID: "g1", ChannelID: "slack",
		SessionID: "s1", ModelID: "m1", InputTokens: 200, OutputTokens: 100,
	}); err != nil {
		t.Fatalf("Record 2: %v", err)
	}
	// Record an entry for a different entity — must not be counted.
	if err := usage.Record(ctx, UsageRecord{
		EntityID: "entity-2", GroupID: "g1", ChannelID: "slack",
		SessionID: "s2", ModelID: "m1", InputTokens: 999, OutputTokens: 999,
	}); err != nil {
		t.Fatalf("Record entity-2: %v", err)
	}

	total, err := usage.TotalTokensSince(ctx, "entity-1", now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("TotalTokensSince: %v", err)
	}
	// 500+300 + 200+100 = 1100
	if total != 1100 {
		t.Errorf("TotalTokensSince = %d, want 1100", total)
	}
}

func TestUsageStore_TotalTokensSince_WindowExcludes(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	usage := NewUsageStore(db)
	ctx := context.Background()

	if err := usage.Record(ctx, UsageRecord{
		EntityID: "entity-1", GroupID: "g1", ChannelID: "slack",
		SessionID: "s1", ModelID: "m1", InputTokens: 400, OutputTokens: 200,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Query with a since time in the future — nothing should be counted.
	total, err := usage.TotalTokensSince(ctx, "entity-1", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("TotalTokensSince: %v", err)
	}
	if total != 0 {
		t.Errorf("TotalTokensSince (future window) = %d, want 0", total)
	}
}

func TestUsageStore_TotalTokensSince_Empty(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	usage := NewUsageStore(db)
	ctx := context.Background()

	total, err := usage.TotalTokensSince(ctx, "nobody", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("TotalTokensSince: %v", err)
	}
	if total != 0 {
		t.Errorf("TotalTokensSince for unknown entity = %d, want 0", total)
	}
}
