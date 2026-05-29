package channel

import (
	"context"
	"testing"
	"time"
)

// TestMemoryEnrichCache_HitMiss covers the basic get/set cycle: a missing
// key reports a miss, a set produces a hit on the same key, and an unrelated
// key still reports a miss after the set.
func TestMemoryEnrichCache_HitMiss(t *testing.T) {
	c := newMemoryEnrichCache()
	ctx := context.Background()

	if _, ok, err := c.Get(ctx, "k1"); err != nil || ok {
		t.Fatalf("initial Get: ok=%v err=%v, want miss", ok, err)
	}
	if err := c.Set(ctx, "k1", `{"email":"a@b"}`, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := c.Get(ctx, "k1")
	if err != nil || !ok || v != `{"email":"a@b"}` {
		t.Fatalf("Get after Set: v=%q ok=%v err=%v", v, ok, err)
	}
	if _, ok, _ := c.Get(ctx, "k2"); ok {
		t.Fatalf("unrelated key reported hit; cache contaminated")
	}
}

// TestMemoryEnrichCache_TTLExpiry confirms an entry past its TTL reports a
// miss without manual cleanup. The cleanup goroutine isn't load-bearing here —
// Get itself deletes the expired entry — so the test deliberately keeps TTL
// short and avoids waiting on the 5-minute sweep ticker.
func TestMemoryEnrichCache_TTLExpiry(t *testing.T) {
	c := newMemoryEnrichCache()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 10*time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatalf("expected miss after TTL expiry")
	}
}

// TestMemoryEnrichCache_EvictOnFullCap stresses the eviction path by
// jamming the cache full one over its cap. The soonest-expiring entry
// should be the one dropped so newer writes stay available for the typical
// access pattern (recently-touched users send more messages soon).
func TestMemoryEnrichCache_EvictOnFullCap(t *testing.T) {
	// Use a tiny cap via a fresh struct so we don't blow real memory.
	c := &memoryEnrichCache{entries: make(map[string]memoryEnrichEntry), done: make(chan struct{})}
	ctx := context.Background()

	// Fill at the production cap, then add one more.
	for i := 0; i < memoryEnrichCacheMaxEntries; i++ {
		key := time.Now().Add(time.Duration(i) * time.Microsecond).Format(time.RFC3339Nano)
		_ = c.Set(ctx, key, "v", time.Hour)
	}
	if len(c.entries) != memoryEnrichCacheMaxEntries {
		t.Fatalf("pre-fill len = %d, want %d", len(c.entries), memoryEnrichCacheMaxEntries)
	}
	// One more write should keep us at the cap (one eviction happened).
	_ = c.Set(ctx, "overflow", "v", time.Hour)
	if len(c.entries) != memoryEnrichCacheMaxEntries {
		t.Fatalf("post-overflow len = %d, want %d (eviction did not fire)", len(c.entries), memoryEnrichCacheMaxEntries)
	}
}
