package auth

import (
	"testing"
	"time"
)

func TestProfileAvailability(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		profile   Profile
		available bool
	}{
		{
			name:      "fresh profile",
			profile:   Profile{ID: "test"},
			available: true,
		},
		{
			name: "in cooldown",
			profile: Profile{
				ID:    "test",
				Stats: UsageStats{CooldownUntil: now.Add(time.Hour)},
			},
			available: false,
		},
		{
			name: "cooldown expired",
			profile: Profile{
				ID:    "test",
				Stats: UsageStats{CooldownUntil: now.Add(-time.Hour)},
			},
			available: true,
		},
		{
			name: "disabled",
			profile: Profile{
				ID:    "test",
				Stats: UsageStats{DisabledUntil: now.Add(24 * time.Hour)},
			},
			available: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.profile.Available(now); got != tt.available {
				t.Errorf("Available() = %v, want %v", got, tt.available)
			}
		})
	}
}

func TestCooldownExponentialBackoff(t *testing.T) {
	cfg := CooldownConfig{Initial: time.Minute, Max: time.Hour, Multiplier: 5}
	tracker := NewCooldownTracker(cfg)
	now := time.Now()

	p := &Profile{ID: "test"}

	tracker.PutInCooldown(p, now)
	d1 := p.Stats.CooldownUntil.Sub(now)
	if d1 != time.Minute {
		t.Errorf("1st cooldown = %v, want 1m", d1)
	}

	tracker.PutInCooldown(p, now)
	d2 := p.Stats.CooldownUntil.Sub(now)
	if d2 != 5*time.Minute {
		t.Errorf("2nd cooldown = %v, want 5m", d2)
	}

	tracker.PutInCooldown(p, now)
	d3 := p.Stats.CooldownUntil.Sub(now)
	if d3 != 25*time.Minute {
		t.Errorf("3rd cooldown = %v, want 25m", d3)
	}
}

func TestCooldownCap(t *testing.T) {
	cfg := CooldownConfig{Initial: time.Minute, Max: 30 * time.Minute, Multiplier: 5}
	tracker := NewCooldownTracker(cfg)
	now := time.Now()

	p := &Profile{ID: "test"}
	for i := 0; i < 10; i++ {
		tracker.PutInCooldown(p, now)
	}

	d := p.Stats.CooldownUntil.Sub(now)
	if d > 30*time.Minute {
		t.Errorf("cooldown %v exceeded cap 30m", d)
	}
}

func TestCooldownReset(t *testing.T) {
	cfg := DefaultCooldownConfig()
	tracker := NewCooldownTracker(cfg)
	now := time.Now()

	p := &Profile{ID: "test"}
	tracker.PutInCooldown(p, now)
	if p.Available(now) {
		t.Error("should be in cooldown")
	}

	tracker.Reset(p)
	if !p.Available(now) {
		t.Error("should be available after reset")
	}
	if p.Stats.ErrorCount != 0 {
		t.Errorf("error count = %d, want 0", p.Stats.ErrorCount)
	}
}

func TestRotatorSelectOldestFirst(t *testing.T) {
	store := NewStore("")
	now := time.Now()

	store.Add(&Profile{
		ID: "a", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: now.Add(-time.Hour)},
	})
	store.Add(&Profile{
		ID: "b", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: now.Add(-2 * time.Hour)},
	})

	rotator := NewRotator(store)
	selected, err := rotator.Select("anthropic", "sess1", now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "b" {
		t.Errorf("expected oldest profile 'b', got %q", selected.ID)
	}
}

func TestRotatorPrefersOAuth(t *testing.T) {
	store := NewStore("")
	now := time.Now()
	base := now.Add(-time.Hour)

	store.Add(&Profile{
		ID: "apikey", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: base},
	})
	store.Add(&Profile{
		ID: "oauth", ProviderID: "anthropic", Type: AuthTypeOAuth,
		Stats: UsageStats{LastUsed: base},
	})

	rotator := NewRotator(store)
	selected, err := rotator.Select("anthropic", "sess1", now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "oauth" {
		t.Errorf("expected OAuth profile, got %q", selected.ID)
	}
}

func TestRotatorSessionPinning(t *testing.T) {
	store := NewStore("")
	now := time.Now()

	store.Add(&Profile{
		ID: "a", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: now.Add(-2 * time.Hour)},
	})
	store.Add(&Profile{
		ID: "b", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: now.Add(-time.Hour)},
	})

	rotator := NewRotator(store)
	rotator.PinForSession("sess1", "b")

	selected, err := rotator.Select("anthropic", "sess1", now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "b" {
		t.Errorf("expected pinned profile 'b', got %q", selected.ID)
	}
}

