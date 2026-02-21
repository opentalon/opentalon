package auth

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type AuthType string

const (
	AuthTypeAPIKey AuthType = "api_key"
	AuthTypeOAuth  AuthType = "oauth"
)

type UsageStats struct {
	LastUsed      time.Time `yaml:"last_used,omitempty"`
	CooldownUntil time.Time `yaml:"cooldown_until,omitempty"`
	DisabledUntil time.Time `yaml:"disabled_until,omitempty"`
	ErrorCount    int       `yaml:"error_count,omitempty"`
}

// Profile represents a single credential for a provider.
// API keys are resolved from env vars at startup and held in memory.
// The Key field is never persisted to disk -- only runtime state (Stats)
// and OAuth tokens are written to the runtime cache file.
type Profile struct {
	ID         string     `yaml:"id"`
	ProviderID string     `yaml:"provider_id"`
	Type       AuthType   `yaml:"type"`
	Key        string     `yaml:"-"` // never persisted; resolved from env vars
	OAuthToken string     `yaml:"oauth_token,omitempty"`
	Stats      UsageStats `yaml:"usage_stats,omitempty"`
}

func (p *Profile) InCooldown(now time.Time) bool {
	return !p.Stats.CooldownUntil.IsZero() && now.Before(p.Stats.CooldownUntil)
}

func (p *Profile) IsDisabled(now time.Time) bool {
	return !p.Stats.DisabledUntil.IsZero() && now.Before(p.Stats.DisabledUntil)
}

func (p *Profile) Available(now time.Time) bool {
	return !p.InCooldown(now) && !p.IsDisabled(now)
}

const maskSuffix = "***"

// MaskedKey returns the credential with most characters replaced by ***.
// Shows at most the first 6 characters for identification.
func (p *Profile) MaskedKey() string {
	secret := p.Key
	if secret == "" {
		secret = p.OAuthToken
	}
	if secret == "" {
		return ""
	}
	visible := 6
	if len(secret) <= visible {
		return maskSuffix
	}
	return secret[:visible] + maskSuffix
}

// Credential returns the active credential (API key or OAuth token).
func (p *Profile) Credential() string {
	if p.Key != "" {
		return p.Key
	}
	return p.OAuthToken
}

type Store struct {
	path     string
	profiles map[string][]*Profile // keyed by provider ID
}

func NewStore(path string) *Store {
	return &Store{
		path:     path,
		profiles: make(map[string][]*Profile),
	}
}

func (s *Store) Load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading auth profiles: %w", err)
	}
	return yaml.Unmarshal(data, &s.profiles)
}

func (s *Store) Save() error {
	data, err := yaml.Marshal(s.profiles)
	if err != nil {
		return fmt.Errorf("marshaling auth profiles: %w", err)
	}
	return os.WriteFile(s.path, data, 0600)
}

func (s *Store) Add(p *Profile) {
	s.profiles[p.ProviderID] = append(s.profiles[p.ProviderID], p)
}

func (s *Store) ForProvider(providerID string) []*Profile {
	return s.profiles[providerID]
}

func (s *Store) Get(profileID string) *Profile {
	for _, profiles := range s.profiles {
		for _, p := range profiles {
			if p.ID == profileID {
				return p
			}
		}
	}
	return nil
}
