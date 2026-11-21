package store

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

type announcementStore interface {
	Create(p state.SessionParams) *state.Session
	AddMessage(id string, msg provider.Message) error
	SetAnnouncement(id string, announcement *provider.ToolAnnouncement) error
	SetSummary(id string, summary string, messages []provider.Message) error
	Get(id string) (*state.Session, error)
}

func renderAnnouncements(msgs []provider.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = "none"
		if a := m.Announcement; a != nil {
			out[i] = fmt.Sprintf("+%v -%v", a.Added, a.Removed)
		}
	}
	return out
}

func storedAnnouncements(t *testing.T, store announcementStore, id string) []string {
	t.Helper()
	sess, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return renderAnnouncements(sess.Messages)
}

func checkAnnouncementRoundTrip(t *testing.T, store announcementStore) {
	t.Helper()
	store.Create(state.SessionParams{ID: "s1"})
	added := &provider.ToolAnnouncement{Added: []string{"p__a", "p__b"}}
	removed := &provider.ToolAnnouncement{Removed: []string{"p__b"}}
	call := func(id string) []provider.ToolCall {
		return []provider.ToolCall{{ID: id, Name: "p__a", Arguments: map[string]string{}}}
	}
	steps := []struct {
		msg          provider.Message
		announcement *provider.ToolAnnouncement
	}{
		{provider.Message{Role: provider.RoleUser, Content: "u1"}, nil},
		{provider.Message{Role: provider.RoleAssistant, ToolCalls: call("c1")}, nil},
		{provider.Message{Role: provider.RoleTool, ToolCallID: "c1", Content: "r1"}, added},
		{provider.Message{Role: provider.RoleAssistant, Content: "a1"}, nil},
		{provider.Message{Role: provider.RoleUser, Content: "u2"}, nil},
		{provider.Message{Role: provider.RoleAssistant, Content: "a2"}, removed},
		{provider.Message{Role: provider.RoleAssistant, ToolCalls: call("c2")}, nil},
		{provider.Message{Role: provider.RoleTool, ToolCallID: "c2", Content: "r2"}, nil},
	}
	for _, step := range steps {
		if err := store.AddMessage("s1", step.msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
		if step.announcement != nil {
			if err := store.SetAnnouncement("s1", step.announcement); err != nil {
				t.Fatalf("SetAnnouncement: %v", err)
			}
		}
	}
	want := []string{"none", "none", "+[p__a p__b] -[]", "none", "+[] -[p__b]", "none", "none", "none"}
	if got := storedAnnouncements(t, store, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after SetAnnouncement: %q, want %q", got, want)
	}

	sess, err := store.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := store.SetSummary("s1", "summary", sess.Messages); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	if got := storedAnnouncements(t, store, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after SetSummary: %q, want %q", got, want)
	}
	if err := store.SetSummary("s1", "summary", sess.Messages[:5]); err != nil {
		t.Fatalf("SetSummary rollback: %v", err)
	}
	if got := storedAnnouncements(t, store, "s1"); !reflect.DeepEqual(got, want[:5]) {
		t.Errorf("after the rollback: %q, want %q", got, want[:5])
	}
}

func checkAnnouncementSurvivesTrim(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 3, 0)
	store.Create(state.SessionParams{ID: "s1"})
	add := func(role provider.Role, content string) {
		t.Helper()
		if err := store.AddMessage("s1", provider.Message{Role: role, Content: content}); err != nil {
			t.Fatalf("AddMessage %s: %v", content, err)
		}
	}
	add(provider.RoleUser, "u1")
	add(provider.RoleAssistant, "a1")
	add(provider.RoleUser, "u2")
	if err := store.SetAnnouncement("s1", &provider.ToolAnnouncement{Added: []string{"p__a"}}); err != nil {
		t.Fatalf("SetAnnouncement: %v", err)
	}
	add(provider.RoleAssistant, "a2")
	add(provider.RoleUser, "u3")
	sess, err := store.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var contents []string
	for _, m := range sess.Messages {
		contents = append(contents, m.Content)
	}
	if want := []string{"u2", "a2", "u3"}; !reflect.DeepEqual(contents, want) {
		t.Fatalf("messages after the trim = %q, want %q", contents, want)
	}
	if got, want := renderAnnouncements(sess.Messages), []string{"+[p__a] -[]", "none", "none"}; !reflect.DeepEqual(got, want) {
		t.Errorf("announcements after the trim = %q, want %q", got, want)
	}
}

func checkAnnouncementMigration(t *testing.T, db *DB) {
	t.Helper()
	store := NewSessionStore(db, 0, 0)
	store.Create(state.SessionParams{ID: "s1"})
	for _, q := range []string{
		`ALTER TABLE messages DROP COLUMN announcement`,
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (20)`,
	} {
		if _, err := db.SQLDB().Exec(q); err != nil {
			t.Fatalf("set the schema back to 20: %s: %v", q, err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows := []provider.Message{
		{Role: provider.RoleUser, Content: "u1"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "p__a", Arguments: map[string]string{}}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "r1"},
	}
	for i, m := range rows {
		var toolCalls, toolCallID any
		if len(m.ToolCalls) > 0 {
			toolCalls = `[{"id":"c1","name":"p__a","arguments":{}}]`
		}
		if m.ToolCallID != "" {
			toolCallID = m.ToolCallID
		}
		if _, err := db.SQLDB().Exec(
			db.Dialect().Rebind(`INSERT INTO messages (session_id, seq, role, content, tool_calls, tool_call_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`),
			"s1", i+1, string(m.Role), m.Content, toolCalls, toolCallID, now); err != nil {
			t.Fatalf("insert row %d: %v", i+1, err)
		}
	}
	if err := db.runMigrations(); err != nil {
		t.Fatalf("apply 021: %v", err)
	}
	if v, err := db.currentVersion(); err != nil || v != 21 {
		t.Fatalf("schema_version = %d (%v), want 21", v, err)
	}
	sess, err := store.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(sess.Messages, rows) {
		t.Errorf("rows after 021 = %+v, want %+v unchanged and without an announcement", sess.Messages, rows)
	}
}

func TestSessionStore_AnnouncementRoundTrip(t *testing.T) {
	checkAnnouncementRoundTrip(t, NewSessionStore(openTestDB(t), 0, 0))
}

func TestInMemorySessionStore_AnnouncementRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := state.NewSessionStore(dir)
	checkAnnouncementRoundTrip(t, store)
	if err := store.Save("s1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded := state.NewSessionStore(dir)
	if err := loaded.Load("s1"); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := storedAnnouncements(t, loaded, "s1"), storedAnnouncements(t, store, "s1"); !reflect.DeepEqual(got, want) {
		t.Errorf("after Save and Load: %q, want %q", got, want)
	}
}

func TestSessionStore_AnnouncementSurvivesTrim(t *testing.T) {
	checkAnnouncementSurvivesTrim(t, openTestDB(t))
}

func TestSessionStore_AnnouncementMigration(t *testing.T) {
	checkAnnouncementMigration(t, openTestDB(t))
}
