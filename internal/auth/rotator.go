package auth

import (
	"fmt"
	"sort"
	"time"
)

type Rotator struct {
	store   *Store
	pinned  map[string]string // session ID -> profile ID
}

func NewRotator(store *Store) *Rotator {
	return &Rotator{
		store:  store,
		pinned: make(map[string]string),
	}
}

func (r *Rotator) PinForSession(sessionID, profileID string) {
	r.pinned[sessionID] = profileID
}

func (r *Rotator) UnpinSession(sessionID string) {
	delete(r.pinned, sessionID)
}

func (r *Rotator) Select(providerID string, sessionID string, now time.Time) (*Profile, error) {
	if pinnedID, ok := r.pinned[sessionID]; ok {
		p := r.store.Get(pinnedID)
		if p != nil && p.Available(now) {
			return p, nil
		}
		delete(r.pinned, sessionID)
	}

	profiles := r.store.ForProvider(providerID)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no auth profiles for provider %q", providerID)
	}

	available := make([]*Profile, 0, len(profiles))
	for _, p := range profiles {
		if p.Available(now) {
			available = append(available, p)
		}
	}

	if len(available) == 0 {
		return nil, fmt.Errorf("all auth profiles for provider %q are in cooldown", providerID)
	}

	sort.Slice(available, func(i, j int) bool {
		pi, pj := available[i], available[j]
		if pi.Type != pj.Type {
			return pi.Type == AuthTypeOAuth
		}
		return pi.Stats.LastUsed.Before(pj.Stats.LastUsed)
	})

	selected := available[0]
	selected.Stats.LastUsed = now
	return selected, nil
}

func (r *Rotator) AllInCooldown(providerID string, now time.Time) bool {
	profiles := r.store.ForProvider(providerID)
	if len(profiles) == 0 {
		return true
	}
	for _, p := range profiles {
		if p.Available(now) {
			return false
		}
	}
	return true
}
