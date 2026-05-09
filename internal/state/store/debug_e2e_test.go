package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/provider"
)

// TestEndToEnd_DebugCaptureFlowsThroughEveryLayer wires up the full chain
// the way main.go does and exercises one request:
//
//  1. state-store opens (sqlite) and migration 007 creates ai_debug_events
//  2. async writer + sessionDebugHandler installed on slog.Default
//  3. provider.OpenAIProvider gets a sink + resolver
//  4. ctx is decorated like orchestrator.Run does (session_id, trace_id,
//     and the session-debug flag from metadata["debug"]=true)
//  5. provider.Complete runs against an httptest server
//
// Expectations:
//   - request and response rows land in ai_debug_events with the right
//     session_id and trace_id
//   - stderr (the captured slog buffer) contains an Info-level "openai
//     raw http" record with session_debug=true even though the underlying
//     handler is at LevelInfo (i.e. the handler promotion really fires)
//   - a request with no session-debug flag in ctx leaves the sink empty
//
// This is the only test that proves all five subsystems compose. Each
// piece has unit tests, but a refactor to any of them could silently
// break the chain.
func TestEndToEnd_DebugCaptureFlowsThroughEveryLayer(t *testing.T) {
	// 1. State store + migration.
	db := openTestDB(t)
	debugStore := NewDebugEventStore(db)

	// 2. Async writer.
	writer := NewDebugEventWriter(debugStore)
	writer.Start(context.Background())
	defer writer.Stop(2 * time.Second)

	// 3. Install the session-debug handler on slog.Default. Capture stderr
	// into a buffer so we can assert the promotion happens.
	var logBuf bytes.Buffer
	prevDefault := slog.Default()
	defer slog.SetDefault(prevDefault)
	base := slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(logger.NewSessionDebugHandler(base)))

	// 4. Mock OpenAI server.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-e2e",
			"model":   "gpt-4o",
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer server.Close()

	// 5. Sink + resolver wired the same way main.go does.
	sink := &writerSink{w: writer}
	resolver := func(ctx context.Context) (string, string, bool) {
		if !logger.IsSessionDebug(ctx) {
			return "", "", false
		}
		return actor.SessionID(ctx), logger.TraceID(ctx), true
	}
	p := provider.NewOpenAIProvider("openai", server.URL, "k", nil,
		provider.WithOpenAIDebugSink(sink),
		provider.WithOpenAIDebugResolver(resolver),
	)

	// --- Path A: session WITHOUT /debug. No capture, no slog promotion. ---
	logBuf.Reset()
	plainCtx := actor.WithSessionID(context.Background(), "sess-quiet")
	plainCtx = logger.WithTraceID(plainCtx, "trace-quiet")
	if _, err := p.Complete(plainCtx, &provider.CompletionRequest{
		Model: "gpt-4o", Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("plain Complete: %v", err)
	}
	if got := writer.dropped.Load(); got != 0 {
		t.Errorf("plain path dropped %d events; want 0", got)
	}
	if strings.Contains(logBuf.String(), "openai raw http") {
		t.Errorf("plain path emitted raw http to slog at Info level: %q", logBuf.String())
	}
	// Drain anything in flight before the assertion.
	writer.Stop(2 * time.Second)
	if n, _ := debugStore.CountForSession(context.Background(), "sess-quiet"); n != 0 {
		t.Errorf("plain path produced %d rows; want 0", n)
	}

	// Restart writer for path B.
	writer = NewDebugEventWriter(debugStore)
	writer.Start(context.Background())
	defer writer.Stop(2 * time.Second)
	sink.w = writer

	// --- Path B: session WITH /debug active. Full chain populates. ---
	logBuf.Reset()
	dbgCtx := actor.WithSessionID(context.Background(), "sess-loud")
	dbgCtx = logger.WithTraceID(dbgCtx, "trace-loud")
	dbgCtx = logger.WithSessionDebug(dbgCtx)

	if _, err := p.Complete(dbgCtx, &provider.CompletionRequest{
		Model: "gpt-4o", Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("debug Complete: %v", err)
	}
	writer.Stop(2 * time.Second)

	rows, err := debugStore.CountForSession(context.Background(), "sess-loud")
	if err != nil {
		t.Fatalf("CountForSession: %v", err)
	}
	if rows != 2 {
		t.Errorf("/debug path rows = %d, want 2 (request+response)", rows)
	}
	if !strings.Contains(logBuf.String(), `"openai raw http"`) {
		t.Errorf("/debug path did not emit raw http on stderr; got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"session_debug":true`) {
		t.Errorf("/debug path missing session_debug=true tag; got: %q", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `"level":"INFO"`) {
		t.Errorf("/debug path slog level was not promoted to INFO; got: %q", logBuf.String())
	}
}

// writerSink adapts the in-package DebugEventWriter to provider.DebugEventSink
// for tests. Production wiring lives in cmd/opentalon/main.go but that's
// behind the binary; the adapter pattern is so trivial it's worth a tiny
// duplicate here to keep the e2e test self-contained.
type writerSink struct{ w *DebugEventWriter }

func (s *writerSink) Submit(_ context.Context, e provider.DebugEvent) {
	s.w.Submit(DebugEvent{
		SessionID: e.SessionID,
		TraceID:   e.TraceID,
		Direction: e.Direction,
		Status:    e.Status,
		URL:       e.URL,
		Body:      e.Body,
		Timestamp: e.Timestamp,
	})
}
