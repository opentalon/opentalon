package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type AuthType string

const (
	AuthTypeAPIKey AuthType = "api_key"
	AuthTypeOAuth  AuthType = "oauth"
)

type UsageStats struct {
	LastUsed      time.Time `json:"last_used"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	DisabledUntil time.Time `json:"disabled_until,omitempty"`
	ErrorCount    int       `json:"error_count"`
}

type Profile struct {
	ID         string     `json:"id"`
	ProviderID string     `json:"provider_id"`
	Type       AuthType   `json:"type"`
	Key        string     `json:"key,omitempty"`
	Stats      UsageStats `json:"usage_stats"`
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
	return json.Unmarshal(data, &s.profiles)
}

func (s *Store) Save() error {
	data, err := json.MarshalIndent(s.profiles, "", "  ")
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
