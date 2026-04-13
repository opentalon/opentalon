//go:build redis

package plugin

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
)

// execRedisURL returns the Redis URL from REDIS_URL env var or skips the test.
func execRedisURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set; skipping exec dispatcher integration tests")
	}
	return url
}

// newTestRedisClient creates a Redis client for tests and registers cleanup.
func newTestRedisClient(t *testing.T) redis.UniversalClient {
	t.Helper()
	opts, err := redis.ParseURL(execRedisURL(t))
	if err != nil {
		t.Fatalf("parse REDIS_URL: %v", err)
	}
	c := redis.NewClient(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// stubRunner is a test ExecActionRunner that captures call arguments and returns
// a configurable result.
type stubRunner struct {
	mu      sync.Mutex
	calls   []stubCall
	result  string
	runErr  error
}

type stubCall struct {
	ctx    context.Context
	plugin string
	action string
	args   map[string]string
}

func (s *stubRunner) RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{ctx: ctx, plugin: plugin, action: action, args: args})
	s.mu.Unlock()
	return s.result, s.runErr
}

func (s *stubRunner) lastCall() (stubCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return stubCall{}, false
	}
	return s.calls[len(s.calls)-1], true
}

func (s *stubRunner) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// waitReply polls for a message on the reply stream with a timeout.
func waitReply(t *testing.T, client redis.UniversalClient, replyStream string) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := client.XRange(context.Background(), replyStream, "-", "+").Result()
		if err == nil && len(msgs) > 0 {
			return msgs[0].Values
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for reply on stream %s", replyStream)
	return nil
}

// waitPendingCount polls until the pending count for the consumer group equals want.
func waitPendingCount(t *testing.T, client redis.UniversalClient, stream, group string, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, err := client.XPending(context.Background(), stream, group).Result()
		if err == nil && info.Count == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := client.XPending(context.Background(), stream, group).Result()
	t.Fatalf("timed out waiting for pending count=%d; got %d", want, info.Count)
}

// waitRunnerCalled polls until the stub runner has at least n calls.
func waitRunnerCalled(t *testing.T, runner *stubRunner, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runner.callCount() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d RunAction call(s); got %d", n, runner.callCount())
}

// cleanupStreams deletes test-specific streams.
func cleanupStreams(t *testing.T, client redis.UniversalClient, keys ...string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, k := range keys {
			_ = client.Del(ctx, k).Err()
		}
	})
}

// uniqueStream returns a stream name unique to this test to avoid cross-test interference.
// We override execStream by launching the dispatcher with a dedicated exec stream key; the
// dispatcher uses the package-level constant, so instead we test using XAdd directly and
// let the dispatcher read from execStream — we isolate by using unique request IDs and
// dedicated reply streams.
func uniqueID(t *testing.T, suffix string) string {
	return fmt.Sprintf("test:%s:%d:%s", t.Name(), time.Now().UnixNano(), suffix)
}

// startDispatcher starts an ExecDispatcher with the given runner and returns a cancel func.
// The caller must call cancel() at the end of the test.
func startDispatcher(t *testing.T, client redis.UniversalClient, runner ExecActionRunner) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := NewExecDispatcher(client, runner, 10*time.Second)
	go d.Start(ctx)
	t.Cleanup(func() { cancel() })
	return cancel
}

// TestExecDispatcher_EndToEnd verifies the full XADD → handle → reply flow:
// 1. Publish a well-formed request to the exec stream.
// 2. Dispatcher handles it, calls RunAction.
// 3. Reply appears on the reply stream.
// 4. Message is acknowledged (pending count drops to 0).
func TestExecDispatcher_EndToEnd(t *testing.T) {
	client := newTestRedisClient(t)
	ctx := context.Background()

	reqID := uniqueID(t, "req")
	replyStream := fmt.Sprintf(replyStreamFmt, reqID)
	cleanupStreams(t, client, replyStream)

	runner := &stubRunner{result: "action-result"}
	startDispatcher(t, client, runner)

	// Give the dispatcher a moment to create the consumer group and start blocking.
	time.Sleep(100 * time.Millisecond)

	_, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: execStream,
		Values: map[string]interface{}{
			"id":     reqID,
			"plugin": "test-plugin",
			"action": "test-action",
			"args":   `{"key":"value"}`,
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}

	// Wait for RunAction to be called.
	waitRunnerCalled(t, runner, 1)

	call, ok := runner.lastCall()
	if !ok {
		t.Fatal("no RunAction call recorded")
	}
	if call.plugin != "test-plugin" {
		t.Errorf("plugin = %q, want %q", call.plugin, "test-plugin")
	}
	if call.action != "test-action" {
		t.Errorf("action = %q, want %q", call.action, "test-action")
	}
	if call.args["key"] != "value" {
		t.Errorf("args[key] = %q, want %q", call.args["key"], "value")
	}

	// Verify reply on reply stream.
	vals := waitReply(t, client, replyStream)
	if got, _ := vals["content"].(string); got != "action-result" {
		t.Errorf("reply content = %q, want %q", got, "action-result")
	}
	if errVal, _ := vals["error"].(string); errVal != "" {
		t.Errorf("unexpected error in reply: %q", errVal)
	}

	// Message must be acknowledged (pending count = 0).
	waitPendingCount(t, client, execStream, consumerGroup, 0)
}

