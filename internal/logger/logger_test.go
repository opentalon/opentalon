package logger

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

func TestTraceIDFromSessionKeyDeterministic(t *testing.T) {
	id1 := TraceIDFromSessionKey("slack:conv1:thread1")
	id2 := TraceIDFromSessionKey("slack:conv1:thread1")
	if id1 != id2 {
		t.Errorf("same key produced different IDs: %q vs %q", id1, id2)
	}
	if len(id1) != 16 {
		t.Errorf("expected 16 hex chars, got %d: %q", len(id1), id1)
	}
}

func TestTraceIDFromSessionKeyUnique(t *testing.T) {
	id1 := TraceIDFromSessionKey("slack:conv1:thread1")
	id2 := TraceIDFromSessionKey("slack:conv2:thread2")
	if id1 == id2 {
		t.Error("different keys produced same ID")
	}
}

func TestContextTraceID(t *testing.T) {
	ctx := context.Background()
	if got := TraceID(ctx); got != "" {
		t.Errorf("expected empty trace_id, got %q", got)
	}
	ctx = WithTraceID(ctx, "abc123")
	if got := TraceID(ctx); got != "abc123" {
		t.Errorf("expected abc123, got %q", got)
	}
}

func TestWithTraceIDEmpty(t *testing.T) {
	ctx := context.Background()
	ctx2 := WithTraceID(ctx, "")
	if ctx != ctx2 {
		t.Error("empty trace_id should return same context")
	}
}

func TestTraceIDEmptyContext(t *testing.T) {
	if got := TraceID(context.Background()); got != "" {
		t.Errorf("empty context should return empty trace_id, got %q", got)
	}
}

func TestFromContext(t *testing.T) {
	ctx := WithTraceID(context.Background(), "test-trace")
	l := FromContext(ctx)
	if l.traceID != "test-trace" {
		t.Errorf("expected test-trace, got %q", l.traceID)
	}
}

func TestLoggerAttrs(t *testing.T) {
	l := New("abc")
	attrs := l.attrs()
	if len(attrs) != 2 || attrs[0] != "trace_id" || attrs[1] != "abc" {
		t.Errorf("unexpected attrs: %v", attrs)
	}

	l2 := New("")
	if l2.attrs() != nil {
		t.Error("empty trace_id should return nil attrs")
	}
}

func TestSetupLevels(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"DEBUG", slog.LevelDebug},
	}
	for _, tt := range tests {
		Setup(tt.level)
		if !slog.Default().Enabled(context.Background(), tt.want) {
			t.Errorf("Setup(%q): expected level %v to be enabled", tt.level, tt.want)
		}
	}
}

func TestLoggerOutputIncludesTraceID(t *testing.T) {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))

	l := New("trace-xyz")
	l.Info("hello", "key", "val")

	out := buf.String()
	if !bytes.Contains([]byte(out), []byte("trace_id=trace-xyz")) {
		t.Errorf("expected trace_id in output, got: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("hello")) {
		t.Errorf("expected message in output, got: %s", out)
	}
}
