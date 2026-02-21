package channel

import (
	"context"
	"fmt"
	"log"
	"sync"
)

// SessionKey builds a deterministic session identifier from the
// channel, conversation, and thread triple.
func SessionKey(channelID, conversationID, threadID string) string {
	if threadID == "" {
		return fmt.Sprintf("%s:%s", channelID, conversationID)
	}
	return fmt.Sprintf("%s:%s:%s", channelID, conversationID, threadID)
}

// MessageHandler is called by the registry when an inbound message
// arrives. The implementation is responsible for feeding the message
// to the orchestrator and returning the response.
type MessageHandler func(ctx context.Context, sessionKey string, msg InboundMessage) (OutboundMessage, error)

// Registry manages channel lifecycle, dispatches inbound messages to
// the orchestrator, and routes responses back to the originating channel.
type Registry struct {
	mu       sync.RWMutex
	channels map[string]Channel
	handler  MessageHandler

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewRegistry(handler MessageHandler) *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		channels: make(map[string]Channel),
		handler:  handler,
		ctx:      ctx,
		cancel:   cancel,
	}
}

func (r *Registry) Register(ch Channel) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	id := ch.ID()
	if _, exists := r.channels[id]; exists {
		return fmt.Errorf("channel %q already registered", id)
	}
	r.channels[id] = ch

	inbox := make(chan InboundMessage, 64)
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

func (r *Registry) Get(id string) (Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[id]
	return ch, ok
}

func (r *Registry) List() []Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Channel, 0, len(r.channels))
	for _, ch := range r.channels {
		out = append(out, ch)
	}
	return out
}

// Send routes an outbound message to a specific channel.
func (r *Registry) Send(ctx context.Context, channelID string, msg OutboundMessage) error {
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
	channels := make([]Channel, 0, len(r.channels))
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

func (r *Registry) dispatch(ch Channel, inbox <-chan InboundMessage) {
	defer r.wg.Done()

	for {
		select {
		case <-r.ctx.Done():
			return
		case msg, ok := <-inbox:
			if !ok {
				return
			}
			sessionKey := SessionKey(ch.ID(), msg.ConversationID, msg.ThreadID)
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
