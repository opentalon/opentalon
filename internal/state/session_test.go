package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func TestSessionCreate(t *testing.T) {
	store := NewSessionStore("")
	sess := store.Create(SessionParams{ID: "sess1"})

	if sess.ID != "sess1" {
		t.Errorf("ID = %q, want sess1", sess.ID)
	}
	if len(sess.Messages) != 0 {
		t.Errorf("Messages should be empty, got %d", len(sess.Messages))
	}
	if sess.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
}

func TestSessionCreateIdempotent(t *testing.T) {
	// A second Create with the same id must return the existing session, not
	// overwrite it. Channels with stale conversation_ids must never silently
	// wipe live message history.
	store := NewSessionStore("")
	first := store.Create(SessionParams{ID: "sess1"})
	_ = store.AddMessage("sess1", provider.Message{Role: provider.RoleUser, Content: "alive"})

	again := store.Create(SessionParams{ID: "sess1"})
	if again != first {
		t.Errorf("Create returned a different pointer on second call; expected idempotent reuse")
	}
	got, _ := store.Get("sess1")
	if len(got.Messages) != 1 || got.Messages[0].Content != "alive" {
		t.Errorf("messages were wiped by second Create: got %#v", got.Messages)
	}
}

func TestSessionGetNotFound(t *testing.T) {
	store := NewSessionStore("")
	_, err := store.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
	// The handler's session_expired vs internal_error branch hinges on
	// errors.Is(err, ErrSessionNotFound). Verify the contract holds for
	// the in-memory store; the DB-backed store has its own coverage.
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want errors.Is(err, ErrSessionNotFound)", err)
	}
}

func TestSessionAddMessage(t *testing.T) {
	store := NewSessionStore("")
	store.Create(SessionParams{ID: "sess1"})

	err := store.AddMessage("sess1", provider.Message{
		Role:    provider.RoleUser,
		Content: "Hello",
	})
	if err != nil {
		t.Fatal(err)
	}

	sess, _ := store.Get("sess1")
	if len(sess.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sess.Messages))
	}
	if sess.Messages[0].Content != "Hello" {
		t.Errorf("content = %q, want Hello", sess.Messages[0].Content)
	}
}

func TestSessionAddMessageNotFound(t *testing.T) {
	store := NewSessionStore("")
	err := store.AddMessage("nonexistent", provider.Message{Content: "test"})
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionSetModel(t *testing.T) {
	store := NewSessionStore("")
	store.Create(SessionParams{ID: "sess1"})

	err := store.SetModel("sess1", "anthropic/claude-sonnet-4")
	if err != nil {
		t.Fatal(err)
	}

	sess, _ := store.Get("sess1")
	if sess.ActiveModel != "anthropic/claude-sonnet-4" {
		t.Errorf("ActiveModel = %q, want anthropic/claude-sonnet-4", sess.ActiveModel)
	}
}

func TestSessionDelete(t *testing.T) {
	store := NewSessionStore("")
	store.Create(SessionParams{ID: "sess1"})
	if err := store.Delete("sess1"); err != nil {
		t.Fatal(err)
	}

	_, err := store.Get("sess1")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestSessionClearMessagesPreservesIdentity(t *testing.T) {
	store := NewSessionStore("")
	sess := store.Create(SessionParams{ID: "sess1"})
	sess.Metadata["debug"] = "true"
	_ = store.SetModel("sess1", "anthropic/claude-sonnet-4")
	_ = store.AddMessage("sess1", provider.Message{Role: provider.RoleUser, Content: "Hello"})
	_ = store.AddMessage("sess1", provider.Message{Role: provider.RoleAssistant, Content: "Hi"})
	_ = store.SetSummary("sess1", "prior summary", []provider.Message{{Role: provider.RoleUser, Content: "kept"}})
	createdAt := sess.CreatedAt

	if err := store.ClearMessages("sess1"); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}

	got, err := store.Get("sess1")
	if err != nil {
		t.Fatalf("session disappeared after clear: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(got.Messages))
	}
	if got.Summary != "" {
		t.Errorf("Summary = %q, want empty", got.Summary)
	}
	if got.ActiveModel != "anthropic/claude-sonnet-4" {
		t.Errorf("ActiveModel = %q, want preserved", got.ActiveModel)
	}
	if got.Metadata["debug"] != "true" {
		t.Errorf("Metadata[debug] = %q, want preserved", got.Metadata["debug"])
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt changed: was %v, now %v", createdAt, got.CreatedAt)
	}
}

func TestSessionClearMessagesUnknownSessionIsNoOp(t *testing.T) {
	store := NewSessionStore("")
	if err := store.ClearMessages("ghost"); err != nil {
		t.Errorf("ClearMessages on unknown id should be a no-op, got: %v", err)
	}
}

func TestSessionList(t *testing.T) {
	store := NewSessionStore("")
	store.Create(SessionParams{ID: "a"})
	store.Create(SessionParams{ID: "b"})
	store.Create(SessionParams{ID: "c"})

	list := store.List()
	if len(list) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(list))
	}
}

func TestSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(dir)
	store.Create(SessionParams{ID: "sess1"})
	_ = store.AddMessage("sess1", provider.Message{Role: provider.RoleUser, Content: "Hello"})
	_ = store.AddMessage("sess1", provider.Message{Role: provider.RoleAssistant, Content: "Hi there"})
	_ = store.SetModel("sess1", "anthropic/claude-haiku-4")

	if err := store.Save("sess1"); err != nil {
		t.Fatal(err)
	}

	loaded := NewSessionStore(dir)
	if err := loaded.Load("sess1"); err != nil {
		t.Fatal(err)
	}

	sess, err := loaded.Get("sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(sess.Messages))
	}
	if sess.Messages[0].Content != "Hello" {
		t.Errorf("first message = %q, want Hello", sess.Messages[0].Content)
	}
	if sess.ActiveModel != "anthropic/claude-haiku-4" {
		t.Errorf("ActiveModel = %q, want anthropic/claude-haiku-4", sess.ActiveModel)
	}
}

func TestSessionUpdatedAtChanges(t *testing.T) {
	store := NewSessionStore("")
	sess := store.Create(SessionParams{ID: "sess1"})
	created := sess.UpdatedAt

	_ = store.AddMessage("sess1", provider.Message{Content: "msg"})
	sess, _ = store.Get("sess1")

	if !sess.UpdatedAt.After(created) || sess.UpdatedAt.Equal(created) {
		t.Error("UpdatedAt should advance after AddMessage")
	}
}

