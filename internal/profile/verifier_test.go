package profile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifier_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "user-123",
			"group":     "team-a",
			"plugins":   []string{"jira", "github"},
		})
	}))
	defer srv.Close()

	savedGroups := map[string][]string{}
	saver := &stubGroupSaver{saved: savedGroups}

	v := NewVerifier(VerifierConfig{URL: srv.URL, CacheTTL: 100 * time.Millisecond}, saver, nil)
	p, err := v.Verify(context.Background(), "tok1")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.EntityID != "user-123" {
		t.Errorf("EntityID = %q, want user-123", p.EntityID)
	}
	if p.Group != "team-a" {
		t.Errorf("Group = %q, want team-a", p.Group)
	}
	if len(p.Plugins) != 2 {
		t.Errorf("Plugins len = %d, want 2", len(p.Plugins))
	}
	// Auto-save should have been called.
	if got := savedGroups["team-a"]; len(got) != 2 {
		t.Errorf("auto-save: got %v, want [jira github]", got)
	}
}

func TestVerifier_CacheHit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL, CacheTTL: 10 * time.Second}, nil, nil)
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(context.Background(), "tok"); err != nil {
			t.Fatalf("Verify %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (cache should serve subsequent)", calls)
	}
}

func TestVerifier_AuthFailed_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	_, err := v.Verify(context.Background(), "bad-token")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestVerifier_AuthFailed_MissingEntityID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"group": "g1"}) // no entity_id
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	_, err := v.Verify(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected error for missing entity_id")
	}
}

// stubGroupSaver records calls to UpsertGroupPlugins for test assertions.
type stubGroupSaver struct {
	saved map[string][]string
}

func (s *stubGroupSaver) UpsertGroupPlugins(_ context.Context, groupID string, pluginIDs []string, _ string) error {
	s.saved[groupID] = append(s.saved[groupID], pluginIDs...)
	return nil
}