// TestExecDispatcher_ContextPropagation verifies that entity_id, group_id, channel_id,
// and session_id from the stream entry are injected into the context seen by RunAction.
func TestExecDispatcher_ContextPropagation(t *testing.T) {
	client := newTestRedisClient(t)
	ctx := context.Background()

	reqID := uniqueID(t, "ctx")
	replyStream := fmt.Sprintf(replyStreamFmt, reqID)
	cleanupStreams(t, client, replyStream)

	runner := &stubRunner{result: "ok"}
	startDispatcher(t, client, runner)

	time.Sleep(100 * time.Millisecond)

	_, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: execStream,
		Values: map[string]interface{}{
			"id":         reqID,
			"plugin":     "ctx-plugin",
			"action":     "ctx-action",
			"entity_id":  "user-42",
			"group_id":   "acme-corp",
			"channel_id": "C999",
			"session_id": "sess-abc",
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}

	waitRunnerCalled(t, runner, 1)

	call, ok := runner.lastCall()
	if !ok {
		t.Fatal("no RunAction call recorded")
	}

	// Assert profile is set in context.
	p := profile.FromContext(call.ctx)
	if p == nil {
		t.Fatal("profile.FromContext returned nil")
	}
	if p.EntityID != "user-42" {
		t.Errorf("profile.EntityID = %q, want %q", p.EntityID, "user-42")
	}
	if p.Group != "acme-corp" {
		t.Errorf("profile.Group = %q, want %q", p.Group, "acme-corp")
	}
	if p.ChannelID != "C999" {
		t.Errorf("profile.ChannelID = %q, want %q", p.ChannelID, "C999")
	}

	// Assert actor is set in context.
	if got := actor.Actor(call.ctx); got != "user-42" {
		t.Errorf("actor.Actor = %q, want %q", got, "user-42")
	}

	// Assert session ID is set in context.
	if got := actor.SessionID(call.ctx); got != "sess-abc" {
		t.Errorf("actor.SessionID = %q, want %q", got, "sess-abc")
	}

	waitReply(t, client, replyStream)
}

// TestExecDispatcher_ErrorPropagation verifies that a RunAction error is written
// to the reply stream's "error" field.
func TestExecDispatcher_ErrorPropagation(t *testing.T) {
	client := newTestRedisClient(t)
	ctx := context.Background()

	reqID := uniqueID(t, "err")
	replyStream := fmt.Sprintf(replyStreamFmt, reqID)
	cleanupStreams(t, client, replyStream)

	runner := &stubRunner{result: "", runErr: fmt.Errorf("plugin returned an error")}
	startDispatcher(t, client, runner)

	time.Sleep(100 * time.Millisecond)

	_, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: execStream,
		Values: map[string]interface{}{
			"id":     reqID,
			"plugin": "failing-plugin",
			"action": "failing-action",
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}

	waitRunnerCalled(t, runner, 1)

	vals := waitReply(t, client, replyStream)
	errVal, _ := vals["error"].(string)
	if errVal == "" {
		t.Error("expected non-empty error field in reply")
	}
	if errVal != "plugin returned an error" {
		t.Errorf("error field = %q, want %q", errVal, "plugin returned an error")
	}

	waitPendingCount(t, client, execStream, consumerGroup, 0)
}

// TestExecDispatcher_InvalidRequest verifies that a malformed stream entry
// (missing required fields) is acknowledged without writing a reply.
func TestExecDispatcher_InvalidRequest(t *testing.T) {
	client := newTestRedisClient(t)
	ctx := context.Background()

	runner := &stubRunner{}
	startDispatcher(t, client, runner)

	time.Sleep(100 * time.Millisecond)

	// Missing "id" field — should be rejected.
	_, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: execStream,
		Values: map[string]interface{}{
			"plugin": "test-plugin",
			"action": "test-action",
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}

	// RunAction must NOT be called for an invalid request.
	time.Sleep(300 * time.Millisecond)
	if runner.callCount() != 0 {
		t.Errorf("RunAction called %d time(s), expected 0 for invalid request", runner.callCount())
	}

	// Entry must still be acknowledged (not stuck in PEL).
	waitPendingCount(t, client, execStream, consumerGroup, 0)
}

// TestExecDispatcher_NoContextWithoutEntityID verifies that when entity_id and
// group_id are absent, no profile is injected into the context (i.e. the caller
// cannot accidentally inherit a prior request's profile).
func TestExecDispatcher_NoContextWithoutEntityID(t *testing.T) {
	client := newTestRedisClient(t)
	ctx := context.Background()

	reqID := uniqueID(t, "noauth")
	replyStream := fmt.Sprintf(replyStreamFmt, reqID)
	cleanupStreams(t, client, replyStream)

	runner := &stubRunner{result: "ok"}
	startDispatcher(t, client, runner)

	time.Sleep(100 * time.Millisecond)

	_, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: execStream,
		Values: map[string]interface{}{
			"id":     reqID,
			"plugin": "anon-plugin",
			"action": "anon-action",
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}

	waitRunnerCalled(t, runner, 1)

	call, ok := runner.lastCall()
	if !ok {
		t.Fatal("no RunAction call recorded")
	}

	if p := profile.FromContext(call.ctx); p != nil {
		t.Errorf("expected no profile in context, got %+v", p)
	}
	if got := actor.Actor(call.ctx); got != "" {
		t.Errorf("expected no actor in context, got %q", got)
	}
	if got := actor.SessionID(call.ctx); got != "" {
		t.Errorf("expected no session ID in context, got %q", got)
	}

	waitReply(t, client, replyStream)
}
