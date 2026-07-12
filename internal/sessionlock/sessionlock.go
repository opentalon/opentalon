// Package sessionlock provides a distributed per-session turn lease for
// multi-pod deployments.
//
// Invariant: at most one turn runs at a time per session, across all pods.
// The orchestrator already serializes turns within a single pod via an
// in-memory per-session mutex; this package extends that guarantee across
// pods so two inbound messages for the same session that land on different
// pods cannot run concurrently (which would race their replies and hide the
// first turn's reply from the second turn's context).
//
// The lock is a lease with a heartbeat, not a fixed TTL. A fixed TTL is
// wrong in both directions: too short and a long-running turn loses the
// lock mid-flight (a second pod enters and the race returns); too long and
// a crashed holder freezes the session for the whole TTL. Instead the lease
// is short (~30s) and a background goroutine renews it every ttl/3 while
// the turn is running, so a turn of any length stays covered while a
// crashed holder frees the session within at most one TTL.
//
// Fail-open: any Redis error during acquire or renew logs a warning and the
// caller proceeds WITHOUT the distributed lock. Chat must never block on
// Redis being down — losing cross-pod serialization temporarily is a much
// smaller failure than refusing every message. The in-memory mutex still
// serializes turns within each pod during such a degradation.
//
// Fairness: waiters poll for the key, so wake-up order is not strictly
// FIFO — the same semantics as competing goroutines on an in-process
// sync.Mutex. There is deliberately no acquire timeout that lets a waiter
// "win by timeout": a waiter either acquires the lease or its context ends;
// leaked locks are broken by the TTL, never by an impatient waiter.
package sessionlock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Locker grants the per-session turn lease. Lock blocks until the lease for
// sessionKey is acquired or ctx is done. The returned release func must be
// called exactly when the turn's critical section ends; it is safe to call
// more than once (subsequent calls are no-ops). err is non-nil only when
// ctx ended before acquisition — infrastructure failures fail open and
// return a working no-op release instead.
type Locker interface {
	Lock(ctx context.Context, sessionKey string) (release func(), err error)
}

// Noop returns a Locker that always grants the lease immediately
// (single-pod mode: the orchestrator's in-memory mutex is sufficient).
func Noop() Locker { return noopLocker{} }

type noopLocker struct{}

func (noopLocker) Lock(context.Context, string) (func(), error) { return func() {}, nil }

const (
	keyPrefix           = "opentalon:session:turn:"
	defaultTTL          = 30 * time.Second
	defaultPollInterval = 250 * time.Millisecond
	releaseTimeout      = 2 * time.Second
)

// renewScript extends the lease only while we still hold it (token match).
// Renewing unconditionally could resurrect a lock that expired and was
// re-acquired by another pod.
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)

// releaseScript deletes the lease only while we still hold it (token
// match), so a stale release can never delete another holder's lock.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

type redisLocker struct {
	client       redis.UniversalClient
	ttl          time.Duration
	pollInterval time.Duration
	// disableHeartbeat is a test hook: when true, acquired leases are not
	// renewed, simulating a holder that crashed mid-turn (the lease then
	// expires after ttl and a competitor may take over).
	disableHeartbeat bool
}

// Option adjusts a Redis-backed Locker.
type Option func(*redisLocker)

// WithTTL sets the lease TTL (default 30s). The heartbeat renews every
// ttl/3, so this is also the upper bound on how long a crashed holder can
// block a session.
func WithTTL(d time.Duration) Option {
	return func(r *redisLocker) {
		if d > 0 {
			r.ttl = d
		}
	}
}

// WithPollInterval sets how often a waiter retries acquisition (default 250ms).
func WithPollInterval(d time.Duration) Option {
	return func(r *redisLocker) {
		if d > 0 {
			r.pollInterval = d
		}
	}
}

// NewRedis creates a Locker backed by Redis for multi-pod deployments.
func NewRedis(client redis.UniversalClient, opts ...Option) Locker {
	r := &redisLocker{
		client:       client,
		ttl:          defaultTTL,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *redisLocker) Lock(ctx context.Context, sessionKey string) (func(), error) {
	key := keyPrefix + sessionKey
	token := newToken()

	for {
		ok, err := r.client.SetNX(ctx, key, token, r.ttl).Result()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Fail open: proceed without the distributed lock rather than
			// blocking chat on Redis being down. See the package doc.
			slog.Warn("sessionlock: redis unavailable, proceeding without distributed session lock",
				"session", sessionKey, "error", err)
			return func() {}, nil
		}
		if ok {
			break
		}
		// Another pod holds the turn lease — poll until it releases (or its
		// lease expires) or our context ends. No acquire timeout by design.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.pollInterval):
		}
	}

	// Heartbeat: renew the lease every ttl/3 while the turn runs. Detached
	// from the caller's ctx so a turn that outlives its request context
	// (e.g. during teardown) still keeps its lease until release is called.
	hbCtx, stopHB := context.WithCancel(context.Background())
	hbDone := make(chan struct{})
	if r.disableHeartbeat {
		close(hbDone)
	} else {
		go r.heartbeat(hbCtx, key, token, sessionKey, hbDone)
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			stopHB()
			<-hbDone
			relCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			// Compare-token-and-delete: a no-op if the lease expired and was
			// re-acquired by another holder in the meantime.
			if err := releaseScript.Run(relCtx, r.client, []string{key}, token).Err(); err != nil && err != redis.Nil {
				slog.Warn("sessionlock: release failed; lease will expire via TTL",
					"session", sessionKey, "error", err)
			}
		})
	}
	return release, nil
}

// heartbeat renews the lease until ctx is cancelled (by release) or the
// lease is observed lost (token no longer matches — e.g. it expired during
// a Redis outage and another pod took over; we back off instead of
// fighting the new holder).
func (r *redisLocker) heartbeat(ctx context.Context, key, token, sessionKey string, done chan<- struct{}) {
	defer close(done)
	ttlMillis := r.ttl.Milliseconds()
	ticker := time.NewTicker(r.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewed, err := renewScript.Run(ctx, r.client, []string{key}, token, ttlMillis).Int()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Fail open: keep trying — a transient Redis error must not kill
			// the lease renewal; if the outage outlasts the TTL the lease
			// simply expires (degraded, but chat keeps working).
			slog.Warn("sessionlock: lease renewal failed, retrying",
				"session", sessionKey, "error", err)
			continue
		}
		if renewed == 0 {
			slog.Warn("sessionlock: lease lost (expired or taken over); stopping renewal",
				"session", sessionKey)
			return
		}
	}
}

func newToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively fatal elsewhere; fall back to a
		// time-derived token rather than panicking in the chat path.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}