func TestRotatorSkipsCooldown(t *testing.T) {
	store := NewStore("")
	now := time.Now()

	store.Add(&Profile{
		ID: "cooled", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{
			LastUsed:      now.Add(-2 * time.Hour),
			CooldownUntil: now.Add(time.Hour),
		},
	})
	store.Add(&Profile{
		ID: "available", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{LastUsed: now.Add(-time.Hour)},
	})

	rotator := NewRotator(store)
	selected, err := rotator.Select("anthropic", "sess1", now)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "available" {
		t.Errorf("expected available profile, got %q", selected.ID)
	}
}

func TestRotatorAllInCooldown(t *testing.T) {
	store := NewStore("")
	now := time.Now()

	store.Add(&Profile{
		ID: "a", ProviderID: "anthropic", Type: AuthTypeAPIKey,
		Stats: UsageStats{CooldownUntil: now.Add(time.Hour)},
	})

	rotator := NewRotator(store)
	_, err := rotator.Select("anthropic", "sess1", now)
	if err == nil {
		t.Error("expected error when all in cooldown")
	}

	if !rotator.AllInCooldown("anthropic", now) {
		t.Error("AllInCooldown should return true")
	}
}

func TestRotatorNoProfiles(t *testing.T) {
	store := NewStore("")
	rotator := NewRotator(store)
	now := time.Now()

	_, err := rotator.Select("nonexistent", "sess1", now)
	if err == nil {
		t.Error("expected error for no profiles")
	}
}

func TestStoreAddAndGet(t *testing.T) {
	store := NewStore("")
	store.Add(&Profile{ID: "anthropic:default", ProviderID: "anthropic"})
	store.Add(&Profile{ID: "openai:default", ProviderID: "openai"})

	if got := store.ForProvider("anthropic"); len(got) != 1 {
		t.Errorf("ForProvider(anthropic) = %d profiles, want 1", len(got))
	}

	p := store.Get("openai:default")
	if p == nil {
		t.Fatal("Get returned nil")
	}
	if p.ProviderID != "openai" {
		t.Errorf("ProviderID = %q, want openai", p.ProviderID)
	}

	if store.Get("nonexistent") != nil {
		t.Error("Get should return nil for nonexistent profile")
	}
}

func TestMaskedKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"long API key", "sk-ant-abc123xyz789secret", "sk-ant***"},
		{"short key", "abc", "***"},
		{"exact 6 chars", "abcdef", "***"},
		{"7 chars", "abcdefg", "abcdef***"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Profile{Key: tt.key}
			if got := p.MaskedKey(); got != tt.want {
				t.Errorf("MaskedKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStoreYAMLRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/auth-state.yaml"

	store := NewStore(path)
	store.Add(&Profile{
		ID:         "anthropic:default",
		ProviderID: "anthropic",
		Type:       AuthTypeAPIKey,
		Key:        "sk-ant-test-key",
	})
	store.Add(&Profile{
		ID:         "openai:default",
		ProviderID: "openai",
		Type:       AuthTypeOAuth,
		OAuthToken: "oauth-token-123",
	})

	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewStore(path)
	if err := loaded.Load(); err != nil {
		t.Fatal(err)
	}

	p := loaded.Get("anthropic:default")
	if p == nil {
		t.Fatal("expected anthropic profile")
	}
	if p.Key != "" {
		t.Errorf("Key should NOT be persisted, got %q", p.Key)
	}
	if p.Type != AuthTypeAPIKey {
		t.Errorf("Type = %q, want api_key", p.Type)
	}

	p2 := loaded.Get("openai:default")
	if p2 == nil {
		t.Fatal("expected openai profile")
	}
	if p2.Type != AuthTypeOAuth {
		t.Errorf("Type = %q, want oauth", p2.Type)
	}
	if p2.OAuthToken != "oauth-token-123" {
		t.Errorf("OAuthToken = %q, want oauth-token-123", p2.OAuthToken)
	}
}

func TestCredential(t *testing.T) {
	apiKey := &Profile{Key: "sk-ant-123"}
	if apiKey.Credential() != "sk-ant-123" {
		t.Errorf("expected API key, got %q", apiKey.Credential())
	}

	oauth := &Profile{OAuthToken: "eyJ-token"}
	if oauth.Credential() != "eyJ-token" {
		t.Errorf("expected OAuth token, got %q", oauth.Credential())
	}

	apiKeyPriority := &Profile{Key: "sk-key", OAuthToken: "token"}
	if apiKeyPriority.Credential() != "sk-key" {
		t.Errorf("API key should take priority, got %q", apiKeyPriority.Credential())
	}

	empty := &Profile{}
	if empty.Credential() != "" {
		t.Errorf("expected empty, got %q", empty.Credential())
	}
}
