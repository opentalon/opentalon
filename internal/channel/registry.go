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
				ctx = pkg.WithCapabilities(ctx, ch.Capabilities())

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

				resp, err := r.handler(ctx, sessionKey, m)
				if err != nil {
					logger.FromContext(ctx).Error("handling message failed", "channel", ch.ID(), "session", sessionKey, "error", err)
					return
				}
				if err := ch.Send(ctx, resp); err != nil {
					logger.FromContext(ctx).Error("sending response failed", "channel", ch.ID(), "error", err)
				}
			}(msg)
		}
	}
}
