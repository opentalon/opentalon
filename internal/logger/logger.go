package logger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"log/slog"
	"os"
	"strings"
)

// Logger provides leveled logging with an embedded trace_id.
// Use FromContext to get one in session-aware code.
type Logger struct {
	traceID string
}

// FromContext returns a Logger with the trace_id extracted from ctx.
func FromContext(ctx context.Context) *Logger {
	return &Logger{traceID: TraceID(ctx)}
}

// New returns a Logger with an explicit trace_id.
func New(traceID string) *Logger {
	return &Logger{traceID: traceID}
}

func (l *Logger) attrs() []any {
	if l.traceID != "" {
		return []any{"trace_id", l.traceID}
	}
	return nil
}

func (l *Logger) Debug(msg string, args ...any) {
	slog.Debug(msg, append(l.attrs(), args...)...)
}

func (l *Logger) Info(msg string, args ...any) {
	slog.Info(msg, append(l.attrs(), args...)...)
}

func (l *Logger) Warn(msg string, args ...any) {
	slog.Warn(msg, append(l.attrs(), args...)...)
}

func (l *Logger) Error(msg string, args ...any) {
	slog.Error(msg, append(l.attrs(), args...)...)
}

// Setup configures the global slog default with the given level string
// (debug, info, warn, error). Default: info. Also redirects the old
// log.Printf through slog at Info level.
//
// The handler is JSON. Earlier versions used TextHandler, which collapsed
// multi-field events onto a single line of `key=value` pairs and quote-
// escaped any string value containing whitespace or punctuation — fine for
// short attrs but unreadable for the bigger payloads we already log
// (planner system prompts, MCP tool descriptions, the raw OpenAI HTTP
// bodies introduced for live A/B comparison). JSON keeps every attr a
// real value (nested objects, numbers, arrays stay typed), and `kubectl
// logs -f opentalon-0 | jq` becomes a usable live tail. The cost is one
// extra layer for human grep'ing — solved by piping through `jq` or a
// small awk filter — and that trade is overwhelmingly worth it as soon
// as any single attr is bigger than a screen line.
func Setup(level string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
	log.SetOutput(&slogWriter{level: slog.LevelInfo})
	log.SetFlags(0)
}

// slogWriter adapts log.Printf output to slog at a fixed level.
type slogWriter struct{ level slog.Level }

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	slog.Log(context.Background(), w.level, msg)
	return len(p), nil
}

// TraceIDFromSessionKey returns a deterministic 16-char hex trace ID
// derived from the session key.
func TraceIDFromSessionKey(sessionKey string) string {
	h := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(h[:8])
}

type traceKey struct{}

// WithTraceID returns a context carrying the given trace ID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, traceID)
}

// TraceID returns the trace ID from the context, or empty string if not set.
func TraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(traceKey{}).(string)
	return v
}
