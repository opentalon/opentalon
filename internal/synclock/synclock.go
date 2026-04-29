// Package synclock provides distributed locking for startup knowledge sync
// in multi-pod deployments. When cluster mode is enabled, only one pod runs
// the initial SyncActions/SyncGlossary/IngestKnowledgeDir sequence; the
// others wait for completion and skip.
package synclock

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// Locker coordinates distributed startup sync across pods.
type Locker interface {
	// AcquireOrWait tries to acquire the startup sync lock.
	// Returns (true, nil) if this pod should run sync,
	// (false, nil) if another pod already completed sync,
	// or (false, err) on timeout/failure.
	AcquireOrWait(ctx context.Context) (acquired bool, err error)

	// ReleaseDone marks sync as complete and releases the lock.
	ReleaseDone(ctx context.Context)

	// ReleaseAbort releases the lock without marking sync as done.
	ReleaseAbort(ctx context.Context)

	// TryAcquirePlugin attempts a non-blocking lock for a single plugin sync.
	TryAcquirePlugin(ctx context.Context, pluginName string) (bool, error)

	// ReleasePlugin releases a per-plugin lock.
	ReleasePlugin(ctx context.Context, pluginName string)
}

// Noop returns a Locker that always grants the lock (single-instance mode).
func Noop() Locker { return noopLocker{} }

type noopLocker struct{}

func (noopLocker) AcquireOrWait(context.Context) (bool, error)            { return true, nil }
func (noopLocker) ReleaseDone(context.Context)                            {}
func (noopLocker) ReleaseAbort(context.Context)                           {}
func (noopLocker) TryAcquirePlugin(context.Context, string) (bool, error) { return true, nil }
func (noopLocker) ReleasePlugin(context.Context, string)                  {}

const (
	lockKey  = "opentalon:sync:startup"
	doneKey  = "opentalon:sync:startup:done"
	lockTTL  = 5 * time.Minute
	doneTTL  = 30 * time.Minute
	pollWait = 2 * time.Second
	timeout  = 5 * time.Minute

	pluginKeyPrefix = "opentalon:sync:plugin:"
	pluginLockTTL   = 30 * time.Second
)

type redisLocker struct {
	client redis.UniversalClient
	owner  string
	stopHB context.CancelFunc // stops heartbeat goroutine
}

// NewRedis creates a Locker backed by Redis for multi-pod coordination.
func NewRedis(client redis.UniversalClient) Locker {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("pid-%d", os.Getpid())
	}
	return &redisLocker{client: client, owner: hostname}
}

func (r *redisLocker) AcquireOrWait(ctx context.Context) (bool, error) {
	// Fast path: another pod already completed sync this cycle.
	if r.doneExists(ctx) {
		return false, nil
	}

	// Try to become the leader.
	ok, err := r.client.SetNX(ctx, lockKey, r.owner, lockTTL).Result()
	if err != nil {
		return false, fmt.Errorf("synclock: SET NX: %w", err)
	}
	if ok {
		r.startHeartbeat(ctx)
		return true, nil
	}

	// Another pod holds the lock — poll until done or lock vanishes.
	return r.waitForCompletion(ctx)
}

func (r *redisLocker) ReleaseDone(ctx context.Context) {
	r.stopHeartbeat()
	r.client.Set(ctx, doneKey, r.owner, doneTTL)
	r.client.Del(ctx, lockKey)
}

func (r *redisLocker) ReleaseAbort(ctx context.Context) {
	r.stopHeartbeat()
	r.client.Del(ctx, lockKey)
}

func (r *redisLocker) TryAcquirePlugin(ctx context.Context, pluginName string) (bool, error) {
	key := pluginKeyPrefix + pluginName
	ok, err := r.client.SetNX(ctx, key, r.owner, pluginLockTTL).Result()
	if err != nil {
		return false, fmt.Errorf("synclock: plugin SET NX: %w", err)
	}
	return ok, nil
}

func (r *redisLocker) ReleasePlugin(ctx context.Context, pluginName string) {
	r.client.Del(ctx, pluginKeyPrefix+pluginName)
}

func (r *redisLocker) doneExists(ctx context.Context) bool {
	n, err := r.client.Exists(ctx, doneKey).Result()
	return err == nil && n > 0
}

func (r *redisLocker) waitForCompletion(ctx context.Context) (bool, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(pollWait)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline:
			return false, fmt.Errorf("synclock: timed out waiting for leader to complete sync")
		case <-ticker.C:
			// Leader finished?
			if r.doneExists(ctx) {
				return false, nil
			}
			// Lock vanished without done signal = leader crashed. Try to take over.
			lockExists, err := r.client.Exists(ctx, lockKey).Result()
			if err != nil {
				slog.Warn("synclock: EXISTS check failed", "error", err)
				continue
			}
			if lockExists == 0 {
				ok, err := r.client.SetNX(ctx, lockKey, r.owner, lockTTL).Result()
				if err != nil {
					return false, fmt.Errorf("synclock: re-acquire SET NX: %w", err)
				}
				if ok {
					slog.Info("synclock: previous leader crashed, this pod took over")
					r.startHeartbeat(ctx)
					return true, nil
				}
				// Another pod beat us to it — keep waiting.
			}
		}
	}
}

func (r *redisLocker) startHeartbeat(ctx context.Context) {
	hbCtx, cancel := context.WithCancel(ctx)
	r.stopHB = cancel
	go func() {
		ticker := time.NewTicker(lockTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				r.client.Expire(hbCtx, lockKey, lockTTL)
			}
		}
	}()
}

func (r *redisLocker) stopHeartbeat() {
	if r.stopHB != nil {
		r.stopHB()
		r.stopHB = nil
	}
}
