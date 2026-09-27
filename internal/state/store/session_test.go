package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

// TestSessionStore_ClearMessagesPreservesIdentityAndEvents verifies the
// contract of the clear_session command after the entity-stripping fix:
// the conversation history (messages + summary) is wiped, but the session
// row itself — including entity_id, group_id, active_model, metadata, and
// created_at — survives. session_events should also not be touched.
func TestSessionStore_ClearMessagesPreservesIdentityAndEvents(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	const sid = "sess-clear"
	store.Create(state.SessionParams{ID: sid, EntityID: "entity-X", GroupID: "group-Y"})

	if err := store.AddMessage(sid, provider.Message{Role: provider.RoleUser, Content: "first"}); err != nil {
		t.Fatalf("AddMessage[1]: %v", err)
	}
	if err := store.AddMessage(sid, provider.Message{Role: provider.RoleAssistant, Content: "reply"}); err != nil {
		t.Fatalf("AddMessage[2]: %v", err)
	}
	if err := store.SetModel(sid, "anthropic/claude-sonnet-4"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := store.SetMetadata(sid, "debug", "true"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.SetSummary(sid, "prior summary", []provider.Message{
		{Role: provider.RoleUser, Content: "summary-anchor"},
	}); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}

	// Seed the audit log — ClearMessages must NOT touch session_events.
	// This guards against a future refactor that accidentally also wipes
	// the audit trail (Issue #244 policy bullet 3).
	eventStore := NewSessionEventStore(db)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := eventStore.Insert(ctx, SessionEvent{
			SessionID: sid,
			EventType: events.TypeUserMessage,
			Payload: payloadJSON(t, events.UserMessagePayload{
				Header: events.Header{V: 1}, Content: "hi", ContentLength: 2,
			}),
		}); err != nil {
			t.Fatalf("seed event[%d]: %v", i, err)
		}
	}
	eventsBefore, err := eventStore.ListForSession(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("ListForSession before clear: %v", err)
	}
	if len(eventsBefore) != 3 {
		t.Fatalf("seeded %d events, got %d back", 3, len(eventsBefore))
	}

	before, err := store.Get(sid)
	if err != nil {
		t.Fatalf("Get before clear: %v", err)
	}
	createdAt := before.CreatedAt

	if err := store.ClearMessages(sid); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}

	after, err := store.Get(sid)
	if err != nil {
		t.Fatalf("Get after clear (session row should still exist): %v", err)
	}
	if len(after.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0 (history should be dropped)", len(after.Messages))
	}
	if after.Summary != "" {
		t.Errorf("Summary = %q, want empty (summary is derived from messages)", after.Summary)
	}
	if after.ActiveModel != "anthropic/claude-sonnet-4" {
		t.Errorf("ActiveModel = %q, want preserved", after.ActiveModel)
	}
	if after.Metadata["debug"] != "true" {
		t.Errorf("Metadata[debug] = %q, want preserved", after.Metadata["debug"])
	}
	if !after.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt changed: was %v, now %v (session identity must not move)", createdAt, after.CreatedAt)
	}

	// Verify the row still carries the original entity/group association.
	// (Get does not project entity_id/group_id, so query the raw column.)
	d := db.Dialect()
	var entityID, groupID string
	if err := db.SQLDB().QueryRow(
		d.Rebind(`SELECT entity_id, group_id FROM sessions WHERE id = ?`), sid,
	).Scan(&entityID, &groupID); err != nil {
		t.Fatalf("query session row: %v", err)
	}
	if entityID != "entity-X" {
		t.Errorf("entity_id = %q, want entity-X — the original entity-stripping bug", entityID)
	}
	if groupID != "group-Y" {
		t.Errorf("group_id = %q, want group-Y — the original entity-stripping bug", groupID)
	}

	// session_events must survive — clearing the conversation history is a
	// context-management op, not an audit-log erasure (Issue #244 policy).
	eventsAfter, err := eventStore.ListForSession(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("ListForSession after clear: %v", err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Errorf("session_events len = %d after clear, want %d (audit trail must survive)",
			len(eventsAfter), len(eventsBefore))
	}
}

