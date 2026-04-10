package profile

import (
	"context"
	"encoding/json"
	"errors"
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
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
}

func TestVerifier_AuthFailed_MissingEntityID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"group": "g1"}) // no entity_id
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	_, err := v.Verify(context.Background(), "tok")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
}

func TestVerifier_ExtraHeaders(t *testing.T) {
	t.Setenv("TEST_WHOAMI_SECRET", "s3cr3t")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-ID") != "U123" {
			http.Error(w, "missing user id", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Security-Token") != "s3cr3t" {
			http.Error(w, "bad security token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "user-123",
			"group":     "team-a",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:         srv.URL,
		TokenHeader: "X-User-ID",
		TokenPrefix: "",
		ExtraHeaders: map[string]string{
			"X-Security-Token": "${TEST_WHOAMI_SECRET}",
		},
	}, nil, nil)
	p, err := v.Verify(context.Background(), "U123")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.EntityID != "user-123" {
		t.Errorf("EntityID = %q, want user-123", p.EntityID)
	}
}

func TestVerifier_ExtraHeaders_WrongSecret(t *testing.T) {
	t.Setenv("TEST_WHOAMI_SECRET", "wrong")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Security-Token") != "s3cr3t" {
			http.Error(w, "bad security token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:          srv.URL,
		TokenHeader:  "X-User-ID",
		TokenPrefix:  "",
		ExtraHeaders: map[string]string{"X-Security-Token": "${TEST_WHOAMI_SECRET}"},
	}, nil, nil)
	_, err := v.Verify(context.Background(), "U123")
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed, got %v", err)
	}
}

// TestVerifier_ExtraHeaders_ExpandedAtConstruction verifies that env vars in
// ExtraHeaders are expanded once when NewVerifier is called, not per-request.
// A mid-run change to the env var must not affect in-flight or cached calls.
func TestVerifier_ExtraHeaders_ExpandedAtConstruction(t *testing.T) {
	t.Setenv("TEST_WHOAMI_HEADER", "initial-value")

	received := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-Snapshot")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:          srv.URL,
		CacheTTL:     10 * time.Second,
		ExtraHeaders: map[string]string{"X-Snapshot": "${TEST_WHOAMI_HEADER}"},
	}, nil, nil)
	defer v.Close()

	// First call — env var is "initial-value" at construction time.
	if _, err := v.Verify(context.Background(), "tok1"); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// Change the env var after construction.
	t.Setenv("TEST_WHOAMI_HEADER", "changed-value")

	// Second call with a different token (bypasses cache) — header must still be "initial-value".
	if _, err := v.Verify(context.Background(), "tok2"); err != nil {
		t.Fatalf("second Verify: %v", err)
	}

	close(received)
	for val := range received {
		if val != "initial-value" {
			t.Errorf("X-Snapshot header = %q, want initial-value (expanded at construction, not per-call)", val)
		}
	}
}

// TestVerifier_ExtraHeaders_UnsetVar verifies that a ${VAR} that resolves to empty
// sends an empty header value (not the literal "${VAR}" string). The warning is
// logged but the request still proceeds.
func TestVerifier_ExtraHeaders_UnsetVar(t *testing.T) {
	// Ensure the var is unset.
	t.Setenv("TEST_WHOAMI_UNSET_VAR", "")

	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-Empty")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:          srv.URL,
		ExtraHeaders: map[string]string{"X-Empty": "${TEST_WHOAMI_UNSET_VAR}"},
	}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "tok"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if val := <-received; val != "" {
		t.Errorf("X-Empty = %q, want empty string when env var is unset", val)
	}
}

// TestVerifier_ExtraHeaders_CollisionWithTokenHeader verifies that an extra_header
// whose canonical name matches token_header is silently dropped (not applied),
// preventing a misconfigured entry from overwriting the auth token.
func TestVerifier_ExtraHeaders_CollisionWithTokenHeader(t *testing.T) {
	authValues := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authValues <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	// "authorization" and "Authorization" are the same canonical header.
	// The extra_header must not overwrite the token.
	v := NewVerifier(VerifierConfig{
		URL: srv.URL,
		ExtraHeaders: map[string]string{
			"authorization": "should-not-overwrite",
		},
	}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "real-token"); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	got := <-authValues
	if got != "Bearer real-token" {
		t.Errorf("Authorization header = %q, want %q — extra_header must not overwrite token_header", got, "Bearer real-token")
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
