package orchestrator

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// stubSessionStore is a minimal in-memory session store for testing.
type stubSessionStore struct {
	sessions map[string]*state.Session
	getCalls int
}

func newStubSessionStore() *stubSessionStore {
	return &stubSessionStore{sessions: make(map[string]*state.Session)}
}

func (s *stubSessionStore) Get(id string) (*state.Session, error) {
	s.getCalls++
	sess, ok := s.sessions[id]
	if !ok {
		return nil, &sessionNotFoundError{id}
	}
	// Return a copy to simulate DB behavior.
	cp := *sess
	cp.Messages = make([]provider.Message, len(sess.Messages))
	copy(cp.Messages, sess.Messages)
	return &cp, nil
}

func (s *stubSessionStore) Create(id, entityID, groupID string) *state.Session {
	sess := &state.Session{ID: id, Messages: []provider.Message{}}
	s.sessions[id] = sess
	return sess
}

func (s *stubSessionStore) AddMessage(id string, msg provider.Message) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.Messages = append(sess.Messages, msg)
	return nil
}

func (s *stubSessionStore) SetModel(id string, model provider.ModelRef) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.ActiveModel = model
	return nil
}

func (s *stubSessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.Summary = summary
	sess.Messages = messages
	return nil
}

func (s *stubSessionStore) Delete(id string) error {
	delete(s.sessions, id)
	return nil
}

type sessionNotFoundError struct{ id string }

func (e *sessionNotFoundError) Error() string { return "session not found: " + e.id }

func TestCachedSessionStore_GetCachesOnHit(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create("s1", "", "")
	cached := newCachedSessionStore(stub)

	// First Get should hit the inner store.
	s1, err := cached.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != "s1" {
		t.Fatalf("expected s1, got %s", s1.ID)
	}
	if stub.getCalls != 1 {
		t.Fatalf("expected 1 inner Get call, got %d", stub.getCalls)
	}

	// Second Get should be a cache hit — no additional inner call.
	s2, err := cached.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID != "s1" {
		t.Fatalf("expected s1, got %s", s2.ID)
	}
	if stub.getCalls != 1 {
		t.Fatalf("expected 1 inner Get call after cache hit, got %d", stub.getCalls)
	}
}

func TestCachedSessionStore_AddMessageUpdatesCache(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create("s1", "", "")
	cached := newCachedSessionStore(stub)

	// Populate cache.
	if _, err := cached.Get("s1"); err != nil {
		t.Fatal(err)
	}

	// Add a message.
	msg := provider.Message{Role: provider.RoleUser, Content: "hello"}
	if err := cached.AddMessage("s1", msg); err != nil {
		t.Fatal(err)
	}

	// Get should return updated messages without hitting inner store again.
	beforeCalls := stub.getCalls
	s, err := cached.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if stub.getCalls != beforeCalls {
		t.Fatalf("expected no additional inner Get call, got %d", stub.getCalls-beforeCalls)
	}
	if len(s.Messages) != 1 || s.Messages[0].Content != "hello" {
		t.Fatalf("expected 1 message 'hello', got %v", s.Messages)
	}
}

func TestCachedSessionStore_SetSummaryInvalidatesCache(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create("s1", "", "")
	cached := newCachedSessionStore(stub)

	// Populate cache.
	if _, err := cached.Get("s1"); err != nil {
		t.Fatal(err)
	}

	// SetSummary should invalidate.
	if err := cached.SetSummary("s1", "summary", []provider.Message{{Role: provider.RoleAssistant, Content: "kept"}}); err != nil {
		t.Fatal(err)
	}

	// Next Get should hit inner store again.
	beforeCalls := stub.getCalls
	s, err := cached.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if stub.getCalls <= beforeCalls {
		t.Fatal("expected inner Get call after SetSummary invalidation")
	}
	if s.Summary != "summary" {
		t.Fatalf("expected summary 'summary', got %q", s.Summary)
	}
}

func TestCachedSessionStore_DeleteRemovesFromCache(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create("s1", "", "")
	cached := newCachedSessionStore(stub)

	if _, err := cached.Get("s1"); err != nil {
		t.Fatal(err)
	}

	if err := cached.Delete("s1"); err != nil {
		t.Fatal(err)
	}

	// Get after delete should fail.
	if _, err := cached.Get("s1"); err == nil {
		t.Fatal("expected error after delete")
	}
}
