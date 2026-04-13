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
}

// NewStandalone returns a Deduplicator backed by a single Redis instance.
// redisURL must be a valid redis:// or rediss:// URL (password may be embedded).
func NewStandalone(redisURL string) (Deduplicator, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("dedup: parsing redis URL: %w", err)
	}
	return newDedup(redis.NewClient(opts))
}

// NewSentinel returns a Deduplicator backed by Redis Sentinel.
// masterName is the name of the monitored master. sentinels is a list of host:port
// addresses for the Sentinel nodes. password is the Redis master auth password
// (empty = no auth). sentinelPassword is the optional Sentinel ACL password.
func NewSentinel(masterName string, sentinels []string, password, sentinelPassword string) (Deduplicator, error) {
	if masterName == "" {
		return nil, fmt.Errorf("dedup: master_name is required for Sentinel mode")
	}
	if len(sentinels) == 0 {
		return nil, fmt.Errorf("dedup: at least one sentinel address is required")
	}
	client := redis.NewFailoverClient(&redis.FailoverOptions{
		MasterName:       masterName,
		SentinelAddrs:    sentinels,
		Password:         password,
		SentinelPassword: sentinelPassword,
	})
	return newDedup(client)
}

func newDedup(client redis.UniversalClient) (Deduplicator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("dedup: connecting to redis: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("pid-%d", os.Getpid())
	}
	return &redisDedup{client: client, ownerVal: hostname}, nil
}

// TryAcquire sets key with NX+EX. Returns true if this call created the key (won the lock).
// The value stored is the pod's hostname so operators can inspect who holds a lock via
// `redis-cli GET <key>`.
func (d *redisDedup) TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return d.client.SetNX(ctx, key, d.ownerVal, ttl).Result()
}

func (d *redisDedup) Close() error {
	return d.client.Close()
}
