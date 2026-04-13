// Package dedup provides Redis-backed message deduplication for multi-pod deployments.
// When multiple pods receive the same inbound message simultaneously, only the pod that
// successfully acquires the lock (SET NX) will process it; others silently skip it.
package dedup

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// Deduplicator claims exclusive processing rights for a message key.
// TryAcquire returns true when this pod wins the lock and should process the message.
// It returns false when another pod already claimed the key.
type Deduplicator interface {
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Close() error
}

type redisDedup struct {
	client   redis.UniversalClient
	ownerVal string // stored as the lock value; aids debugging ("who owns this key?")
	owned    bool   // true = this struct owns the client and must close it
}

// NewFromClient wraps an already-connected redis.UniversalClient.
// Close is a no-op: the caller owns the client and is responsible for closing it.
// Use this when the same Redis connection is shared with another subsystem (e.g. the
// exec dispatcher) to avoid opening a second pool to the same instance.
func NewFromClient(client redis.UniversalClient) Deduplicator {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("pid-%d", os.Getpid())
	}
	return &redisDedup{client: client, ownerVal: hostname, owned: false}
}

// TryAcquire sets key with NX+EX. Returns true if this call created the key (won the lock).
// The value stored is the pod's hostname so operators can inspect who holds a lock via
// `redis-cli GET <key>`.
func (d *redisDedup) TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return d.client.SetNX(ctx, key, d.ownerVal, ttl).Result()
}

func (d *redisDedup) Close() error {
	if d.owned {
		return d.client.Close()
	}
	return nil
}
