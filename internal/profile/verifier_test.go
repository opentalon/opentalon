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
	p, err := v.Verify(context.Background(), "tok1", "", nil)
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
		if _, err := v.Verify(context.Background(), "tok", "", nil); err != nil {
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
	_, err := v.Verify(context.Background(), "bad-token", "", nil)
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
	_, err := v.Verify(context.Background(), "tok", "", nil)
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
	p, err := v.Verify(context.Background(), "U123", "", nil)
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
	_, err := v.Verify(context.Background(), "U123", "", nil)
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
	if _, err := v.Verify(context.Background(), "tok1", "", nil); err != nil {
		t.Fatalf("first Verify: %v", err)
	}

	// Change the env var after construction.
	t.Setenv("TEST_WHOAMI_HEADER", "changed-value")

	// Second call with a different token (bypasses cache) — header must still be "initial-value".
	if _, err := v.Verify(context.Background(), "tok2", "", nil); err != nil {
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

	if _, err := v.Verify(context.Background(), "tok", "", nil); err != nil {
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

	if _, err := v.Verify(context.Background(), "real-token", "", nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	got := <-authValues
	if got != "Bearer real-token" {
		t.Errorf("Authorization header = %q, want %q — extra_header must not overwrite token_header", got, "Bearer real-token")
	}
}

// TestVerifier_ChannelTypeHeader verifies that the channel type is forwarded as a
// request header when ChannelTypeHeader is configured.
func TestVerifier_ChannelTypeHeader(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-Channel-Type")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:               srv.URL,
		ChannelTypeHeader: "X-Channel-Type",
	}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "tok", "slack", nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if got := <-received; got != "slack" {
		t.Errorf("X-Channel-Type = %q, want %q", got, "slack")
	}
}

// TestVerifier_ChannelTypeHeader_NotSentWhenUnconfigured verifies that no
// channel-type header is sent when ChannelTypeHeader is empty.
func TestVerifier_ChannelTypeHeader_NotSentWhenUnconfigured(t *testing.T) {
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("X-Channel-Type")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "tok", "slack", nil); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if got := <-received; got != "" {
		t.Errorf("X-Channel-Type = %q, want empty when ChannelTypeHeader not configured", got)
	}
}

// TestVerifier_LimitFields verifies that limit and limit_time are parsed from the
// WhoAmI response and stored on the profile.
func TestVerifier_LimitFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id":  "u1",
			"group":      "g1",
			"limit":      2000,
			"limit_time": "1h",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Limit != 2000 {
		t.Errorf("Limit = %d, want 2000", p.Limit)
	}
	if p.LimitWindow != time.Hour {
		t.Errorf("LimitWindow = %v, want 1h", p.LimitWindow)
	}
}

// TestVerifier_LimitFields_Missing verifies that absent limit fields result in
// zero values (no enforcement).
func TestVerifier_LimitFields_Missing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Limit != 0 {
		t.Errorf("Limit = %d, want 0 (unlimited)", p.Limit)
	}
	if p.LimitWindow != 0 {
		t.Errorf("LimitWindow = %v, want 0 (unlimited)", p.LimitWindow)
	}
}

// TestVerifier_ChannelTypeResponse verifies that channel_type in the WhoAmI
// response is stored on the profile.
func TestVerifier_ChannelTypeResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id":    "u1",
			"group":        "g1",
			"channel_type": "telegram",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.ChannelType != "telegram" {
		t.Errorf("ChannelType = %q, want telegram", p.ChannelType)
	}
}

// TestVerifier_CredentialHeaders verifies that credential headers returned by WhoAmI
// are parsed and stored on the profile.
func TestVerifier_CredentialHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
			"credentials": map[string]interface{}{
				"myapp": map[string]string{"header": "X-App-User", "value": "user-123"},
				"jira":  map[string]string{"header": "Authorization", "value": "Bearer jira-xyz"},
			},
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(p.Credentials) != 2 {
		t.Fatalf("Credentials len = %d, want 2", len(p.Credentials))
	}
	if c := p.Credentials["myapp"]; c.Header != "X-App-User" || c.Value != "user-123" {
		t.Errorf("Credentials[myapp] = %+v, want {X-App-User user-123}", c)
	}
	if c := p.Credentials["jira"]; c.Header != "Authorization" || c.Value != "Bearer jira-xyz" {
		t.Errorf("Credentials[jira] = %+v, want {Authorization Bearer jira-xyz}", c)
	}
}

// TestVerifier_CredentialHeaders_Missing verifies that an absent credentials field
// results in a nil map (no credentials, no error).
func TestVerifier_CredentialHeaders_Missing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(p.Credentials) != 0 {
		t.Errorf("Credentials = %v, want empty", p.Credentials)
	}
}

