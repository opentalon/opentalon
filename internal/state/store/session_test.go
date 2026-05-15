package store

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
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
	store.Create(sid, "entity-X", "group-Y")

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

// TestSessionStore_ClearMessagesIsIdempotent verifies a second ClearMessages
// on an already-empty session is harmless.
func TestSessionStore_ClearMessagesIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)

	const sid = "sess-empty"
	store.Create(sid, "entity-X", "group-Y")

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
