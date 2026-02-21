package state

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

func TestSessionCreate(t *testing.T) {
	store := NewSessionStore("")
	sess := store.Create("sess1")

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

func TestSessionGetNotFound(t *testing.T) {
	store := NewSessionStore("")
	_, err := store.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestSessionAddMessage(t *testing.T) {
	store := NewSessionStore("")
	store.Create("sess1")

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
	store.Create("sess1")

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
	store.Create("sess1")
	store.Delete("sess1")

	_, err := store.Get("sess1")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestSessionList(t *testing.T) {
	store := NewSessionStore("")
	store.Create("a")
	store.Create("b")
	store.Create("c")

	list := store.List()
	if len(list) != 3 {
		t.Errorf("expected 3 sessions, got %d", len(list))
	}
}

func TestSessionPersistence(t *testing.T) {
	dir := t.TempDir()
	store := NewSessionStore(dir)
	store.Create("sess1")
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
	sess := store.Create("sess1")
	created := sess.UpdatedAt

	_ = store.AddMessage("sess1", provider.Message{Content: "msg"})
	sess, _ = store.Get("sess1")

	if !sess.UpdatedAt.After(created) || sess.UpdatedAt.Equal(created) {
		t.Error("UpdatedAt should advance after AddMessage")
	}
}
