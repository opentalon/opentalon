package channel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/logger"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// MessageDeduplicator is implemented by the Redis dedup client.
// TryAcquire returns true when this pod wins the lock for the given key.
type MessageDeduplicator interface {
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// Registry manages channel lifecycle, dispatches inbound messages to
// the orchestrator, and routes responses back to the originating channel.
// Concurrency is enforced by the orchestrator (via its semaphore and per-session
// locks); the registry simply forwards messages and lets the orchestrator block.
type Registry struct {
	mu       sync.RWMutex
	channels map[string]pkg.Channel
	handler  pkg.MessageHandler

	dedup    MessageDeduplicator
	dedupTTL time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRegistry creates a Registry.
func NewRegistry(handler pkg.MessageHandler) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		channels: make(map[string]pkg.Channel),
		handler:  handler,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// SetDeduplicator attaches a Redis-backed deduplicator to the registry.
// Must be called before any channels are registered.
func (r *Registry) SetDeduplicator(d MessageDeduplicator, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dedup = d
	r.dedupTTL = ttl
}

func (r *Registry) Register(ch pkg.Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := ch.ID()
	if _, exists := r.channels[id]; exists {
		return fmt.Errorf("channel %q already registered", id)
	}
	r.channels[id] = ch

	inbox := make(chan pkg.InboundMessage, 64)
	if err := ch.Start(r.ctx, inbox); err != nil {
		delete(r.channels, id)
		return fmt.Errorf("starting channel %q: %w", id, err)
	}

	r.wg.Add(1)
	go r.dispatch(ch, inbox)

	return nil
}

func (r *Registry) Deregister(id string) error {
	r.mu.Lock()
	ch, ok := r.channels[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("channel %q not found", id)
	}
	delete(r.channels, id)
	r.mu.Unlock()

	return ch.Stop()
}

func (r *Registry) Get(id string) (pkg.Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[id]
	return ch, ok
}

func (r *Registry) List() []pkg.Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]pkg.Channel, 0, len(r.channels))
	for _, ch := range r.channels {
		out = append(out, ch)
	}
	return out
}

// Send routes an outbound message to a specific channel.
func (r *Registry) Send(ctx context.Context, channelID string, msg pkg.OutboundMessage) error {
	r.mu.RLock()
	ch, ok := r.channels[channelID]
	r.mu.RUnlock()

	if !ok {
		return fmt.Errorf("channel %q not found", channelID)
	}
	return ch.Send(ctx, msg)
}

// StopAll gracefully shuts down every registered channel.
func (r *Registry) StopAll() {
	r.cancel()

	r.mu.RLock()
	channels := make([]pkg.Channel, 0, len(r.channels))
	for _, ch := range r.channels {
		channels = append(channels, ch)
	}
	r.mu.RUnlock()

	for _, ch := range channels {
		if err := ch.Stop(); err != nil {
			slog.Warn("stopping channel failed", "channel", ch.ID(), "error", err)
		}
	}
	r.wg.Wait()
}

