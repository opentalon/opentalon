package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
)

const (
	execStream     = "opentalon:plugin-exec"
	replyStreamFmt = "opentalon:plugin-exec-reply:%s"
	consumerGroup  = "opentalon-dispatcher"

	execReadTimeout   = 5 * time.Second
	replyTTL          = 5 * time.Minute
	autoclaimMinIdle  = 2 * time.Minute
	autoclaimInterval = 30 * time.Second

	// maxConcurrentHandlers caps the number of plugin-exec requests handled in
	// parallel. Prevents unbounded goroutine growth under stream bursts.
	maxConcurrentHandlers = 32
)

// ExecActionRunner executes a named plugin action. Implemented by orchestrator.Orchestrator.
type ExecActionRunner interface {
	RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error)
}

// ExecDispatcher reads plugin-exec requests from a Redis stream and dispatches them through
// the ToolRegistry. Plugins write requests to the stream; the dispatcher executes them with
// the correct actor/profile context and writes results to a per-request reply stream.
//
// Security: the opentalon:plugin-exec stream is the trust boundary. Any process with
// XADD on this stream can execute plugin actions impersonating any entity_id / group_id.
// The Redis instance used here MUST be network-isolated (not shared with external services)
// and its credentials must be treated as high-privilege secrets.
type ExecDispatcher struct {
	client     redis.UniversalClient
	runner     ExecActionRunner
	consumerID string        // unique per process; e.g. "hostname" or "hostname-fallback"
	sem        chan struct{} // bounds concurrent handlers
}

type execRequest struct {
	ID        string
	Plugin    string
	Action    string
	Args      map[string]string
	EntityID  string
	GroupID   string
	ChannelID string
	SessionID string
}

// NewExecDispatcher creates a dispatcher backed by the given Redis client.
func NewExecDispatcher(client redis.UniversalClient, runner ExecActionRunner) *ExecDispatcher {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("pod-%d", os.Getpid())
	}
	return &ExecDispatcher{
		client:     client,
		runner:     runner,
		consumerID: hostname,
		sem:        make(chan struct{}, maxConcurrentHandlers),
	}
}

// NewExecRedisClient creates a Redis client for the exec dispatcher.
// Accepts the same cluster config values used for deduplication.
func NewExecRedisClient(redisURL, masterName string, sentinels []string, password, sentinelPassword string) (redis.UniversalClient, error) {
	var client redis.UniversalClient
	if len(sentinels) > 0 {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       masterName,
			SentinelAddrs:    sentinels,
			Password:         password,
			SentinelPassword: sentinelPassword,
		})
	} else {
		opts, err := redis.ParseURL(redisURL)
		if err != nil {
			return nil, fmt.Errorf("plugin exec: parsing redis URL: %w", err)
		}
		client = redis.NewClient(opts)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("plugin exec: connecting to redis: %w", err)
	}
	return client, nil
}

// Start runs the dispatcher loop until ctx is cancelled.
// It blocks until all in-flight handlers finish before returning.
func (d *ExecDispatcher) Start(ctx context.Context) {
	if err := d.client.XGroupCreateMkStream(ctx, execStream, consumerGroup, "$").Err(); err != nil {
		// BUSYGROUP means the group already exists — that's fine.
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			slog.Error("plugin exec: create consumer group failed", "error", err)
			return
		}
	}
	slog.Info("plugin exec dispatcher started", "stream", execStream, "consumer", d.consumerID)

	var wg sync.WaitGroup
	defer wg.Wait() // drain in-flight handlers before returning

	// Reclaim stale pending entries from dead consumers periodically.
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.autoclaimLoop(ctx, &wg)
	}()

	for {
		streams, err := d.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumerGroup,
			Consumer: d.consumerID,
			Streams:  []string{execStream, ">"},
			Count:    10,
			Block:    execReadTimeout,
		}).Result()
		if ctx.Err() != nil {
			return
		}
		if err == redis.Nil {
			continue
		}
		if err != nil {
			slog.Warn("plugin exec: xreadgroup error", "error", err)
			time.Sleep(time.Second)
			continue
		}
		for _, stream := range streams {
			for _, msg := range stream.Messages {
				d.sem <- struct{}{} // acquire semaphore; blocks if at capacity
				wg.Add(1)
				go func(m redis.XMessage) {
					defer wg.Done()
					defer func() { <-d.sem }()
					d.handle(ctx, m)
				}(msg)
			}
		}
	}
}

