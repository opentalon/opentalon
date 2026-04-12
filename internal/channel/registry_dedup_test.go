package channel

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// fakeDedup is a controllable MessageDeduplicator for tests.
type fakeDedup struct {
	mu sync.Mutex
	// acquired tracks which keys have been claimed; second call for same key returns false.
	acquired map[string]bool
	// err is returned on every call when non-nil.
	err error
}

func newFakeDedup() *fakeDedup { return &fakeDedup{acquired: make(map[string]bool)} }

func (f *fakeDedup) TryAcquire(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.acquired[key] {
		return false, nil
	}
	f.acquired[key] = true
	return true, nil
}

// stubChannel delivers messages and records sends.
type stubChannel struct {
	id    string
	inbox chan<- pkg.InboundMessage
}

func (c *stubChannel) ID() string                                          { return c.id }
func (c *stubChannel) Capabilities() pkg.Capabilities                      { return pkg.Capabilities{} }
func (c *stubChannel) Stop() error                                         { return nil }
func (c *stubChannel) Send(_ context.Context, _ pkg.OutboundMessage) error { return nil }
func (c *stubChannel) Start(_ context.Context, inbox chan<- pkg.InboundMessage) error {
	c.inbox = inbox
	return nil
}

func newMsg(channelID, convID string, ts time.Time) pkg.InboundMessage {
	return pkg.InboundMessage{
		ChannelID:      channelID,
		ConversationID: convID,
		SenderID:       "U1",
		Content:        "hello",
		Timestamp:      ts,
	}
}

func TestRegistryDedup_WinsOnce(t *testing.T) {
	var handled atomic.Int32
	handler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled.Add(1)
		return pkg.OutboundMessage{}, nil
	}

	reg := NewRegistry(handler)
	d := newFakeDedup()
	reg.SetDeduplicator(d, 5*time.Minute)

	ch := &stubChannel{id: "slack"}
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}

	ts := time.Now()
	msg := newMsg("slack", "C123", ts)

	// Send the same message twice (simulates two pods both receiving it).
	ch.inbox <- msg
	ch.inbox <- msg

	// Give dispatch goroutines time to process.
	time.Sleep(100 * time.Millisecond)

	reg.StopAll()

	if got := handled.Load(); got != 1 {
		t.Errorf("expected handler called 1 time, got %d", got)
	}
}

func TestRegistryDedup_DifferentMessagesAllHandled(t *testing.T) {
	var handled atomic.Int32
	handler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled.Add(1)
		return pkg.OutboundMessage{}, nil
	}

	reg := NewRegistry(handler)
	reg.SetDeduplicator(newFakeDedup(), 5*time.Minute)

	ch := &stubChannel{id: "slack"}
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	ch.inbox <- newMsg("slack", "C123", now)
	ch.inbox <- newMsg("slack", "C456", now)
	ch.inbox <- newMsg("slack", "C123", now.Add(time.Second))

	time.Sleep(100 * time.Millisecond)
	reg.StopAll()

	if got := handled.Load(); got != 3 {
		t.Errorf("expected handler called 3 times, got %d", got)
	}
}

func TestRegistryDedup_ZeroTimestampProcessesAnyway(t *testing.T) {
	var handled atomic.Int32
	handler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled.Add(1)
		return pkg.OutboundMessage{}, nil
	}

	reg := NewRegistry(handler)
	reg.SetDeduplicator(newFakeDedup(), 5*time.Minute)

	ch := &stubChannel{id: "slack"}
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}

	// Zero timestamp: dedup is skipped, message must still be processed.
	ch.inbox <- newMsg("slack", "C123", time.Time{})

	time.Sleep(100 * time.Millisecond)
	reg.StopAll()

	if got := handled.Load(); got != 1 {
		t.Errorf("expected handler called 1 time, got %d", got)
	}
}

func TestRegistryDedup_RedisErrorFallsThrough(t *testing.T) {
	var handled atomic.Int32
	handler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled.Add(1)
		return pkg.OutboundMessage{}, nil
	}

	reg := NewRegistry(handler)
	d := newFakeDedup()
	d.err = context.DeadlineExceeded // simulate Redis being unreachable
	reg.SetDeduplicator(d, 5*time.Minute)

	ch := &stubChannel{id: "slack"}
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}

	ch.inbox <- newMsg("slack", "C123", time.Now())

	time.Sleep(100 * time.Millisecond)
	reg.StopAll()

	// Must still process when Redis is down.
	if got := handled.Load(); got != 1 {
		t.Errorf("expected handler called 1 time on Redis error, got %d", got)
	}
}

func TestRegistryDedup_DisabledWhenNotSet(t *testing.T) {
	var handled atomic.Int32
	handler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		handled.Add(1)
		return pkg.OutboundMessage{}, nil
	}

	reg := NewRegistry(handler) // no SetDeduplicator

	ch := &stubChannel{id: "slack"}
	if err := reg.Register(ch); err != nil {
		t.Fatal(err)
	}

	ts := time.Now()
	ch.inbox <- newMsg("slack", "C123", ts)
	ch.inbox <- newMsg("slack", "C123", ts) // same key, but no dedup configured

	time.Sleep(100 * time.Millisecond)
	reg.StopAll()

	if got := handled.Load(); got != 2 {
		t.Errorf("expected handler called 2 times without dedup, got %d", got)
	}
}