func (r *Registry) dispatch(ch pkg.Channel, inbox <-chan pkg.InboundMessage) {
	// Shutdown ordering: r.cancel() closes r.ctx, the select below returns,
	// defer wg.Wait() drains in-flight goroutines, then defer r.wg.Done() signals StopAll.
	defer r.wg.Done()

	var wg sync.WaitGroup
	defer wg.Wait() // drain in-flight goroutines before signalling outer WaitGroup

	for {
		select {
		case <-r.ctx.Done():
			return
		case msg, ok := <-inbox:
			if !ok {
				return
			}

			r.mu.RLock()
			dedup, dedupTTL := r.dedup, r.dedupTTL
			r.mu.RUnlock()

			wg.Add(1)
			go func(m pkg.InboundMessage) {
				defer wg.Done()

				sessionKey := pkg.SessionKey(ch.ID(), m.ConversationID, m.ThreadID)
				traceID := logger.TraceIDFromSessionKey(sessionKey)
				ctx := logger.WithTraceID(r.ctx, traceID)
				caps := ch.Capabilities()
				ctx = pkg.WithCapabilities(ctx, caps)
				slog.Debug("channel capabilities",
					"channel", ch.ID(),
					"response_format", caps.ResponseFormat,
					"response_format_prompt", caps.ResponseFormatPrompt,
					"edits", caps.Edits,
				)

				// Deduplication: when running multiple pods each pod receives every
				// inbound message. Only the pod that wins the Redis SET NX lock
				// processes the message; others silently skip it.
				if dedup != nil {
					if m.Timestamp.IsZero() {
						slog.Warn("dedup skipped: message has no timestamp", "channel", ch.ID(), "session", sessionKey)
					} else {
						key := fmt.Sprintf("dedup:%s:%s:%d", m.ChannelID, m.ConversationID, m.Timestamp.UnixNano())
						won, err := dedup.TryAcquire(ctx, key, dedupTTL)
						if err != nil {
							slog.Warn("dedup acquire failed, processing anyway", "channel", ch.ID(), "error", err)
						} else if !won {
							slog.Debug("dedup skipped duplicate message", "channel", ch.ID(), "session", sessionKey)
							return
						}
					}
				}

				// When the channel supports edits, attach a StreamWriter so the
				// orchestrator can progressively deliver LLM output in real-time.
				// gRPC plugin channels report Edits=true but their PluginClient
				// doesn't implement UpdatableChannel, so StreamWriter would fall
				// back to plain Send() for every flush — spamming the client with
				// intermediate frames. Only create StreamWriter when the channel
				// actually supports in-place message updates.
				var sw *pkg.StreamWriter
				if caps.Edits {
					if _, ok := ch.(pkg.UpdatableChannel); ok {
						sw = pkg.NewStreamWriter(ch, m.ConversationID, m.ThreadID, safeMetadata(m.Metadata))
						ctx = pkg.WithStreamWriter(ctx, sw)
					}
				}

				// Send periodic typing indicators while the handler is
				// processing. This keeps WebSocket connections (and any
				// intermediate proxies) alive during long LLM calls.
				typingStop := startTypingIndicator(ctx, ch, m)

				resp, err := r.handler(ctx, sessionKey, m)
				typingStop()
				if err != nil {
					logger.FromContext(ctx).Error("handling message failed", "channel", ch.ID(), "session", sessionKey, "error", err)
					return
				}

				// Streaming delivered content progressively, but the final
				// handler response may differ (tool-call blocks stripped,
				// Lua formatting applied). Update the streamed message
				// with the clean response so users see the processed text.
				hasMeta := len(resp.Metadata) > 0
				if sw != nil && sw.Flushed() {
					logger.FromContext(ctx).Debug("registry: streaming path",
						"channel", ch.ID(), "has_metadata", hasMeta, "metadata", resp.Metadata)
					// Merge handler result metadata (e.g. confirmation type,
					// options) into the stream writer so FinalUpdate carries it.
					sw.MergeMetadata(resp.Metadata)
					if resp.Content != sw.FullContent() || hasMeta {
						if err := sw.FinalUpdate(ctx, resp.Content); err != nil {
							logger.FromContext(ctx).Debug("stream final update failed", "channel", ch.ID(), "error", err)
						}
					}
					return
				}

				logger.FromContext(ctx).Debug("registry: direct send path",
					"channel", ch.ID(), "has_metadata", hasMeta, "sw_nil", sw == nil)
				if err := ch.Send(ctx, resp); err != nil {
					logger.FromContext(ctx).Error("sending response failed", "channel", ch.ID(), "error", err)
				}
			}(msg)
		}
	}
}

// typingIndicatorInterval is how often keepalive typing messages are sent
// while the handler is processing. 25 s keeps connections alive through
// most proxy idle timeouts (commonly 30–60 s) without spamming the client.
var typingIndicatorInterval = 25 * time.Second

// startTypingIndicator launches a background goroutine that sends periodic
// typing-indicator messages to the channel. This prevents WebSocket and
// reverse-proxy idle timeouts from killing connections during long LLM calls.
// Call the returned function to stop the goroutine.
func startTypingIndicator(ctx context.Context, ch pkg.Channel, m pkg.InboundMessage) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(typingIndicatorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				msg := pkg.OutboundMessage{
					ConversationID: m.ConversationID,
					ThreadID:       m.ThreadID,
					Metadata: map[string]string{
						"_typing": "true",
					},
				}
				if err := ch.Send(ctx, msg); err != nil {
					slog.Debug("typing indicator send failed", "channel", ch.ID(), "error", err)
					return
				}
				slog.Debug("typing indicator sent", "channel", ch.ID(), "conversation", m.ConversationID)
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}