// autoclaimLoop periodically reclaims pending entries that have been idle
// longer than autoclaimMinIdle (i.e. owned by a consumer that died).
func (d *ExecDispatcher) autoclaimLoop(ctx context.Context, wg *sync.WaitGroup) {
	ticker := time.NewTicker(autoclaimInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msgs, _, err := d.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
				Stream:   execStream,
				Group:    consumerGroup,
				Consumer: d.consumerID,
				MinIdle:  autoclaimMinIdle,
				Start:    "0-0",
			}).Result()
			if err != nil {
				slog.Warn("plugin exec: xautoclaim failed", "error", err)
				continue
			}
			for _, msg := range msgs {
				d.sem <- struct{}{}
				wg.Add(1)
				go func(m redis.XMessage) {
					defer wg.Done()
					defer func() { <-d.sem }()
					d.handle(ctx, m)
				}(msg)
			}
		}
	}
}

func (d *ExecDispatcher) handle(ctx context.Context, msg redis.XMessage) {
	// Use a fresh context for XAck so it succeeds even if ctx was cancelled
	// (e.g. during graceful shutdown — work is done, we still need to ack).
	defer func() {
		ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := d.client.XAck(ackCtx, execStream, consumerGroup, msg.ID).Err(); err != nil {
			slog.Warn("plugin exec: ack failed", "msg_id", msg.ID, "error", err)
		}
	}()

	req, err := parseExecRequest(msg)
	if err != nil {
		slog.Warn("plugin exec: invalid request", "msg_id", msg.ID, "error", err)
		return
	}

	reqCtx := ctx
	if req.EntityID != "" || req.GroupID != "" {
		p := &profile.Profile{
			EntityID:  req.EntityID,
			Group:     req.GroupID,
			ChannelID: req.ChannelID,
		}
		reqCtx = profile.WithProfile(reqCtx, p)
		reqCtx = actor.WithActor(reqCtx, req.EntityID)
	}
	if req.SessionID != "" {
		reqCtx = actor.WithSessionID(reqCtx, req.SessionID)
	}

	content, runErr := d.runner.RunAction(reqCtx, req.Plugin, req.Action, req.Args)

	fields := map[string]any{"content": content}
	if runErr != nil {
		fields["error"] = runErr.Error()
	}

	replyStream := fmt.Sprintf(replyStreamFmt, req.ID)
	if err := d.client.XAdd(ctx, &redis.XAddArgs{
		Stream: replyStream,
		MaxLen: 1,
		Values: fields,
	}).Err(); err != nil {
		slog.Warn("plugin exec: write reply failed", "req_id", req.ID, "error", err)
		return
	}
	d.client.Expire(ctx, replyStream, replyTTL) //nolint:errcheck
}

func parseExecRequest(msg redis.XMessage) (execRequest, error) {
	get := func(key string) string {
		if v, ok := msg.Values[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}

	req := execRequest{
		ID:        get("id"),
		Plugin:    get("plugin"),
		Action:    get("action"),
		EntityID:  get("entity_id"),
		GroupID:   get("group_id"),
		ChannelID: get("channel_id"),
		SessionID: get("session_id"),
	}
	if req.ID == "" {
		return req, fmt.Errorf("missing id")
	}
	if req.Plugin == "" || req.Action == "" {
		return req, fmt.Errorf("missing plugin or action")
	}
	if argsJSON := get("args"); argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &req.Args); err != nil {
			return req, fmt.Errorf("invalid args JSON: %w", err)
		}
	}
	return req, nil
}

// Close shuts down the Redis client.
func (d *ExecDispatcher) Close() error {
	return d.client.Close()
}