// TestSessionStore_LastUserMessageIDSurvivesTranscriptRewrites pins the
// storage contract the orchestrator's last_user_message_id relies on: the id
// lives in session metadata, so summarisation (SetSummary), clear_session
// (ClearMessages) and writes of other metadata keys (the pending tool call is
// set and cleared on every confirmation) must all leave it in place.
func TestSessionStore_LastUserMessageIDSurvivesTranscriptRewrites(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	const sid = "sess-last-user-message"
	const key = "last_user_message_id" // orchestrator.lastUserMessageIDMetaKey
	store.Create(state.SessionParams{ID: sid})
	if err := store.AddMessage(sid, provider.Message{Role: provider.RoleUser, Content: "first"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.SetMetadata(sid, key, "id-a"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	check := func(step string) {
		t.Helper()
		got, err := store.Get(sid)
		if err != nil {
			t.Fatalf("Get after %s: %v", step, err)
		}
		if got.Metadata[key] != "id-a" {
			t.Errorf("after %s: %s = %q, want id-a", step, key, got.Metadata[key])
		}
	}

	if err := store.SetMetadata(sid, "pending_tool_call", `{"id":"c1"}`); err != nil {
		t.Fatalf("SetMetadata pending: %v", err)
	}
	if err := store.SetMetadata(sid, "pending_tool_call", ""); err != nil {
		t.Fatalf("clear pending: %v", err)
	}
	check("setting and clearing another key")

	if err := store.SetSummary(sid, "summary", nil); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	check("SetSummary")

	if err := store.ClearMessages(sid); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	check("ClearMessages")
}

// TestSessionStore_GetMissingReturnsErrSessionNotFound pins the contract the
// channel handler's session_expired vs internal_error split depends on: an
// absent row must surface as a wrapped state.ErrSessionNotFound so the
// handler can route it to error_code=session_expired. Anything else (e.g.
// dropped DB connection — not exercised here as it needs fault injection)
// must NOT match, so it falls through to internal_error and the client
// keeps its conversation_id across a retry.
func TestSessionStore_GetMissingReturnsErrSessionNotFound(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	_, err := store.Get("nope")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
	if !errors.Is(err, state.ErrSessionNotFound) {
		t.Errorf("err = %v, want errors.Is(err, state.ErrSessionNotFound)", err)
	}
}

// TestSessionStore_ClearMessagesIsIdempotent verifies a second ClearMessages
// on an already-empty session is harmless.
func TestSessionStore_ClearMessagesIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	const sid = "sess-empty"
	store.Create(state.SessionParams{ID: sid, EntityID: "entity-X", GroupID: "group-Y"})

	if err := store.ClearMessages(sid); err != nil {
		t.Fatalf("first ClearMessages: %v", err)
	}
	if err := store.ClearMessages(sid); err != nil {
		t.Fatalf("second ClearMessages: %v", err)
	}
	after, err := store.Get(sid)
	if err != nil {
		t.Fatalf("Get after double clear: %v", err)
	}
	if len(after.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(after.Messages))
	}
}

// sessionSystemSource reads the raw sessions.system_source column, which no
// reader in this repository projects yet (the api-plugin does, one deploy
// later) — so the write path is asserted directly against the column.
func sessionSystemSource(t *testing.T, db *DB, sid string) sql.NullString {
	t.Helper()
	var src sql.NullString
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT system_source FROM sessions WHERE id = ?`), sid,
	).Scan(&src); err != nil {
		t.Fatalf("read sessions.system_source for %q: %v", sid, err)
	}
	return src
}

// TestSessionStore_CreateStoresSystemSourceOnce pins the session-level feature
// label: it is written at creation from the run's profile and never afterwards.
// A second Create for the same id — the idempotent fresh-mint path a
// reconnecting channel takes — must return the existing row untouched rather
// than relabel a live conversation with whatever feature happened to call last.
func TestSessionStore_CreateStoresSystemSourceOnce(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	const sid = "sess-system-source"
	store.Create(state.SessionParams{ID: sid, EntityID: "entity-X", GroupID: "group-Y", Kind: "system", SystemSource: "csv_mapping"})

	src := sessionSystemSource(t, db, sid)
	if !src.Valid || src.String != "csv_mapping" {
		t.Fatalf("system_source after create = %#v, want valid csv_mapping", src)
	}

	// Same id, different label: the row already exists, so the insert conflicts
	// and the original label survives.
	store.Create(state.SessionParams{ID: sid, EntityID: "entity-X", GroupID: "group-Y", Kind: "system", SystemSource: "other_feature"})
	if src := sessionSystemSource(t, db, sid); !src.Valid || src.String != "csv_mapping" {
		t.Errorf("system_source after second create = %#v, want it unchanged at csv_mapping", src)
	}
}

// TestSessionStore_ChatSessionKeepsNullSourceWhenSystemRunInjected guards the
// two-placement split this column exists for: a human chat session opened by
// nobody's feature keeps a NULL session source even when a backend feature
// later injects a run into it — that run's own usage row is what carries the
// feature label. Collapsing the two (deriving the session label from usage)
// would mislabel every injected conversation as belonging to the injecting
// feature and hide it from the customer's own session list.
func TestSessionStore_ChatSessionKeepsNullSourceWhenSystemRunInjected(t *testing.T) {
	db := openTestDB(t)
	sessions := NewSessionStore(db, 0, 0)
	usage := NewUsageStore(db)

	const sid = "sess-chat-injected"
	sessions.Create(state.SessionParams{ID: sid, EntityID: "entity-1", GroupID: "group-1", Kind: "chat"})

	if err := usage.Record(context.Background(), UsageRecord{
		EntityID: "entity-1", GroupID: "group-1", ChannelID: "websocket",
		SessionID: sid, ModelID: "m1", InputTokens: 10, OutputTokens: 5,
		InteractionKind: "system", SystemSource: "job_notify",
	}); err != nil {
		t.Fatalf("Record injected system run: %v", err)
	}

	if src := sessionSystemSource(t, db, sid); src.Valid {
		t.Errorf("session system_source = %q, want NULL (the person opened this session, not the feature)", src.String)
	}

	var usageSource sql.NullString
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT system_source FROM profile_usage WHERE session_id = ?`), sid,
	).Scan(&usageSource); err != nil {
		t.Fatalf("read profile_usage.system_source: %v", err)
	}
	if !usageSource.Valid || usageSource.String != "job_notify" {
		t.Errorf("usage system_source = %#v, want valid job_notify", usageSource)
	}
}

// Migration 016's index is partial: ordinary chats leave the column NULL and
// no reader asks IS NULL through it, so a full index would mostly hold NULLs.
func TestSessionStore_SystemSourceIndexIsPartial(t *testing.T) {
	db := openTestDB(t)

	rows, err := db.SQLDB().Query(`PRAGMA index_list('sessions')`)
	if err != nil {
		t.Fatalf("PRAGMA index_list: %v", err)
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list: %v", err)
		}
		if name != "idx_sessions_system_source" {
			continue
		}
		found = true
		if partial != 1 {
			t.Errorf("idx_sessions_system_source partial = %d, want 1 (WHERE system_source IS NOT NULL)", partial)
		}
	}
	if !found {
		t.Error("idx_sessions_system_source not created by migration 016")
	}
}
