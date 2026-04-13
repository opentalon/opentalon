// Package redisclient provides a single Redis client factory used by all
// subsystems (cluster dedup, plugin exec dispatcher) that need a shared pool.
package redisclient

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// New creates and pings a Redis client. When sentinels is non-empty a Sentinel
// failover client is returned; otherwise redisURL is parsed as a redis:// or
// rediss:// URL. The caller is responsible for closing the returned client.
func New(redisURL, masterName string, sentinels []string, password, sentinelPassword string) (redis.UniversalClient, error) {
	var client redis.UniversalClient
	if len(sentinels) > 0 {
		if masterName == "" {
			return nil, fmt.Errorf("redis: master_name is required for Sentinel mode")
		}
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       masterName,
			SentinelAddrs:    sentinels,
			Password:         password,
			SentinelPassword: sentinelPassword,
		})
	} else {
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			return nil, fmt.Errorf("redis: parsing redis URL: %w", err)
		}
		client = redis.NewClient(opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: connecting to redis: %w", err)
	}
	return client, nil
}
