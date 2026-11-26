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
	// labels Create was told per id, so the cached wrapper's forwarding of
	// the write-once session labels is observable.
	created map[string][2]string
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

func (s *stubSessionStore) Create(p state.SessionParams) *state.Session {
	sess := &state.Session{ID: p.ID, Messages: []provider.Message{}}
	s.sessions[p.ID] = sess
	if s.created == nil {
		s.created = map[string][2]string{}
	}
	s.created[p.ID] = [2]string{p.Kind, p.SystemSource}
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

func (s *stubSessionStore) AddMessageWithMetadata(id string, msg provider.Message, _ map[string]string) error {
	return s.AddMessage(id, msg)
}

func (s *stubSessionStore) SetModel(id string, model provider.ModelRef) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.ActiveModel = model
	return nil
}

func (s *stubSessionStore) TruncateMessages(id string, n int) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.Messages = sess.Messages[:min(n, len(sess.Messages))]
	return nil
}

func (s *stubSessionStore) SetTitle(id, title string) error {
	sess := s.sessions[id]
	if sess == nil {
		return nil
	}
	sess.Title = title
	return nil
}

func (s *stubSessionStore) SetMetadata(id, key, value string) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	if sess.Metadata == nil {
		sess.Metadata = map[string]string{}
	}
	if value == "" {
		delete(sess.Metadata, key)
	} else {
		sess.Metadata[key] = value
	}
	return nil
}

func (s *stubSessionStore) ClearMessages(id string) error {
	sess := s.sessions[id]
	if sess == nil {
		return &sessionNotFoundError{id}
	}
	sess.Messages = nil
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
	stub.Create(state.SessionParams{ID: "s1"})
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
	stub.Create(state.SessionParams{ID: "s1"})
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

func TestCachedSessionStore_TruncateMessagesReachesCacheAndInner(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create(state.SessionParams{ID: "s1"})
	cached := newCachedSessionStore(stub)
	for _, c := range []string{"a", "b", "c"} {
		if err := cached.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: c}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cached.TruncateMessages("s1", 1); err != nil {
		t.Fatal(err)
	}
	if err := cached.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "d"}); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func(string) (*state.Session, error){"cache": cached.Get, "inner": stub.Get} {
		s, err := get("s1")
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Messages) != 2 || s.Messages[0].Content != "a" || s.Messages[1].Content != "d" {
			t.Errorf("%s messages = %+v, want a then d", name, s.Messages)
		}
	}
}

func TestCachedSessionStore_DeleteRemovesFromCache(t *testing.T) {
	stub := newStubSessionStore()
	stub.Create(state.SessionParams{ID: "s1"})
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

// TestCachedStore_PromptTypeReachesCache pins the fix for the half-effective
// confirmation stripping: DURING a turn the LLM context is built from the
// request-scoped cache, so the prompt_type marker must ride on the CACHED
// message too — not only on the DB row that a later fresh load would surface.
// Without this, the continuation right after an approval still replayed the
// narrated prompt + "y" to the model.
func TestCachedStore_PromptTypeReachesCache(t *testing.T) {
	inner := newStubSessionStore()
	c := newCachedSessionStore(inner)
	// Create on the INNER store and pull via Get — the cache then holds its own
	// copy (the stub, like the in-memory store, appends to the same object it
	// hands out, so caching Create's return value would double-append here).
	inner.Create(state.SessionParams{ID: "s1", EntityID: "e", GroupID: "g", Kind: "chat"})
	if _, err := c.Get("s1"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	err := c.AddMessageWithMetadata("s1",
		provider.Message{Role: provider.RoleAssistant, Content: "This will create Box A. Proceed?"},
		toolConfirmationFrameMetadata("call-1"))
	if err != nil {
		t.Fatalf("AddMessageWithMetadata: %v", err)
	}
	err = c.AddMessageWithMetadata("s1",
		provider.Message{Role: provider.RoleUser, Content: "y"},
		confirmationReplyMetadata("approve"))
	if err != nil {
		t.Fatalf("AddMessageWithMetadata reply: %v", err)
	}

	sess, err := c.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if n := len(sess.Messages); n != 2 {
		t.Fatalf("want 2 cached messages, got %d", n)
	}
	if got := sess.Messages[0].PromptType; got != provider.PromptTypeToolConfirmation {
		t.Fatalf("cached prompt row PromptType = %q, want %q", got, provider.PromptTypeToolConfirmation)
	}
	if got := sess.Messages[1].PromptType; got != provider.PromptTypeConfirmationResponse {
		t.Fatalf("cached reply row PromptType = %q, want %q", got, provider.PromptTypeConfirmationResponse)
	}
	if got := stripApprovedConfirmationExchanges(sess.Messages); len(got) != 0 {
		t.Fatalf("resolved exchange must strip from cached history, %d rows survived", len(got))
	}
}

// The cache wraps Create without looking at the labels; it must still hand
// both to the inner store, or a system session minted through the cache
// would land as an unlabelled chat.
func TestCachedSessionStore_CreateForwardsLabels(t *testing.T) {
	stub := newStubSessionStore()
	cached := newCachedSessionStore(stub)

	cached.Create(state.SessionParams{ID: "s-sys", EntityID: "e1", GroupID: "g1", Kind: "system", SystemSource: "csv_mapping"})

	if got := stub.created["s-sys"]; got != [2]string{"system", "csv_mapping"} {
		t.Errorf("inner Create got (kind, source) = %v, want (system, csv_mapping)", got)
	}
}
