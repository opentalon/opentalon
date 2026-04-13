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

	// defaultActionTimeout is used when the operator does not set action_timeout.
	defaultActionTimeout = 60 * time.Second
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
	client        redis.UniversalClient
	runner        ExecActionRunner
	consumerID    string        // unique per process; e.g. "hostname" or "hostname-fallback"
	sem           chan struct{} // bounds concurrent handlers
	actionTimeout time.Duration // per-request RunAction deadline
	done          chan struct{} // closed when Start returns (all in-flight handlers drained)
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
// actionTimeout is the per-request deadline for RunAction; 0 uses defaultActionTimeout.
func NewExecDispatcher(client redis.UniversalClient, runner ExecActionRunner, actionTimeout time.Duration) *ExecDispatcher {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = fmt.Sprintf("pod-%d", os.Getpid())
	}
	if actionTimeout <= 0 {
		actionTimeout = defaultActionTimeout
	}
	return &ExecDispatcher{
		client:        client,
		runner:        runner,
		consumerID:    hostname,
		sem:           make(chan struct{}, maxConcurrentHandlers),
		actionTimeout: actionTimeout,
		done:          make(chan struct{}),
	}
}

// Start runs the dispatcher loop until ctx is cancelled.
// It blocks until all in-flight handlers finish before returning,
// then closes the done channel so Wait() callers are unblocked.
func (d *ExecDispatcher) Start(ctx context.Context) {
	defer close(d.done)
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
				select {
				case d.sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
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
			// Loop until XAutoClaim returns cursor "0-0", meaning the entire PEL
			// has been scanned. A single call only returns up to COUNT entries
			// (default 100), so without this loop entries beyond the first page
			// are never reclaimed within a tick, and because Start always resets
			// to "0-0" the same head entries can repeatedly starve the tail.
			cursor := "0-0"
			for {
				msgs, next, err := d.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
					Stream:   execStream,
					Group:    consumerGroup,
					Consumer: d.consumerID,
					MinIdle:  autoclaimMinIdle,
					Start:    cursor,
				}).Result()
				if err != nil {
					slog.Warn("plugin exec: xautoclaim failed", "error", err)
					break
				}
				for _, msg := range msgs {
					select {
					case d.sem <- struct{}{}:
					case <-ctx.Done():
						return
					}
					wg.Add(1)
					go func(m redis.XMessage) {
						defer wg.Done()
						defer func() { <-d.sem }()
						d.handle(ctx, m)
					}(msg)
				}
				if next == "" || next == "0-0" {
					break
				}
				cursor = next
				// Yield between pages so the main XReadGroup loop isn't starved.
				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}
	}
}

func (d *ExecDispatcher) handle(ctx context.Context, msg redis.XMessage) {
	// replyCtx is used for all Redis writes after the action completes (XAdd reply,
	// Expire, XAck). It must survive parent ctx cancellation so that graceful shutdown
	// does not silently drop the reply and leave the plugin-side waiter hanging until
	// its own timeout, while still acking the message (losing the retry).
	replyCtx, replyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer replyCancel()

	// req is declared here so the recover closure below can write the error reply
	// using req.ID even when the panic occurs after parseExecRequest returns.
	var req execRequest

	// Recover from panics in RunAction or any downstream code. Registered first so
	// it fires last (LIFO), after XAck has already removed the message from the PEL.
	// Without this, a panic kills the process and every plugin waiter hangs until
	// its own request timeout.
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		slog.Error("plugin exec: panic in handler", "msg_id", msg.ID, "req_id", req.ID, "panic", r)
		if req.ID == "" {
			return
		}
		replyStream := fmt.Sprintf(replyStreamFmt, req.ID)
		_ = d.client.XAdd(replyCtx, &redis.XAddArgs{
			Stream: replyStream,
			MaxLen: 1,
			Values: map[string]any{"error": fmt.Sprintf("internal error: %v", r)},
		}).Err()
		d.client.Expire(replyCtx, replyStream, replyTTL) //nolint:errcheck
	}()

	defer func() {
		if err := d.client.XAck(replyCtx, execStream, consumerGroup, msg.ID).Err(); err != nil {
			slog.Warn("plugin exec: ack failed", "msg_id", msg.ID, "error", err)
		}
	}()

	var err error
	req, err = parseExecRequest(msg)
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

	actionCtx, actionCancel := context.WithTimeout(reqCtx, d.actionTimeout)
	defer actionCancel()
	content, runErr := d.runner.RunAction(actionCtx, req.Plugin, req.Action, req.Args)

	fields := map[string]any{"content": content}
	if runErr != nil {
		fields["error"] = runErr.Error()
	}

	replyStream := fmt.Sprintf(replyStreamFmt, req.ID)
	if err := d.client.XAdd(replyCtx, &redis.XAddArgs{
		Stream: replyStream,
		MaxLen: 1,
		Values: fields,
	}).Err(); err != nil {
		slog.Warn("plugin exec: write reply failed", "req_id", req.ID, "error", err)
		return
	}
	d.client.Expire(replyCtx, replyStream, replyTTL) //nolint:errcheck
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

// Wait blocks until Start has returned and all in-flight handlers have finished.
// Must be called after the context passed to Start is cancelled.
func (d *ExecDispatcher) Wait() {
	<-d.done
}

// Close shuts down the Redis client.
func (d *ExecDispatcher) Close() error {
	return d.client.Close()
}
