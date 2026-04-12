package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
)

const (
	execStream      = "opentalon:plugin-exec"
	replyStreamFmt  = "opentalon:plugin-exec-reply:%s"
	consumerGroup   = "opentalon-dispatcher"
	consumerName    = "dispatcher"
	execReadTimeout = 5 * time.Second
	replyTTL        = 5 * time.Minute
)

// ExecActionRunner executes a named plugin action. Implemented by orchestrator.Orchestrator.
type ExecActionRunner interface {
	RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error)
}

// ExecDispatcher reads plugin-exec requests from a Redis stream and dispatches them through
// the ToolRegistry. Trusted plugins (e.g. opentalon-workflows) write requests to the stream;
// the dispatcher executes them with the correct actor/profile context and writes results to
// a per-request reply stream that the plugin reads.
type ExecDispatcher struct {
	client redis.UniversalClient
	runner ExecActionRunner
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
	return &ExecDispatcher{client: client, runner: runner}
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
func (d *ExecDispatcher) Start(ctx context.Context) {
	if err := d.client.XGroupCreateMkStream(ctx, execStream, consumerGroup, "$").Err(); err != nil {
		// BUSYGROUP means the group already exists — that's fine.
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			slog.Error("plugin exec: create consumer group failed", "error", err)
			return
		}
	}
	slog.Info("plugin exec dispatcher started", "stream", execStream)
	for {
		streams, err := d.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumerGroup,
			Consumer: consumerName,
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
				go d.handle(ctx, msg)
			}
		}
	}
}

func (d *ExecDispatcher) handle(ctx context.Context, msg redis.XMessage) {
	defer func() {
		if err := d.client.XAck(ctx, execStream, consumerGroup, msg.ID).Err(); err != nil {
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
	d.client.Expire(ctx, replyStream, replyTTL)
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
