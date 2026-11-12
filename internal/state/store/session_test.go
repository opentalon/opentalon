package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
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

// openedSessionRow reads the raw columns OpenSession writes, which Get does not
// project (kind, source, channel).
type openedSessionRow struct {
	kind, channel       string
	source, origin, key sql.NullString
}

func readOpenedSessionRow(t *testing.T, db *DB, sid string) openedSessionRow {
	t.Helper()
	var r openedSessionRow
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT interaction_kind, channel_id, system_source, opened_by, open_key FROM sessions WHERE id = ?`), sid,
	).Scan(&r.kind, &r.channel, &r.source, &r.origin, &r.key); err != nil {
		t.Fatalf("read session row %q: %v", sid, err)
	}
	return r
}

func countSessionsWithOpenKey(t *testing.T, db *DB, key string) int {
	t.Helper()
	var n int
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT COUNT(*) FROM sessions WHERE open_key = ?`), key,
	).Scan(&n); err != nil {
		t.Fatalf("count sessions with open_key %q: %v", key, err)
	}
	return n
}

// checkOpenSessionCreateOrFind is shared by the SQLite and Postgres tests: the
// first open creates a chat-stamped session with origin, key and channel; the
// same entity, origin and key answer that session (never the id the second
// call minted, never a relabelled row); another key or another entity opens
// another session.
func checkOpenSessionCreateOrFind(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 0, 0)
	first := state.SessionParams{ID: "open-1", EntityID: "user-1", GroupID: "group-1", ChannelID: "websocket", OpenedBy: "origin-a", OpenKey: "event-1"}

	sess, created, err := store.OpenSession(first)
	if err != nil || !created {
		t.Fatalf("first OpenSession = (%v, created=%v, err=%v), want a created session", sess, created, err)
	}
	if sess.ID != "open-1" || sess.OpenedBy != "origin-a" || sess.OpenKey != "event-1" {
		t.Errorf("created session = %+v, want id open-1 with origin-a and event-1", sess)
	}
	row := readOpenedSessionRow(t, db, "open-1")
	if row.kind != "chat" || row.source.Valid || row.channel != "websocket" ||
		row.origin.String != "origin-a" || row.key.String != "event-1" {
		t.Errorf("stored row = %+v, want kind chat, NULL system_source, channel websocket, origin-a, event-1", row)
	}

	again := first
	again.ID, again.GroupID, again.ChannelID = "open-2", "group-2", "http"
	found, created, err := store.OpenSession(again)
	if err != nil || created {
		t.Fatalf("repeated OpenSession = (created=%v, err=%v), want found", created, err)
	}
	if found.ID != "open-1" || found.OpenedBy != "origin-a" || found.OpenKey != "event-1" {
		t.Errorf("found session = %+v, want the stored open-1 with its origin and key", found)
	}
	if row := readOpenedSessionRow(t, db, "open-1"); row.channel != "websocket" {
		t.Errorf("channel after repeat = %q, want websocket unchanged", row.channel)
	}
	if _, err := store.Get("open-2"); !errors.Is(err, state.ErrSessionNotFound) {
		t.Errorf("Get(open-2) err = %v, want ErrSessionNotFound (the repeat minted nothing)", err)
	}

	for _, tc := range []struct {
		name string
		p    state.SessionParams
	}{
		{"another key", state.SessionParams{ID: "open-3", EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-2"}},
		{"another entity", state.SessionParams{ID: "open-4", EntityID: "user-2", OpenedBy: "origin-a", OpenKey: "event-1"}},
	} {
		sess, created, err := store.OpenSession(tc.p)
		if err != nil || !created || sess.ID != tc.p.ID {
			t.Errorf("%s: OpenSession = (%v, created=%v, err=%v), want a new session %s", tc.name, sess, created, err, tc.p.ID)
		}
	}
}

// checkOpenSessionRefusals: a call without origin or key, and an insert that
// conflicts on the minted id rather than the key, answer an error and no
// session — never Create's transient fallback.
func checkOpenSessionRefusals(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 0, 0)
	store.Create(state.SessionParams{ID: "taken", EntityID: "user-1"})

	for _, tc := range []struct {
		name string
		p    state.SessionParams
		want error
	}{
		{"no key", state.SessionParams{ID: "x-1", EntityID: "user-1", OpenedBy: "origin-a"}, state.ErrOpenParams},
		{"no origin", state.SessionParams{ID: "x-2", EntityID: "user-1", OpenKey: "event-1"}, state.ErrOpenParams},
		{"id of another session", state.SessionParams{ID: "taken", EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-9"}, nil},
	} {
		sess, created, err := store.OpenSession(tc.p)
		if err == nil || sess != nil || created {
			t.Errorf("%s: OpenSession = (%v, created=%v, err=%v), want an error and no session", tc.name, sess, created, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if n := countSessionsWithOpenKey(t, db, "event-9"); n != 0 {
		t.Errorf("sessions with event-9 = %d, want 0", n)
	}
}

// checkOpenSessionConcurrent: concurrent opens for one key, each with its own
// minted id, yield one row, one created and the rest found with that row's id.
func checkOpenSessionConcurrent(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 0, 0)
	const n = 8
	type result struct {
		id      string
		created bool
		err     error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess, created, err := store.OpenSession(state.SessionParams{
				ID: fmt.Sprintf("race-%d", i), EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-race",
			})
			results[i] = result{created: created, err: err}
			if sess != nil {
				results[i].id = sess.ID
			}
		}(i)
	}
	wg.Wait()

	createdID, createdCount := "", 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent OpenSession: %v", r.err)
		}
		if r.created {
			createdCount++
			createdID = r.id
		}
	}
	if createdCount != 1 {
		t.Fatalf("created answers = %d, want exactly 1", createdCount)
	}
	for i, r := range results {
		if r.id != createdID {
			t.Errorf("call %d answered %q, want the created session %q", i, r.id, createdID)
		}
	}
	if got := countSessionsWithOpenKey(t, db, "event-race"); got != 1 {
		t.Errorf("rows for the key = %d, want 1", got)
	}
}

// claimStore is what the claim checks need. The DB-backed store and the
// in-memory state.SessionStore both satisfy it, so one check pins both.
type claimStore interface {
	Get(id string) (*state.Session, error)
	OpenSession(p state.SessionParams) (*state.Session, bool, error)
	ClaimOpener(sessionID string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	ReserveNote(sessionID, noteKey string, now time.Time, staleAfter time.Duration) (state.ClaimResult, error)
	AddMessage(id string, msg provider.Message) error
	AddMessageWithMetadata(id string, msg provider.Message, metadata map[string]string) error
	ClearMessages(id string) error
}

// concurrentWinners calls try from n goroutines at once and counts the calls
// that won the claim.
func concurrentWinners(t *testing.T, n int, try func() (state.ClaimResult, error)) int {
	t.Helper()
	var wg sync.WaitGroup
	wins := make([]bool, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := try()
			if err != nil {
				t.Errorf("concurrent call: %v", err)
			}
			wins[i] = result == state.ClaimWon
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, w := range wins {
		if w {
			winners++
		}
	}
	return winners
}

// claimStep calls try and fails the test unless it answers want.
func claimStep(t *testing.T, step string, want state.ClaimResult, try func() (state.ClaimResult, error)) {
	t.Helper()
	got, err := try()
	if err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	if got != want {
		t.Errorf("%s: answered %v, want %v", step, got, want)
	}
}

// countContent is how many of session id's messages carry content.
func countContent(t *testing.T, store claimStore, id, content string) int {
	t.Helper()
	sess, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	n := 0
	for _, m := range sess.Messages {
		if m.Content == content {
			n++
		}
	}
	return n
}

// storeStep stores msg under metadata and fails the test unless the store
// answers want (nil for success, or the error errors.Is must match).
func storeStep(t *testing.T, step string, store claimStore, id string, msg provider.Message, metadata map[string]string, want error) {
	t.Helper()
	err := store.AddMessageWithMetadata(id, msg, metadata)
	if want == nil && err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	if want != nil && !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", step, err, want)
	}
}

// checkClaimOpener: of concurrent claimers exactly one wins; a fresh claim
// refuses; a stale claim is won again while the first turn's input was not
// stored under it, whatever else the session stored meanwhile (the user's own
// turn); the input is stored only under the claim held, once, so a claimant
// whose stale claim another run took over stores nothing; once the input is
// stored the claim is refused for good, also after the transcript is cleared;
// an unknown session is refused.
func checkClaimOpener(t *testing.T, store claimStore) {
	t.Helper()
	sess, _, err := store.OpenSession(state.SessionParams{ID: "claim-1", EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-claim"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	now := time.Now()
	const stale = time.Minute
	claimAt := func(id string, at time.Time) func() (state.ClaimResult, error) {
		return func() (state.ClaimResult, error) { return store.ClaimOpener(id, at, stale) }
	}

	if winners := concurrentWinners(t, 8, claimAt(sess.ID, now)); winners != 1 {
		t.Fatalf("concurrent claim winners = %d, want exactly 1", winners)
	}
	claimStep(t, "fresh claim stands", state.ClaimHeld, claimAt(sess.ID, now.Add(30*time.Second)))
	if err := store.AddMessage(sess.ID, provider.Message{Role: provider.RoleUser, Content: "the user's own turn"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	retaken := now.Add(2 * time.Minute)
	claimStep(t, "stale claim, only another message stored", state.ClaimWon, claimAt(sess.ID, retaken))

	input := provider.Message{Role: provider.RoleUser, Content: "context", Visibility: "hidden"}
	storeStep(t, "input under the claim taken over", store, sess.ID, input, state.TurnClaim{At: now}.Metadata(), state.ErrClaimLost)
	storeStep(t, "input under the claim held", store, sess.ID, input, state.TurnClaim{At: retaken}.Metadata(), nil)
	storeStep(t, "input stored again under that claim", store, sess.ID, input, state.TurnClaim{At: retaken}.Metadata(), state.ErrClaimLost)
	if n := countContent(t, store, sess.ID, "context"); n != 1 {
		t.Errorf("first turn's input stored %d times, want once", n)
	}
	claimStep(t, "stale claim, first turn stored", state.ClaimSettled, claimAt(sess.ID, now.Add(10*time.Minute)))
	if err := store.ClearMessages(sess.ID); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	claimStep(t, "stale claim, transcript cleared", state.ClaimSettled, claimAt(sess.ID, now.Add(20*time.Minute)))
	claimStep(t, "unknown session", state.ClaimSettled, claimAt("no-such-session", now))
}

// checkReserveNote: of concurrent reservations for one key exactly one wins; a
// fresh reservation refuses; another key or another session reserves on its
// own; a stale reservation is won again while its note was not stored under
// it (other messages, other keys' notes do not count); the note is stored only
// under the reservation held, once; once it is stored the key is refused for
// good, also after the transcript is cleared; a note key without its
// reservation is no note. With a db, the note's key is on its row and a second
// note with that key is refused.
func checkReserveNote(t *testing.T, store claimStore, db *DB) {
	t.Helper()
	for _, id := range []string{"note-s1", "note-s2"} {
		if _, _, err := store.OpenSession(state.SessionParams{ID: id, EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-" + id}); err != nil {
			t.Fatalf("OpenSession %s: %v", id, err)
		}
	}
	now := time.Now()
	const stale = time.Minute
	reserveAt := func(sid, key string, at time.Time) func() (state.ClaimResult, error) {
		return func() (state.ClaimResult, error) { return store.ReserveNote(sid, key, at, stale) }
	}

	if winners := concurrentWinners(t, 8, reserveAt("note-s1", "chapter-1", now)); winners != 1 {
		t.Fatalf("concurrent reservation winners = %d, want exactly 1", winners)
	}
	claimStep(t, "fresh reservation stands", state.ClaimHeld, reserveAt("note-s1", "chapter-1", now.Add(30*time.Second)))
	claimStep(t, "another key", state.ClaimWon, reserveAt("note-s1", "chapter-2", now))
	claimStep(t, "another session", state.ClaimWon, reserveAt("note-s2", "chapter-1", now))
	claimStep(t, "no such session", state.ClaimSettled, reserveAt("note-gone", "chapter-1", now))

	if err := store.AddMessage("note-s1", provider.Message{Role: provider.RoleUser, Content: "opener", Visibility: "hidden"}); err != nil {
		t.Fatalf("AddMessage opener: %v", err)
	}
	other := provider.Message{Role: provider.RoleUser, Content: "chapter two", Visibility: "hidden"}
	storeStep(t, "another key's note", store, "note-s1", other, state.TurnClaim{NoteKey: "chapter-2", At: now}.Metadata(), nil)
	retaken := now.Add(2 * time.Minute)
	claimStep(t, "stale, only other messages", state.ClaimWon, reserveAt("note-s1", "chapter-1", retaken))

	note := provider.Message{Role: provider.RoleUser, Content: "chapter note", Visibility: "hidden"}
	storeStep(t, "note under the reservation taken over", store, "note-s1", note, state.TurnClaim{NoteKey: "chapter-1", At: now}.Metadata(), state.ErrClaimLost)
	storeStep(t, "note under the reservation held", store, "note-s1", note, state.TurnClaim{NoteKey: "chapter-1", At: retaken}.Metadata(), nil)
	storeStep(t, "note stored again under that reservation", store, "note-s1", note, state.TurnClaim{NoteKey: "chapter-1", At: retaken}.Metadata(), state.ErrClaimLost)
	if n := countContent(t, store, "note-s1", "chapter note"); n != 1 {
		t.Errorf("note stored %d times, want once", n)
	}
	claimStep(t, "stale, note stored", state.ClaimSettled, reserveAt("note-s1", "chapter-1", now.Add(10*time.Minute)))
	if err := store.ClearMessages("note-s1"); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	claimStep(t, "stale, transcript cleared", state.ClaimSettled, reserveAt("note-s1", "chapter-1", now.Add(20*time.Minute)))
	if err := store.AddMessageWithMetadata("note-s1", note, map[string]string{state.NoteKeyMetadataKey: "chapter-3"}); err == nil {
		t.Error("a note key without its reservation was stored, want an error")
	}

	if db == nil {
		return
	}
	if n := countNoteReservations(t, db, "note-gone"); n != 0 {
		t.Errorf("reservations of a missing session = %d, want none", n)
	}
	storeStep(t, "note-s2's note", store, "note-s2", note, state.TurnClaim{NoteKey: "chapter-1", At: now}.Metadata(), nil)
	var key sql.NullString
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT note_key FROM messages WHERE session_id = ?`), "note-s2",
	).Scan(&key); err != nil {
		t.Fatalf("read messages.note_key: %v", err)
	}
	if key.String != "chapter-1" {
		t.Errorf("messages.note_key = %#v, want chapter-1", key)
	}
	if err := store.AddMessageWithMetadata("note-s2", note, state.TurnClaim{NoteKey: "chapter-1", At: now}.Metadata()); err == nil {
		t.Error("a second note with the same key was stored, want it refused")
	}
}

func countNoteReservations(t *testing.T, db *DB, sid string) int {
	t.Helper()
	var n int
	if err := db.SQLDB().QueryRow(
		db.Dialect().Rebind(`SELECT COUNT(*) FROM session_note_reservations WHERE session_id = ?`), sid,
	).Scan(&n); err != nil {
		t.Fatalf("count reservations of %q: %v", sid, err)
	}
	return n
}

// checkNoteReservationsGoWithSession: deleting a session, directly or by idle
// pruning, removes its note reservations.
func checkNoteReservationsGoWithSession(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 0, 1)
	for _, tc := range []struct {
		name   string
		sid    string
		remove func(sid string) error
	}{
		{"Delete", "gone-deleted", store.Delete},
		{"PruneIdleSessions", "gone-pruned", func(sid string) error {
			if _, err := db.SQLDB().Exec(
				db.Dialect().Rebind(`UPDATE sessions SET updated_at = ? WHERE id = ?`),
				time.Now().AddDate(0, 0, -2).UTC().Format(time.RFC3339), sid); err != nil {
				return err
			}
			return store.PruneIdleSessions()
		}},
	} {
		store.Create(state.SessionParams{ID: tc.sid, EntityID: "user-1"})
		if result, err := store.ReserveNote(tc.sid, "chapter-1", time.Now(), time.Minute); err != nil || result != state.ClaimWon {
			t.Fatalf("%s: ReserveNote = (%v, %v), want reserved", tc.name, result, err)
		}
		if err := tc.remove(tc.sid); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := countNoteReservations(t, db, tc.sid); got != 0 {
			t.Errorf("%s: reservations left = %d, want 0", tc.name, got)
		}
	}
}

// ownerStore is what checkSessionOwner needs; both session stores satisfy it.
type ownerStore interface {
	Create(p state.SessionParams) *state.Session
	OpenSession(p state.SessionParams) (*state.Session, bool, error)
	Get(id string) (*state.Session, error)
}

// checkSessionOwner: a session keeps the entity it was created or opened for,
// and an opened session the channel it was opened on, whatever its key looks
// like, and Get returns them; they, not a prefix of the key, authorise a note
// into the session.
func checkSessionOwner(t *testing.T, store ownerStore) {
	t.Helper()
	store.Create(state.SessionParams{ID: "u1:other:websocket:c1", EntityID: "u1:other"})
	opened := state.SessionParams{ID: "u1:ws:admin:c1", EntityID: "u1", OpenedBy: "origin-a", OpenKey: "event-owner", ChannelID: "ws:admin"}
	for range 2 {
		if sess, _, err := store.OpenSession(opened); err != nil || sess.ChannelID != "ws:admin" {
			t.Fatalf("OpenSession = %+v, %v; want channel ws:admin", sess, err)
		}
	}
	for id, want := range map[string][2]string{"u1:other:websocket:c1": {"u1:other", ""}, "u1:ws:admin:c1": {"u1", "ws:admin"}} {
		sess, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if sess.EntityID != want[0] || sess.ChannelID != want[1] {
			t.Errorf("Get(%s) owner/channel = %q/%q, want %q/%q", id, sess.EntityID, sess.ChannelID, want[0], want[1])
		}
	}
}

func TestSessionStore_SessionOwner(t *testing.T) {
	checkSessionOwner(t, NewSessionStore(openTestDB(t), 0, 0))
}

func TestInMemorySessionStore_SessionOwner(t *testing.T) {
	checkSessionOwner(t, state.NewSessionStore(""))
}

func TestSessionStore_OpenSessionCreateOrFind(t *testing.T) {
	checkOpenSessionCreateOrFind(t, openTestDB(t))
}

func TestSessionStore_OpenSessionRefusals(t *testing.T) {
	checkOpenSessionRefusals(t, openTestDB(t))
}

func TestSessionStore_OpenSessionConcurrent(t *testing.T) {
	checkOpenSessionConcurrent(t, openTestDB(t))
}

func TestSessionStore_ClaimOpener(t *testing.T) {
	checkClaimOpener(t, NewSessionStore(openTestDB(t), 0, 0))
}

func TestSessionStore_ReserveNote(t *testing.T) {
	db := openTestDB(t)
	checkReserveNote(t, NewSessionStore(db, 0, 0), db)
}

func TestInMemorySessionStore_ClaimOpener(t *testing.T) {
	checkClaimOpener(t, state.NewSessionStore(""))
}

func TestInMemorySessionStore_ReserveNote(t *testing.T) {
	checkReserveNote(t, state.NewSessionStore(""), nil)
}

// Deleting an in-memory session forgets its open identity, its claim and its
// reservations: a session later created under the same id answers only for
// its own key and claims afresh.
func TestInMemorySessionStore_DeleteForgetsOpenedSession(t *testing.T) {
	store := state.NewSessionStore("")
	now := time.Now()
	open := func(id, key string) (*state.Session, bool, error) {
		return store.OpenSession(state.SessionParams{ID: id, EntityID: "user-1", OpenedBy: "origin-a", OpenKey: key})
	}
	if _, _, err := open("s", "key-a"); err != nil {
		t.Fatalf("OpenSession key-a: %v", err)
	}
	claimStep(t, "first claim", state.ClaimWon, func() (state.ClaimResult, error) { return store.ClaimOpener("s", now, time.Minute) })
	claimStep(t, "first reservation", state.ClaimWon, func() (state.ClaimResult, error) { return store.ReserveNote("s", "note-1", now, time.Minute) })
	if err := store.Delete("s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, created, err := open("s", "key-b"); err != nil || !created {
		t.Fatalf("OpenSession key-b under the old id = (created=%v, err=%v), want created", created, err)
	}
	claimStep(t, "claim after delete", state.ClaimWon, func() (state.ClaimResult, error) { return store.ClaimOpener("s", now, time.Minute) })
	claimStep(t, "reservation after delete", state.ClaimWon, func() (state.ClaimResult, error) { return store.ReserveNote("s", "note-1", now, time.Minute) })
	if sess, created, err := open("t", "key-a"); err != nil || !created || sess.ID != "t" {
		t.Errorf("OpenSession key-a after delete = (%v, created=%v, err=%v), want a new session t", sess, created, err)
	}
}

func TestSessionStore_NoteReservationsGoWithSession(t *testing.T) {
	checkNoteReservationsGoWithSession(t, openTestDB(t))
}

// A persistence failure answers an error: no session (never a transient one),
// and never "claim lost" or "note already reserved".
func TestSessionStore_PersistenceFailureIsAnError(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionStore(db, 0, 0)
	_ = db.Close()

	sess, created, err := store.OpenSession(state.SessionParams{ID: "down-1", EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-1"})
	if err == nil || sess != nil || created {
		t.Errorf("OpenSession on a closed DB = (%v, created=%v, err=%v), want an error and no session", sess, created, err)
	}
	for name, call := range map[string]func() (state.ClaimResult, error){
		"ClaimOpener": func() (state.ClaimResult, error) { return store.ClaimOpener("down-1", time.Now(), time.Minute) },
		"ReserveNote": func() (state.ClaimResult, error) {
			return store.ReserveNote("down-1", "chapter-1", time.Now(), time.Minute)
		},
	} {
		if result, err := call(); err == nil || result == state.ClaimWon {
			t.Errorf("%s on a closed DB = (%v, %v), want an error", name, result, err)
		}
	}
}

// The key is durable: after a restart the same key still finds its session.
func TestSessionStore_OpenSessionFoundAfterRestart(t *testing.T) {
	dir := t.TempDir()
	p := state.SessionParams{ID: "before-restart", EntityID: "user-1", OpenedBy: "origin-a", OpenKey: "event-1"}

	db, err := Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, created, err := NewSessionStore(db, 0, 0).OpenSession(p); err != nil || !created {
		t.Fatalf("OpenSession before restart: created=%v err=%v", created, err)
	}
	_ = db.Close()

	db, err = Open(config.DBConfig{}, dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p.ID = "after-restart"
	sess, created, err := NewSessionStore(db, 0, 0).OpenSession(p)
	if err != nil || created || sess.ID != "before-restart" {
		t.Errorf("OpenSession after restart = (%v, created=%v, err=%v), want found before-restart", sess, created, err)
	}
}

// Migration 018's indexes are unique and partial: ordinary sessions and
// messages leave the key NULL and must never collide on it.
func TestSessionStore_OpenKeyIndexesAreUniqueAndPartial(t *testing.T) {
	db := openTestDB(t)
	for _, tc := range []struct{ table, index string }{
		{"sessions", "idx_sessions_open_key"},
		{"messages", "idx_messages_note_key"},
	} {
		rows, err := db.SQLDB().Query(fmt.Sprintf(`PRAGMA index_list('%s')`, tc.table))
		if err != nil {
			t.Fatalf("PRAGMA index_list(%s): %v", tc.table, err)
		}
		found := false
		for rows.Next() {
			var seq, unique, partial int
			var name, origin string
			if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatalf("scan index_list: %v", err)
			}
			if name == tc.index {
				found = true
				if unique != 1 || partial != 1 {
					t.Errorf("%s: unique=%d partial=%d, want 1 and 1", tc.index, unique, partial)
				}
			}
		}
		_ = rows.Close()
		if !found {
			t.Errorf("%s not created by migration 018", tc.index)
		}
	}
}
