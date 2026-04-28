package channel

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// streamWriterKey is the context key for a per-request StreamWriter.
type streamWriterKey struct{}

// WithStreamWriter stores a StreamWriter in the context.
func WithStreamWriter(ctx context.Context, sw *StreamWriter) context.Context {
	return context.WithValue(ctx, streamWriterKey{}, sw)
}

// StreamWriterFromContext retrieves the StreamWriter from ctx, or nil if none.
func StreamWriterFromContext(ctx context.Context) *StreamWriter {
	sw, _ := ctx.Value(streamWriterKey{}).(*StreamWriter)
	return sw
}

// UpdatableChannel is an optional interface that channels can implement to
// support editing a previously sent message. This enables progressive
// streaming updates (send a partial message, then update it as tokens arrive).
type UpdatableChannel interface {
	Channel
	// SendAndCapture sends a message and returns a channel-specific message ID
	// (e.g. Slack's "ts") that can be used for subsequent updates.
	// Returns "" if the channel doesn't support ID capture.
	SendAndCapture(ctx context.Context, msg OutboundMessage) (messageID string, err error)
	// SendUpdate replaces the content of a previously sent message.
	// messageID is the identifier returned by SendAndCapture.
	SendUpdate(ctx context.Context, messageID string, msg OutboundMessage) error
}

// StreamWriter buffers streaming LLM chunks and progressively delivers them
// to a channel. It debounces updates to avoid flooding the channel API.
//
// Usage: the handler creates a StreamWriter before calling Run(), stores it
// in the context, and the orchestrator's streaming callback invokes OnChunk.
type StreamWriter struct {
	ch       Channel
	convID   string
	threadID string
	metadata map[string]string

	mu        sync.Mutex
	buf       strings.Builder
	lastSent  string
	messageID string // populated after first send, used for updates
	done      bool
	flushed   bool // true if at least one flush happened

	// flushInterval controls how often partial updates are sent.
	flushInterval time.Duration
	lastFlush     time.Time
	// minChunkSize is the minimum new bytes before we flush (avoids tiny updates).
	minChunkSize int
}

// NewStreamWriter creates a StreamWriter for the given channel and message routing info.
func NewStreamWriter(ch Channel, convID, threadID string, metadata map[string]string) *StreamWriter {
	return &StreamWriter{
		ch:            ch,
		convID:        convID,
		threadID:      threadID,
		metadata:      metadata,
		flushInterval: 800 * time.Millisecond,
		minChunkSize:  50,
	}
}

// SetFlushParams overrides the flush interval and minimum chunk size.
// Primarily useful for testing.
func (sw *StreamWriter) SetFlushParams(interval time.Duration, minBytes int) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	sw.flushInterval = interval
	sw.minChunkSize = minBytes
}

// OnChunk is the callback compatible with orchestrator.StreamChunkCallback.
// It accumulates text and debounces channel sends.
func (sw *StreamWriter) OnChunk(ctx context.Context, content string, done bool) {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	if content != "" {
		sw.buf.WriteString(content)
	}

	if done {
		sw.done = true
		sw.flush(ctx)
		return
	}

	// Debounce: flush if enough time has passed AND enough new content accumulated.
	now := time.Now()
	newBytes := sw.buf.Len() - len(sw.lastSent)
	if newBytes >= sw.minChunkSize && now.Sub(sw.lastFlush) >= sw.flushInterval {
		sw.flush(ctx)
	}
}

// Flushed returns true if the StreamWriter sent at least one message.
func (sw *StreamWriter) Flushed() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.flushed
}

// FullContent returns the complete accumulated content.
func (sw *StreamWriter) FullContent() string {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	return sw.buf.String()
}

// flush sends or updates the message on the channel. Must be called with mu held.
func (sw *StreamWriter) flush(ctx context.Context) {
	current := sw.buf.String()
	if current == sw.lastSent {
		return
	}

	indicator := ""
	if !sw.done {
		indicator = " \u25CD" // typing cursor for partial messages
	}

	msg := OutboundMessage{
		ConversationID: sw.convID,
		ThreadID:       sw.threadID,
		Content:        current + indicator,
		Metadata:       sw.cloneMetadata(),
	}

	// After the first send, try to update the existing message in place.
	if sw.flushed && sw.messageID != "" {
		if uch, ok := sw.ch.(UpdatableChannel); ok {
			if err := uch.SendUpdate(ctx, sw.messageID, msg); err != nil {
				slog.Debug("stream update failed", "error", err)
			} else {
				sw.lastSent = current
				sw.lastFlush = time.Now()
				return
			}
		}
	}

	// First message: try SendAndCapture to get a message ID for future updates.
	if !sw.flushed {
		if uch, ok := sw.ch.(UpdatableChannel); ok {
			msgID, err := uch.SendAndCapture(ctx, msg)
			if err != nil {
				slog.Debug("stream send-and-capture failed", "error", err)
				return
			}
			sw.messageID = msgID
		} else {
			if err := sw.ch.Send(ctx, msg); err != nil {
				slog.Debug("stream send failed", "error", err)
				return
			}
		}
		sw.flushed = true
		sw.lastSent = current
		sw.lastFlush = time.Now()
		return
	}

	// Channel doesn't support updates and we already sent — skip intermediate
	// flushes to avoid message spam. Only the final done=true flush goes through
	// as a new Send so the user sees the complete text.
	if sw.done {
		if err := sw.ch.Send(ctx, msg); err != nil {
			slog.Debug("stream final send failed", "error", err)
		}
		sw.lastSent = current
		sw.lastFlush = time.Now()
	}
}

// FinalUpdate replaces the streamed message content with the final processed
// response. Called by the registry after the handler returns, so the user sees
// the clean formatted text (tool-call blocks stripped, Lua formatting applied)
// instead of the raw accumulated stream.
func (sw *StreamWriter) FinalUpdate(ctx context.Context, content string) error {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !sw.flushed || content == sw.lastSent {
		return nil
	}
	msg := OutboundMessage{
		ConversationID: sw.convID,
		ThreadID:       sw.threadID,
		Content:        content,
		Metadata:       sw.cloneMetadata(),
	}
	if sw.messageID != "" {
		if uch, ok := sw.ch.(UpdatableChannel); ok {
			return uch.SendUpdate(ctx, sw.messageID, msg)
		}
	}
	return sw.ch.Send(ctx, msg)
}

func (sw *StreamWriter) cloneMetadata() map[string]string {
	if len(sw.metadata) == 0 {
		return nil
	}
	out := make(map[string]string, len(sw.metadata))
	for k, v := range sw.metadata {
		out[k] = v
	}
	return out
}
