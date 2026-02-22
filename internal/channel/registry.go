package channel

import (
	"context"
	"fmt"
	"log"
	"sync"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// Registry manages channel lifecycle, dispatches inbound messages to
// the orchestrator, and routes responses back to the originating channel.
type Registry struct {
	mu       sync.RWMutex
	channels map[string]pkg.Channel
	handler  pkg.MessageHandler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewRegistry(handler pkg.MessageHandler) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		channels: make(map[string]pkg.Channel),
		handler:  handler,
		ctx:      ctx,
		cancel:   cancel,
	}
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
			log.Printf("stopping channel %q: %v", ch.ID(), err)
		}
	}
	r.wg.Wait()
}

func (r *Registry) dispatch(ch pkg.Channel, inbox <-chan pkg.InboundMessage) {
	defer r.wg.Done()

	for {
		select {
		case <-r.ctx.Done():
			return
		case msg, ok := <-inbox:
			if !ok {
				return
			}
			sessionKey := pkg.SessionKey(ch.ID(), msg.ConversationID, msg.ThreadID)
			resp, err := r.handler(r.ctx, sessionKey, msg)
			if err != nil {
				log.Printf("handling message on channel %q session %q: %v", ch.ID(), sessionKey, err)
				continue
			}
			if err := ch.Send(r.ctx, resp); err != nil {
				log.Printf("sending response on channel %q: %v", ch.ID(), err)
			}
		}
	}
}