// TestVerifier_CredentialHeaders_CustomField verifies that credentials_field can be
// overridden to read from a different JSON key.
func TestVerifier_CredentialHeaders_CustomField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
			"tokens": map[string]interface{}{
				"myapp": map[string]string{"header": "X-App-Token", "value": "custom-tok"},
			},
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL, CredentialsField: "tokens"}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c := p.Credentials["myapp"]; c.Header != "X-App-Token" || c.Value != "custom-tok" {
		t.Errorf("Credentials[myapp] = %+v, want {X-App-Token custom-tok}", c)
	}
}

// TestVerifier_CredentialHeaders_Malformed verifies that an unparseable credentials
// field yields nil credentials with no error — the call succeeds and the bad field
// is silently ignored.
func TestVerifier_CredentialHeaders_Malformed(t *testing.T) {
	cases := []struct {
		name        string
		credentials interface{}
	}{
		{"array", []string{"myapp", "jira"}},
		{"plain strings", map[string]string{"myapp": "token-only"}},
		{"number values", map[string]interface{}{"myapp": 123}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"entity_id":   "u1",
					"group":       "g1",
					"credentials": tc.credentials,
				})
			}))
			defer srv.Close()

			v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
			defer v.Close()

			p, err := v.Verify(context.Background(), "tok", "", nil)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(p.Credentials) != 0 {
				t.Errorf("Credentials = %v, want nil/empty for malformed input", p.Credentials)
			}
		})
	}
}

// TestVerifier_Name verifies that the name field from the WhoAmI response is
// stored on the profile.
func TestVerifier_Name(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
			"name":      "Alex",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Name != "Alex" {
		t.Errorf("Name = %q, want Alex", p.Name)
	}
}

// TestVerifier_Name_Missing verifies that an absent name field results in an
// empty string.
func TestVerifier_Name_Missing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"entity_id": "u1",
			"group":     "g1",
		})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{URL: srv.URL}, nil, nil)
	defer v.Close()

	p, err := v.Verify(context.Background(), "tok", "", nil)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Name != "" {
		t.Errorf("Name = %q, want empty when not provided", p.Name)
	}
}

// TestVerifier_MetadataHeaders_Forwarded verifies that values from the
// inbound metadata map configured under MetadataHeaders are sent as outbound
// HTTP headers on every WhoAmI request.
func TestVerifier_MetadataHeaders_Forwarded(t *testing.T) {
	gotChannelID := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotChannelID <- r.Header.Get("X-Channel-ID")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:             srv.URL,
		MetadataHeaders: map[string]string{"channel_id": "X-Channel-ID"},
	}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "tok", "", map[string]string{"channel_id": "UBOTA"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := <-gotChannelID; got != "UBOTA" {
		t.Errorf("X-Channel-ID = %q, want UBOTA", got)
	}
}

// TestVerifier_MetadataHeaders_CacheKeyIsolation is the load-bearing test for
// the two-bots-one-token use case: the same bearer token must not return a
// cached profile when the configured metadata-header value differs, otherwise
// an admin bot and a customer bot would share permissions.
func TestVerifier_MetadataHeaders_CacheKeyIsolation(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		entity := "user-" + r.Header.Get("X-Channel-ID")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": entity, "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:             srv.URL,
		CacheTTL:        10 * time.Second,
		MetadataHeaders: map[string]string{"channel_id": "X-Channel-ID"},
	}, nil, nil)
	defer v.Close()

	pA, err := v.Verify(context.Background(), "shared-token", "slack", map[string]string{"channel_id": "BOT_A"})
	if err != nil {
		t.Fatalf("Verify A: %v", err)
	}
	pB, err := v.Verify(context.Background(), "shared-token", "slack", map[string]string{"channel_id": "BOT_B"})
	if err != nil {
		t.Fatalf("Verify B: %v", err)
	}
	if pA.EntityID == pB.EntityID {
		t.Fatalf("cache collision across bots: both resolved to %q", pA.EntityID)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (different bots must miss cache)", calls)
	}

	// Same bot twice should hit cache.
	if _, err := v.Verify(context.Background(), "shared-token", "slack", map[string]string{"channel_id": "BOT_A"}); err != nil {
		t.Fatalf("Verify A repeat: %v", err)
	}
	if calls != 2 {
		t.Errorf("server calls after repeat = %d, want still 2 (same bot must hit cache)", calls)
	}
}

// TestVerifier_MetadataHeaders_CollisionDropped verifies that a metadata
// header configured to collide with the token header is dropped at construction
// rather than allowed to silently overwrite the auth header at request time.
func TestVerifier_MetadataHeaders_CollisionDropped(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"entity_id": "u1", "group": "g1"})
	}))
	defer srv.Close()

	v := NewVerifier(VerifierConfig{
		URL:             srv.URL,
		MetadataHeaders: map[string]string{"hijack": "authorization"}, // canonicalizes to Authorization
	}, nil, nil)
	defer v.Close()

	if _, err := v.Verify(context.Background(), "real-token", "", map[string]string{"hijack": "ATTACKER"}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := <-gotAuth; got != "Bearer real-token" {
		t.Errorf("Authorization = %q, want unchanged Bearer real-token (collision should have been dropped)", got)
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
