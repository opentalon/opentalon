package channel

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// EnrichCache stores the JSON-encoded result of an inbound-enrichment step
// keyed by a channel-instance-scoped key. Hits avoid re-running the HTTP
// call described in `inbound.enrich`; misses fall through to the live call
// in runEnrich and the returned value is written back via Set.
//
// Two implementations: redisEnrichCache (preferred when the deployment
// already runs Redis for dedup or sync-locks) and memoryEnrichCache (fallback
// for single-pod or test setups). Selection happens once at startup in
// NewEnrichCache; the chosen implementation is shared across every channel
// instance and namespaces its keys by channel id internally.
type EnrichCache interface {
	// Get returns (raw JSON, true, nil) on hit, ("", false, nil) on miss,
	// and ("", false, err) on cache backend failure. Backend failure is
	// distinct from miss so callers can decide whether to fall through to
	// the live call (preferred for memory cache) or fail closed (preferred
	// for cross-pod consistency with Redis).
	Get(ctx context.Context, key string) (string, bool, error)
	// Set stores raw JSON under key with the given TTL. Errors are
	// logged-and-ignored at the call site so a temporary backend hiccup
	// doesn't block message processing — the next message will re-fetch
	// and re-attempt the write.
	Set(ctx context.Context, key, val string, ttl time.Duration) error
}

// NewEnrichCache returns a Redis-backed cache when client is non-nil,
// otherwise an in-memory cache. The choice is made once at startup so every
// channel instance in this process shares the same backend — there is no
// per-channel cache, which is what lets two slack instances of the same
// kind reuse cached user.info lookups when the configured cache key (e.g.
// `{{event.user}}`) collides across them. Namespace prefixes added by
// callers (channel instance id) keep the keys distinct when isolation is
// actually wanted.
func NewEnrichCache(client redis.UniversalClient) EnrichCache {
	if client != nil {
		return &redisEnrichCache{client: client}
	}
	return newMemoryEnrichCache()
}

// ── Redis-backed implementation ─────────────────────────────────────────

type redisEnrichCache struct {
	client redis.UniversalClient
}

func (c *redisEnrichCache) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (c *redisEnrichCache) Set(ctx context.Context, key, val string, ttl time.Duration) error {
	return c.client.Set(ctx, key, val, ttl).Err()
}

// ── In-memory implementation ────────────────────────────────────────────

type memoryEnrichEntry struct {
	val       string
	expiresAt time.Time
}

type memoryEnrichCache struct {
	mu      sync.Mutex
	entries map[string]memoryEnrichEntry
	done    chan struct{}
}

// memoryEnrichCacheMaxEntries caps the in-memory map so a misconfigured
// enrich step (e.g. cache key set to a unique value per message) cannot
// grow the map unbounded and exhaust the process. When the cap is hit
// the entry expiring soonest is evicted on the write path.
const memoryEnrichCacheMaxEntries = 100_000

func newMemoryEnrichCache() *memoryEnrichCache {
	c := &memoryEnrichCache{
		entries: make(map[string]memoryEnrichEntry),
		done:    make(chan struct{}),
	}
	go c.cleanupLoop()
	return c
}

func (c *memoryEnrichCache) Get(_ context.Context, key string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false, nil
	}
	if time.Now().After(e.expiresAt) {
		delete(c.entries, key)
		return "", false, nil
	}
	return e.val, true, nil
}

func (c *memoryEnrichCache) Set(_ context.Context, key, val string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= memoryEnrichCacheMaxEntries {
		c.evictSoonestLocked()
	}
	c.entries[key] = memoryEnrichEntry{val: val, expiresAt: time.Now().Add(ttl)}
	return nil
}

// evictSoonestLocked removes the entry whose TTL fires first; preferred over
// a random eviction because the soonest-expiring entry is the cheapest to
// re-fetch (it would have expired anyway shortly).
func (c *memoryEnrichCache) evictSoonestLocked() {
	var victimKey string
	var victimExp time.Time
	first := true
	for k, e := range c.entries {
		if first || e.expiresAt.Before(victimExp) {
			victimKey = k
			victimExp = e.expiresAt
			first = false
		}
	}
	if !first {
		delete(c.entries, victimKey)
	}
}

func (c *memoryEnrichCache) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.entries {
				if now.After(e.expiresAt) {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		}
	}
}

// jsonOrEmpty is a small helper used by tests + the enrich runtime to
// produce a deterministic JSON string from a Go map; logs and stores empty
// string when marshalling fails. Kept here next to the cache because the
// cache's value contract is "JSON object as string".
func jsonOrEmpty(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
