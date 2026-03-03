package channel

import (
	"sync"
	"time"
)

// Deduplicator tracks recently seen event IDs and rejects duplicates
// within a configurable TTL. Channels like Slack retry events on timeout,
// so this prevents processing the same message twice.
type Deduplicator struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
}

// NewDeduplicator creates a deduplicator with the given TTL.
// If ttl is zero, a default of 10 minutes is used.
func NewDeduplicator(ttl time.Duration) *Deduplicator {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	d := &Deduplicator{
		seen: make(map[string]time.Time),
		ttl:  ttl,
	}
	go d.cleanupLoop()
	return d
}

// IsDuplicate returns true if the eventID was already seen within the TTL.
// If not, it records it and returns false.
func (d *Deduplicator) IsDuplicate(eventID string) bool {
	if eventID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[eventID]; ok {
		return true
	}
	d.seen[eventID] = time.Now()
	return false
}

func (d *Deduplicator) cleanupLoop() {
	ticker := time.NewTicker(d.ttl / 2)
	defer ticker.Stop()
	for range ticker.C {
		d.mu.Lock()
		cutoff := time.Now().Add(-d.ttl)
		for id, t := range d.seen {
			if t.Before(cutoff) {
				delete(d.seen, id)
			}
		}
		d.mu.Unlock()
	}
}