func TestSessionSetModelNotFound(t *testing.T) {
	store := NewSessionStore("")
	err := store.SetModel("nonexistent", "anthropic/haiku")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionSaveNotFound(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	err := store.Save("nonexistent")
	if err == nil {
		t.Error("expected error for saving nonexistent session")
	}
}

func TestSessionLoadNonexistent(t *testing.T) {
	store := NewSessionStore(t.TempDir())
	err := store.Load("nonexistent")
	if err == nil {
		t.Error("expected error for loading nonexistent session file")
	}
}

func TestSessionMetadata(t *testing.T) {
	store := NewSessionStore("")
	sess := store.Create(SessionParams{ID: "sess1"})
	sess.Metadata["user_id"] = "u123"
	sess.Metadata["mode"] = "debug"

	got, _ := store.Get("sess1")
	if got.Metadata["user_id"] != "u123" {
		t.Errorf("metadata user_id = %q", got.Metadata["user_id"])
	}
}

// TestSessionMarkSetWhileTheSessionIsRead: SetVerifiedOwner runs outside the
// turn lock, so it may run while a turn copies the whole session Get answered
// (the orchestrator's request cache does), while the session is saved, listed
// or opened again. Under -race a reader fails here while it reads the live
// session outside the store's lock.
func TestSessionMarkSetWhileTheSessionIsRead(t *testing.T) {
	opened := SessionParams{ID: "u1:websocket:o1", EntityID: "u1", OpenedBy: "origin-a", OpenKey: "event-1"}
	for _, tc := range []struct {
		name string
		read func(t *testing.T, store *SessionStore) []Session
	}{
		{"Get", func(t *testing.T, store *SessionStore) []Session {
			sess, err := store.Get(opened.ID)
			if err != nil {
				t.Errorf("Get: %v", err)
				return nil
			}
			return []Session{*sess}
		}},
		{"Save", func(t *testing.T, store *SessionStore) []Session {
			if err := store.Save(opened.ID); err != nil {
				t.Errorf("Save: %v", err)
			}
			return nil
		}},
		{"List", func(t *testing.T, store *SessionStore) []Session {
			var copies []Session
			for _, sess := range store.List() {
				copies = append(copies, *sess)
			}
			return copies
		}},
		{"OpenSession", func(t *testing.T, store *SessionStore) []Session {
			sess, _, err := store.OpenSession(context.Background(), opened)
			if err != nil {
				t.Errorf("OpenSession: %v", err)
				return nil
			}
			return []Session{*sess}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewSessionStore(t.TempDir())
			if _, _, err := store.OpenSession(context.Background(), opened); err != nil {
				t.Fatalf("OpenSession: %v", err)
			}
			const rounds = 200
			var copies []Session
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i := range rounds {
					owner := VerifiedOwner{EntityID: "u1", GroupID: fmt.Sprintf("g%d", i), ChannelID: "websocket"}
					if err := store.SetVerifiedOwner(context.Background(), opened.ID, owner); err != nil {
						t.Errorf("SetVerifiedOwner: %v", err)
						return
					}
				}
			}()
			go func() {
				defer wg.Done()
				for range rounds {
					copies = append(copies, tc.read(t, store)...)
				}
			}()
			wg.Wait()
			for _, sess := range copies {
				if sess.ID != opened.ID {
					t.Fatalf("read session %q, want %q", sess.ID, opened.ID)
				}
			}
		})
	}
}

// TestSessionGetAnswersACopy: changing what Get, OpenSession or List answered
// changes nothing in the store; the store's own writes do.
func TestSessionGetAnswersACopy(t *testing.T) {
	store := NewSessionStore("")
	opened := SessionParams{ID: "u1:websocket:o1", EntityID: "u1", OpenedBy: "origin-a", OpenKey: "event-1"}
	if _, _, err := store.OpenSession(context.Background(), opened); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	_ = store.AddMessage(opened.ID, provider.Message{Role: provider.RoleUser, Content: "kept"})
	_ = store.SetMetadata(opened.ID, "debug", "true")
	owner := VerifiedOwner{EntityID: "u1", GroupID: "g1", ChannelID: "websocket"}
	_ = store.SetVerifiedOwner(context.Background(), opened.ID, owner)

	got, _ := store.Get(opened.ID)
	again, _, _ := store.OpenSession(context.Background(), opened)
	answered := append([]*Session{got, again}, store.List()...)
	for _, sess := range answered {
		sess.Messages[0].Content = "changed"
		sess.Messages = append(sess.Messages, provider.Message{Content: "appended"})
		sess.Metadata["debug"] = "false"
		sess.VerifiedOwner.GroupID = "g2"
		sess.Title = "changed"
	}

	after, _ := store.Get(opened.ID)
	if len(after.Messages) != 1 || after.Messages[0].Content != "kept" {
		t.Errorf("Messages = %+v, want only the stored one", after.Messages)
	}
	if after.Metadata["debug"] != "true" {
		t.Errorf("Metadata[debug] = %q, want true", after.Metadata["debug"])
	}
	if after.VerifiedOwner == nil || *after.VerifiedOwner != owner {
		t.Errorf("mark = %+v, want %+v", after.VerifiedOwner, owner)
	}
	if after.Title != "" {
		t.Errorf("Title = %q, want none", after.Title)
	}
}

func TestSessionMultipleMessages(t *testing.T) {
	store := NewSessionStore("")
	store.Create(SessionParams{ID: "sess1"})
	for i := 0; i < 50; i++ {
		_ = store.AddMessage("sess1", provider.Message{
			Role:    provider.RoleUser,
			Content: "msg",
		})
	}
	sess, _ := store.Get("sess1")
	if len(sess.Messages) != 50 {
		t.Errorf("expected 50 messages, got %d", len(sess.Messages))
	}
}
