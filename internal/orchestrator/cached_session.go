package orchestrator

import (
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// cachedSessionStore wraps a SessionStoreInterface with a request-scoped
// in-memory cache. It is created at the start of Run() and garbage-collected
// when Run() returns. The per-session lock in Run() guarantees that only one
// goroutine accesses a given session's cache at a time, so no extra
// synchronization is needed.
type cachedSessionStore struct {
	inner SessionStoreInterface
	cache map[string]*state.Session
}

func newCachedSessionStore(inner SessionStoreInterface) *cachedSessionStore {
	return &cachedSessionStore{inner: inner, cache: make(map[string]*state.Session)}
}

func (c *cachedSessionStore) Get(id string) (*state.Session, error) {
	if s, ok := c.cache[id]; ok {
		return s, nil
	}
	s, err := c.inner.Get(id)
	if err != nil {
		return nil, err
	}
	// Copy so in-memory stores (which return pointers to live data) don't
	// share the Messages slice with the cache — prevents double-append.
	cp := *s
	cp.Messages = make([]provider.Message, len(s.Messages))
	copy(cp.Messages, s.Messages)
	c.cache[id] = &cp
	return &cp, nil
}

func (c *cachedSessionStore) Create(id, entityID, groupID string) *state.Session {
	s := c.inner.Create(id, entityID, groupID)
	c.cache[id] = s
	return s
}

func (c *cachedSessionStore) AddMessage(id string, msg provider.Message) error {
	if err := c.inner.AddMessage(id, msg); err != nil {
		return err
	}
	// Update cache in-memory so subsequent Get() calls don't hit the DB.
	if s, ok := c.cache[id]; ok {
		s.Messages = append(s.Messages, msg)
	}
	return nil
}

func (c *cachedSessionStore) SetModel(id string, model provider.ModelRef) error {
	if err := c.inner.SetModel(id, model); err != nil {
		return err
	}
	if s, ok := c.cache[id]; ok {
		s.ActiveModel = model
	}
	return nil
}

func (c *cachedSessionStore) SetMetadata(id, key, value string) error {
	if err := c.inner.SetMetadata(id, key, value); err != nil {
		return err
	}
	if s, ok := c.cache[id]; ok {
		if s.Metadata == nil {
			s.Metadata = make(map[string]string)
		}
		if value == "" {
			delete(s.Metadata, key)
		} else {
			s.Metadata[key] = value
		}
	}
	return nil
}

func (c *cachedSessionStore) SetSummary(id string, summary string, messages []provider.Message) error {
	if err := c.inner.SetSummary(id, summary, messages); err != nil {
		return err
	}
	// Invalidate — the message set changed completely.
	delete(c.cache, id)
	return nil
}

func (c *cachedSessionStore) ClearMessages(id string) error {
	// Drop cache entry so the next Get reloads the cleared state from the
	// inner store (messages emptied, summary cleared, identity preserved).
	delete(c.cache, id)
	return c.inner.ClearMessages(id)
}

func (c *cachedSessionStore) Delete(id string) error {
	delete(c.cache, id)
	return c.inner.Delete(id)
}
