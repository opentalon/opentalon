package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSessionDebug(t *testing.T) {
	if IsSessionDebug(context.Background()) {
		t.Error("background ctx must not have session-debug flag")
	}
	// IsSessionDebug must guard against a nil ctx for safety, even though
	// callers should not pass one. Using a typed-nil var avoids the
	// staticcheck warning for passing a literal nil to context-typed args.
	var nilCtx context.Context
	if IsSessionDebug(nilCtx) {
		t.Error("nil ctx must return false")
	}
	ctx := WithSessionDebug(context.Background())
	if !IsSessionDebug(ctx) {
		t.Error("WithSessionDebug ctx should report true")
	}
}

// TestSessionDebugHandlerPromotesDebug verifies the central guarantee: a
// Debug record on a session-debug ctx is emitted at Info even when the
// underlying handler is Info-level.
func TestSessionDebugHandlerPromotesDebug(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewSessionDebugHandler(base))

	// Without flag: Debug record dropped.
	logger.DebugContext(context.Background(), "should-be-dropped", "k", "v")
	if buf.Len() > 0 {
		t.Errorf("expected no output, got %q", buf.String())
	}

	// With flag: same Debug call emits at Info.
	logger.DebugContext(WithSessionDebug(context.Background()), "promoted", "k", "v")
	if buf.Len() == 0 {
		t.Fatal("expected promoted record on stderr, got nothing")
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("emitted record not valid JSON: %v", err)
	}
	if rec["level"] != "INFO" {
		t.Errorf("level = %v, want INFO (promoted)", rec["level"])
	}
	if rec["msg"] != "promoted" {
		t.Errorf("msg = %v, want %q", rec["msg"], "promoted")
	}
	if rec["session_debug"] != true {
		t.Errorf("session_debug attr missing or wrong value: %v", rec["session_debug"])
	}
	if rec["k"] != "v" {
		t.Errorf("attr k missing: rec=%v", rec)
	}
}

// TestSessionDebugHandlerLeavesNonDebugAlone ensures that records at Info
// or higher pass through unchanged whether or not the flag is set — the
// wrapper only tinkers with promoted Debug records.
func TestSessionDebugHandlerLeavesNonDebugAlone(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewSessionDebugHandler(base))

	logger.InfoContext(WithSessionDebug(context.Background()), "info-with-flag")
	if !strings.Contains(buf.String(), `"level":"INFO"`) {
		t.Errorf("Info record should pass through unchanged: %q", buf.String())
	}
	if strings.Contains(buf.String(), `"session_debug":true`) {
		t.Errorf("Info-level records must NOT be tagged with session_debug; got %q", buf.String())
	}
}

// TestSessionDebugHandlerLogLevelDebugStillWorks ensures the wrapper does
// not break the LOG_LEVEL=debug case — Debug records still flow even
// without the per-session flag.
func TestSessionDebugHandlerLogLevelDebugStillWorks(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewSessionDebugHandler(base))

	logger.DebugContext(context.Background(), "global-debug")
	if !strings.Contains(buf.String(), `"msg":"global-debug"`) {
		t.Errorf("Debug record should flow when global level is Debug; got %q", buf.String())
	}
	if strings.Contains(buf.String(), `"session_debug":true`) {
		t.Errorf("global Debug records must not be tagged session_debug; got %q", buf.String())
	}
}
