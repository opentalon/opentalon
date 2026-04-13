//go:build redis

package dedup_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/dedup"
	"github.com/opentalon/opentalon/internal/redisclient"
)

// Run with: go test -tags redis -run TestRedisDedup ./internal/dedup/
// Requires REDIS_URL pointing at a Redis instance (e.g. "redis://localhost:6379/0").

func redisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping Redis integration tests")
	}
	return url
}

func TestRedisDedupStandalone_TryAcquire(t *testing.T) {
	client, err := redisclient.New(redisURL(t), "", nil, "", "")
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	d := dedup.NewFromClient(client)
	defer client.Close()

	ctx := context.Background()
	key := "test:dedup:standalone:" + t.Name()
	ttl := 10 * time.Second

	// First acquire must win.
	won, err := d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire (1): %v", err)
	}
	if !won {
		t.Fatal("expected first TryAcquire to return true")
	}

	// Second acquire for the same key must lose.
	won, err = d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire (2): %v", err)
	}
	if won {
		t.Fatal("expected second TryAcquire to return false")
	}
}

func TestRedisDedupStandalone_DifferentKeys(t *testing.T) {
	client, err := redisclient.New(redisURL(t), "", nil, "", "")
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	d := dedup.NewFromClient(client)
	defer client.Close()

	ctx := context.Background()
	prefix := "test:dedup:keys:" + t.Name() + ":"
	ttl := 10 * time.Second

	for _, key := range []string{prefix + "a", prefix + "b", prefix + "c"} {
		won, err := d.TryAcquire(ctx, key, ttl)
		if err != nil {
			t.Fatalf("TryAcquire(%s): %v", key, err)
		}
		if !won {
			t.Fatalf("expected TryAcquire(%s) to return true", key)
		}
	}
}

func TestRedisDedupStandalone_TTLExpiry(t *testing.T) {
	client, err := redisclient.New(redisURL(t), "", nil, "", "")
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	d := dedup.NewFromClient(client)
	defer client.Close()

	ctx := context.Background()
	key := "test:dedup:ttl:" + t.Name()
	ttl := 200 * time.Millisecond

	won, err := d.TryAcquire(ctx, key, ttl)
	if err != nil || !won {
		t.Fatalf("first acquire failed: won=%v err=%v", won, err)
	}

	// Wait for TTL to expire.
	time.Sleep(300 * time.Millisecond)

	// After expiry the key is gone — a new acquire must win again.
	won, err = d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire after TTL: %v", err)
	}
	if !won {
		t.Fatal("expected TryAcquire to win after TTL expiry")
	}
}

func TestRedisDedupStandalone_InvalidURL(t *testing.T) {
	_, err := redisclient.New("not-a-url", "", nil, "", "")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
	if !strings.Contains(err.Error(), "parsing redis URL") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// sentinelAddrs returns the Sentinel address(es) from REDIS_SENTINEL_ADDRS
// (comma-separated, e.g. "localhost:26379") and skips if not set.
func sentinelAddrs(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("REDIS_SENTINEL_ADDRS")
	if raw == "" {
		t.Skip("REDIS_SENTINEL_ADDRS not set; skipping Sentinel integration tests")
	}
	var addrs []string
	for _, a := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(a); s != "" {
			addrs = append(addrs, s)
		}
	}
	return addrs
}

func sentinelMaster(t *testing.T) string {
	t.Helper()
	name := os.Getenv("REDIS_SENTINEL_MASTER")
	if name == "" {
		name = "mymaster"
	}
	return name
}

func TestRedisDedupSentinel_TryAcquire(t *testing.T) {
	client, err := redisclient.New("", sentinelMaster(t), sentinelAddrs(t), "", "")
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	d := dedup.NewFromClient(client)
	defer client.Close()

	ctx := context.Background()
	key := "test:dedup:sentinel:" + t.Name()
	ttl := 10 * time.Second

	won, err := d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire (1): %v", err)
	}
	if !won {
		t.Fatal("expected first TryAcquire to return true")
	}

	won, err = d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire (2): %v", err)
	}
	if won {
		t.Fatal("expected second TryAcquire to return false")
	}
}

func TestRedisDedupSentinel_TTLExpiry(t *testing.T) {
	client, err := redisclient.New("", sentinelMaster(t), sentinelAddrs(t), "", "")
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	d := dedup.NewFromClient(client)
	defer client.Close()

	ctx := context.Background()
	key := "test:dedup:sentinel:ttl:" + t.Name()
	ttl := 200 * time.Millisecond

	won, err := d.TryAcquire(ctx, key, ttl)
	if err != nil || !won {
		t.Fatalf("first acquire failed: won=%v err=%v", won, err)
	}

	time.Sleep(300 * time.Millisecond)

	won, err = d.TryAcquire(ctx, key, ttl)
	if err != nil {
		t.Fatalf("TryAcquire after TTL: %v", err)
	}
	if !won {
		t.Fatal("expected TryAcquire to win after TTL expiry")
	}
}

func TestRedisDedupSentinel_InvalidMaster(t *testing.T) {
	sentinelAddrs(t) // skip if REDIS_SENTINEL_ADDRS not set
	_, err := redisclient.New("", "", []string{"localhost:26379"}, "", "")
	if err == nil {
		t.Fatal("expected error for empty master name")
	}
}

func TestRedisDedupSentinel_NoSentinels(t *testing.T) {
	sentinelAddrs(t) // skip if REDIS_SENTINEL_ADDRS not set
	_, err := redisclient.New("", "mymaster", []string{}, "", "")
	if err == nil {
		t.Fatal("expected error for empty sentinel list")
	}
}
