package sessionlock

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, redis.UniversalClient) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// lockInBackground starts a Lock call in a goroutine and returns a channel
// that is closed once the lock is acquired, plus a func to release it.
func lockInBackground(t *testing.T, l Locker, key string) (acquired chan struct{}, release func()) {
	t.Helper()
	acquired = make(chan struct{})
	relCh := make(chan func(), 1)
	go func() {
		rel, err := l.Lock(context.Background(), key)
		if err != nil {
			t.Errorf("background Lock(%q): %v", key, err)
			return
		}
		relCh <- rel
		close(acquired)
	}()
	return acquired, func() {
		select {
		case rel := <-relCh:
			rel()
		case <-time.After(5 * time.Second):
			t.Fatalf("background lock for %q never acquired; nothing to release", key)
		}
	}
}

func assertBlocked(t *testing.T, acquired chan struct{}, wait time.Duration, msg string) {
	t.Helper()
	select {
	case <-acquired:
		t.Fatal(msg)
	case <-time.After(wait):
	}
}

func assertAcquired(t *testing.T, acquired chan struct{}, wait time.Duration, msg string) {
	t.Helper()
	select {
	case <-acquired:
	case <-time.After(wait):
		t.Fatal(msg)
	}
}

func TestNoop_AlwaysGrants(t *testing.T) {
	l := Noop()
	rel1, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	// Release is a no-op and safe to call repeatedly.
	rel1()
	rel1()
	rel2()
}

// (a) Two concurrent Lock() calls for the same key: the second blocks until
// the first releases.
func TestRedis_SameKeySerializes(t *testing.T) {
	_, client := newTestRedis(t)
	l := NewRedis(client, WithPollInterval(20*time.Millisecond))

	rel, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}

	acquired, releaseSecond := lockInBackground(t, l, "s1")
	assertBlocked(t, acquired, 200*time.Millisecond, "second Lock acquired while first still held")

	rel()
	assertAcquired(t, acquired, 2*time.Second, "second Lock not granted after first release")
	releaseSecond()
}

// (b) Different keys do not block each other.
func TestRedis_DifferentKeysIndependent(t *testing.T) {
	_, client := newTestRedis(t)
	l := NewRedis(client, WithPollInterval(20*time.Millisecond))

	rel1, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()

	acquired, releaseSecond := lockInBackground(t, l, "s2")
	assertAcquired(t, acquired, 2*time.Second, "Lock on a different key blocked by an unrelated holder")
	releaseSecond()
}

// (c) The heartbeat keeps a lock held longer than the TTL: a competitor
// stays blocked while simulated time passes well beyond the TTL, and only
// acquires after the holder releases.
func TestRedis_HeartbeatOutlivesTTL(t *testing.T) {
	mr, client := newTestRedis(t)
	ttl := 600 * time.Millisecond // heartbeat renews every 200ms
	l := NewRedis(client, WithTTL(ttl), WithPollInterval(20*time.Millisecond))

	rel, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}

	acquired, releaseSecond := lockInBackground(t, l, "s1")

	// Advance simulated Redis time past 2x the TTL in small steps, giving
	// the wall-clock heartbeat room to renew between steps. Without the
	// heartbeat the key would expire after the first two steps.
	for i := 0; i < 4; i++ {
		time.Sleep(450 * time.Millisecond) // >= 2 heartbeat periods
		mr.FastForward(400 * time.Millisecond)
		if !mr.Exists(keyPrefix + "s1") {
			t.Fatalf("lease expired at step %d despite heartbeat", i)
		}
	}
	assertBlocked(t, acquired, 100*time.Millisecond, "competitor acquired while heartbeat-renewed lease was held")

	rel()
	assertAcquired(t, acquired, 2*time.Second, "competitor not granted after holder release")
	releaseSecond()
}

// (d) Crashed-holder simulation: a holder whose heartbeat stopped (test
// hook) frees the session within one TTL.
func TestRedis_CrashedHolderExpiresWithinTTL(t *testing.T) {
	mr, client := newTestRedis(t)
	ttl := 500 * time.Millisecond
	crashed := NewRedis(client, WithTTL(ttl)).(*redisLocker)
	crashed.disableHeartbeat = true

	if _, err := crashed.Lock(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	// Never released: the holder "crashed".

	l := NewRedis(client, WithTTL(ttl), WithPollInterval(20*time.Millisecond))
	acquired, releaseSecond := lockInBackground(t, l, "s1")
	assertBlocked(t, acquired, 150*time.Millisecond, "competitor acquired before the crashed holder's lease expired")

	mr.FastForward(ttl + 50*time.Millisecond)
	assertAcquired(t, acquired, 2*time.Second, "competitor not granted after crashed holder's lease expired")
	releaseSecond()
}

// (e) Release only removes its own token: a stale release fired after the
// lease expired and was re-acquired by another holder is a no-op.
func TestRedis_StaleReleaseIsNoop(t *testing.T) {
	mr, client := newTestRedis(t)
	ttl := 500 * time.Millisecond

	stale := NewRedis(client, WithTTL(ttl)).(*redisLocker)
	stale.disableHeartbeat = true
	staleRelease, err := stale.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Lease expires; a second holder takes over.
	mr.FastForward(ttl + 50*time.Millisecond)
	l := NewRedis(client, WithTTL(ttl), WithPollInterval(20*time.Millisecond))
	rel2, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}

	// The first holder's release must not delete the new holder's lock.
	staleRelease()
	if !mr.Exists(keyPrefix + "s1") {
		t.Fatal("stale release deleted another holder's lock")
	}

	rel2()
	if mr.Exists(keyPrefix + "s1") {
		t.Fatal("owner release did not delete the lock")
	}
}

// (f) Fail-open: with Redis unreachable, Lock returns a working release
// func after the dial attempt instead of blocking or failing the turn.
func TestRedis_FailOpenOnDeadRedis(t *testing.T) {
	client := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // nothing listens here
		DialTimeout: 200 * time.Millisecond,
		ReadTimeout: 200 * time.Millisecond,
		MaxRetries:  -1, // disable go-redis internal retries to keep the bound tight
	})
	t.Cleanup(func() { _ = client.Close() })
	l := NewRedis(client)

	type result struct {
		rel func()
		err error
	}
	done := make(chan result, 1)
	go func() {
		rel, err := l.Lock(context.Background(), "s1")
		done <- result{rel, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("fail-open Lock returned error: %v", res.err)
		}
		res.rel() // must be callable and instant
	case <-time.After(3 * time.Second):
		t.Fatal("Lock blocked on a dead Redis instead of failing open")
	}
}

// A waiter whose context ends never acquires; it gets the context error.
func TestRedis_WaiterContextCancelled(t *testing.T) {
	_, client := newTestRedis(t)
	l := NewRedis(client, WithPollInterval(20*time.Millisecond))

	rel, err := l.Lock(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer rel()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "s1"); err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
