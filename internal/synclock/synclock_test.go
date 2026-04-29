package synclock

import (
	"context"
	"sync"
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

func TestNoop_AlwaysAcquires(t *testing.T) {
	l := Noop()
	ctx := context.Background()

	acquired, err := l.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("noop locker should always acquire")
	}

	// Plugin locks also always succeed.
	ok, err := l.TryAcquirePlugin(ctx, "test")
	if err != nil || !ok {
		t.Fatal("noop plugin lock should always acquire")
	}

	// Release methods are no-ops, should not panic.
	l.ReleaseDone(ctx)
	l.ReleaseAbort(ctx)
	l.ReleasePlugin(ctx, "test")
}

func TestRedis_SinglePodAcquiresAndDone(t *testing.T) {
	_, client := newTestRedis(t)
	l := NewRedis(client)
	ctx := context.Background()

	acquired, err := l.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("first pod should acquire")
	}

	l.ReleaseDone(ctx)

	// Second call sees done key and skips.
	l2 := NewRedis(client)
	acquired2, err := l2.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if acquired2 {
		t.Fatal("second pod should skip after done")
	}
}

func TestRedis_ConcurrentAcquire_ExactlyOneWins(t *testing.T) {
	_, client := newTestRedis(t)
	ctx := context.Background()

	const n = 5
	results := make([]bool, n)
	var wg sync.WaitGroup
	wg.Add(n)

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			l := NewRedis(client)
			acquired, err := l.AcquireOrWait(ctx)
			if err != nil {
				t.Errorf("pod %d: %v", idx, err)
				return
			}
			results[idx] = acquired
			if acquired {
				// Simulate short sync work.
				time.Sleep(50 * time.Millisecond)
				l.ReleaseDone(ctx)
			}
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, r := range results {
		if r {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", winners)
	}
}

func TestRedis_AbortAllowsReacquire(t *testing.T) {
	mr, client := newTestRedis(t)
	ctx := context.Background()

	l1 := NewRedis(client)
	acquired, err := l1.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("should acquire")
	}

	// Abort without setting done.
	l1.ReleaseAbort(ctx)

	// Fast-forward miniredis so any TTL-based state is clear.
	mr.FastForward(time.Second)

	// Another pod should be able to acquire now.
	l2 := NewRedis(client)
	acquired2, err := l2.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired2 {
		t.Fatal("should re-acquire after abort")
	}
	l2.ReleaseDone(ctx)
}

func TestRedis_CrashedLeader_FollowerTakesOver(t *testing.T) {
	mr, client := newTestRedis(t)
	ctx := context.Background()

	// Pod 1 acquires the lock.
	l1 := NewRedis(client)
	acquired, err := l1.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("should acquire")
	}

	// Simulate crash: stop heartbeat manually and expire the lock.
	l1.(*redisLocker).stopHeartbeat()

	// Fast-forward past lock TTL so the key expires.
	mr.FastForward(lockTTL + time.Second)

	// Pod 2 starts, sees no done key and no lock — should take over.
	l2 := NewRedis(client)
	acquired2, err := l2.AcquireOrWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired2 {
		t.Fatal("follower should take over after leader crash")
	}
	l2.ReleaseDone(ctx)
}

func TestRedis_TryAcquirePlugin_MutualExclusion(t *testing.T) {
	_, client := newTestRedis(t)
	ctx := context.Background()

	l1 := NewRedis(client)
	l2 := NewRedis(client)

	ok1, err := l1.TryAcquirePlugin(ctx, "myplugin")
	if err != nil {
		t.Fatal(err)
	}
	if !ok1 {
		t.Fatal("first should acquire plugin lock")
	}

	ok2, err := l2.TryAcquirePlugin(ctx, "myplugin")
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Fatal("second should NOT acquire same plugin lock")
	}

	// Different plugin should succeed.
	ok3, err := l2.TryAcquirePlugin(ctx, "otherplugin")
	if err != nil {
		t.Fatal(err)
	}
	if !ok3 {
		t.Fatal("different plugin name should acquire")
	}

	// After release, same plugin should be acquirable.
	l1.ReleasePlugin(ctx, "myplugin")
	ok4, err := l2.TryAcquirePlugin(ctx, "myplugin")
	if err != nil {
		t.Fatal(err)
	}
	if !ok4 {
		t.Fatal("should acquire after release")
	}
}

func TestRedis_ContextCancelled(t *testing.T) {
	_, client := newTestRedis(t)

	// Pod 1 holds the lock (never releases).
	l1 := NewRedis(client)
	ctx := context.Background()
	acquired, err := l1.AcquireOrWait(ctx)
	if err != nil || !acquired {
		t.Fatal("should acquire")
	}

	// Pod 2 waits with a cancelled context.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancelled

	l2 := NewRedis(client)
	_, err = l2.AcquireOrWait(cancelCtx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}
