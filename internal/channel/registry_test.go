package channel

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

type mockChannel struct {
	id    string
	caps  pkg.Capabilities
	mu    sync.Mutex
	sent  []pkg.OutboundMessage
	stop  bool
	inbox chan<- pkg.InboundMessage
}

func newMockChannel(id string) *mockChannel {
	return &mockChannel{
		id:   id,
		caps: pkg.Capabilities{ID: id, Name: id, Threads: true},
	}
}

func (m *mockChannel) ID() string                     { return m.id }
func (m *mockChannel) Capabilities() pkg.Capabilities { return m.caps }

func (m *mockChannel) Start(_ context.Context, inbox chan<- pkg.InboundMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inbox = inbox
	return nil
}

func (m *mockChannel) Send(_ context.Context, msg pkg.OutboundMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

func (m *mockChannel) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stop = true
	return nil
}

func (m *mockChannel) pushMessage(msg pkg.InboundMessage) {
	m.mu.Lock()
	inbox := m.inbox
	m.mu.Unlock()
	if inbox != nil {
		inbox <- msg
	}
}

func (m *mockChannel) sentMessages() []pkg.OutboundMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pkg.OutboundMessage, len(m.sent))
	copy(out, m.sent)
	return out
}

func (m *mockChannel) stopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop
}

type failStartChannel struct{ mockChannel }

func (f *failStartChannel) Start(_ context.Context, _ chan<- pkg.InboundMessage) error {
	return fmt.Errorf("start failed")
}

func echoHandler(_ context.Context, sessionKey string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
	return pkg.OutboundMessage{
		ConversationID: msg.ConversationID,
		ThreadID:       msg.ThreadID,
		Content:        "echo: " + msg.Content,
	}, nil
}

func TestSessionKey(t *testing.T) {
	tests := []struct {
		ch, conv, thread, want string
	}{
		{"slack", "C123", "T456", "slack:C123:T456"},
		{"slack", "C123", "", "slack:C123"},
		{"teams", "general", "", "teams:general"},
		{"discord", "guild1", "thread1", "discord:guild1:thread1"},
	}
	for _, tt := range tests {
		got := pkg.SessionKey(tt.ch, tt.conv, tt.thread)
		if got != tt.want {
			t.Errorf("SessionKey(%q,%q,%q) = %q, want %q", tt.ch, tt.conv, tt.thread, got, tt.want)
		}
	}
}

func TestRegistryRegisterAndList(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("slack")
	if err := reg.Register(ch); err != nil {
		t.Fatalf("Register: %v", err)
	}

	channels := reg.List()
	if len(channels) != 1 {
		t.Fatalf("List() returned %d channels, want 1", len(channels))
	}
	if channels[0].ID() != "slack" {
		t.Errorf("channel ID = %q, want %q", channels[0].ID(), "slack")
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch1 := newMockChannel("slack")
	if err := reg.Register(ch1); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ch2 := newMockChannel("slack")
	if err := reg.Register(ch2); err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestRegistryRegisterStartFailure(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := &failStartChannel{mockChannel: mockChannel{id: "bad", caps: pkg.Capabilities{ID: "bad", Name: "bad"}}}
	if err := reg.Register(ch); err == nil {
		t.Fatal("expected error when Start fails")
	}

	if _, ok := reg.Get("bad"); ok {
		t.Error("channel should not be registered after Start failure")
	}
}

func TestRegistryGet(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("telegram")
	_ = reg.Register(ch)

	got, ok := reg.Get("telegram")
	if !ok || got.ID() != "telegram" {
		t.Error("Get failed for registered channel")
	}

	_, ok = reg.Get("nonexistent")
	if ok {
		t.Error("Get should return false for unregistered channel")
	}
}

func TestRegistryDeregister(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("teams")
	_ = reg.Register(ch)

	if err := reg.Deregister("teams"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if !ch.stopped() {
		t.Error("channel should be stopped after deregister")
	}
	if _, ok := reg.Get("teams"); ok {
		t.Error("channel should not be findable after deregister")
	}
}

func TestRegistryDeregisterNotFound(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	if err := reg.Deregister("nope"); err == nil {
		t.Fatal("expected error for deregistering nonexistent channel")
	}
}

func TestRegistrySend(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("whatsapp")
	_ = reg.Register(ch)

	msg := pkg.OutboundMessage{
		ConversationID: "conv1",
		Content:        "hello from core",
	}
	if err := reg.Send(context.Background(), "whatsapp", msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sent := ch.sentMessages()
	if len(sent) != 1 || sent[0].Content != "hello from core" {
		t.Errorf("sent messages = %v, want 1 message with 'hello from core'", sent)
	}
}

func TestRegistrySendNotFound(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	if err := reg.Send(context.Background(), "nope", pkg.OutboundMessage{}); err == nil {
		t.Fatal("expected error sending to unregistered channel")
	}
}

func TestRegistryDispatch(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("discord")
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{
		ConversationID: "room1",
		ThreadID:       "t1",
		Content:        "hi",
	})

	deadline := time.After(2 * time.Second)
	for {
		sent := ch.sentMessages()
		if len(sent) > 0 {
			if sent[0].Content != "echo: hi" {
				t.Errorf("response content = %q, want %q", sent[0].Content, "echo: hi")
			}
			if sent[0].ConversationID != "room1" {
				t.Errorf("ConversationID = %q, want %q", sent[0].ConversationID, "room1")
			}
			if sent[0].ThreadID != "t1" {
				t.Errorf("ThreadID = %q, want %q", sent[0].ThreadID, "t1")
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for dispatched response")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestRegistryStopAll(t *testing.T) {
	reg := NewRegistry(echoHandler)

	ch1 := newMockChannel("ch1")
	ch2 := newMockChannel("ch2")
	_ = reg.Register(ch1)
	_ = reg.Register(ch2)

	reg.StopAll()

	if !ch1.stopped() {
		t.Error("ch1 should be stopped")
	}
	if !ch2.stopped() {
		t.Error("ch2 should be stopped")
	}
}

// TestRegistryDispatchConcurrent verifies that multiple messages are dispatched in
// parallel — the registry spawns a goroutine per message and the handler runs them
// concurrently. Concurrency limiting is the orchestrator's responsibility.
func TestRegistryDispatchConcurrent(t *testing.T) {
	var inflight int32
	var peak int32
	unblock := make(chan struct{})

	slowHandler := func(_ context.Context, _ string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		cur := atomic.AddInt32(&inflight, 1)
		defer atomic.AddInt32(&inflight, -1)
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old {
				break
			}
			if atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		<-unblock
		return pkg.OutboundMessage{ConversationID: msg.ConversationID, Content: "ok"}, nil
	}

	reg := NewRegistry(slowHandler)
	defer reg.StopAll()

	ch := newMockChannel("parallel")
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{ConversationID: "c1", Content: "msg1"})
	ch.pushMessage(pkg.InboundMessage{ConversationID: "c2", Content: "msg2"})

	// Wait until both are in-flight.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&inflight) < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for both messages to be dispatched concurrently")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	close(unblock)

	deadline2 := time.After(2 * time.Second)
	for len(ch.sentMessages()) < 2 {
		select {
		case <-deadline2:
			t.Fatal("timed out waiting for both responses")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := atomic.LoadInt32(&peak); got < 2 {
		t.Errorf("peak concurrent dispatches = %d, want ≥2", got)
	}
}

func TestRegistryDispatchHandlerError(t *testing.T) {
	errHandler := func(_ context.Context, _ string, _ pkg.InboundMessage) (pkg.OutboundMessage, error) {
		return pkg.OutboundMessage{}, fmt.Errorf("handler error")
	}

	reg := NewRegistry(errHandler)
	defer reg.StopAll()

	ch := newMockChannel("errch")
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{
		ConversationID: "c1",
		Content:        "fail",
	})

	time.Sleep(100 * time.Millisecond)
	sent := ch.sentMessages()
	if len(sent) != 0 {
		t.Errorf("expected no sent messages on handler error, got %d", len(sent))
	}
}

func TestRegistryDispatchInjectsCapabilities(t *testing.T) {
	var capturedCaps pkg.Capabilities
	done := make(chan struct{})
	capturingHandler := func(ctx context.Context, _ string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		capturedCaps = pkg.CapabilitiesFromContext(ctx)
		close(done)
		return pkg.OutboundMessage{ConversationID: msg.ConversationID, Content: "ok"}, nil
	}

	reg := NewRegistry(capturingHandler)
	defer reg.StopAll()

	ch := &mockChannel{
		id: "slack",
		caps: pkg.Capabilities{
			ID:             "slack",
			Name:           "Slack",
			Threads:        true,
			ResponseFormat: pkg.FormatSlack,
		},
	}
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{
		ChannelID:      "slack",
		ConversationID: "c1",
		Content:        "hello",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler to be called")
	}

	if capturedCaps.ID != "slack" {
		t.Errorf("capabilities.ID = %q, want %q", capturedCaps.ID, "slack")
	}
	if capturedCaps.ResponseFormat != pkg.FormatSlack {
		t.Errorf("capabilities.ResponseFormat = %q, want %q", capturedCaps.ResponseFormat, pkg.FormatSlack)
	}
	if !capturedCaps.Threads {
		t.Error("capabilities.Threads should be true")
	}
}

func TestRegistryDispatchStreamWriterInjected(t *testing.T) {
	// When a channel declares edits:true, the dispatch should inject a
	// StreamWriter into the context. The handler can verify this.
	var gotSW bool
	done := make(chan struct{})

	handler := func(ctx context.Context, _ string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		sw := pkg.StreamWriterFromContext(ctx)
		gotSW = sw != nil
		// Simulate streaming: call OnChunk so the StreamWriter flushes,
		// which means the final Send should be skipped.
		if sw != nil {
			sw.OnChunk(ctx, "streamed!", true)
		}
		close(done)
		return pkg.OutboundMessage{ConversationID: msg.ConversationID, Content: "streamed!"}, nil
	}

	reg := NewRegistry(handler)
	defer reg.StopAll()

	ch := &mockChannel{
		id:   "slack-stream",
		caps: pkg.Capabilities{ID: "slack-stream", Name: "Slack", Edits: true},
	}
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{
		ChannelID:      "slack-stream",
		ConversationID: "c1",
		Content:        "hello",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	if !gotSW {
		t.Error("expected StreamWriter to be injected for edits:true channel")
	}

	// Give dispatch goroutine time to finish.
	time.Sleep(50 * time.Millisecond)
	sent := ch.sentMessages()

	// The StreamWriter flushed (streaming delivered the content), so the
	// registry dispatch should have skipped the final ch.Send. However,
	// the StreamWriter itself called Send once for the streaming flush.
	// So we expect exactly 1 Send (from StreamWriter), not 2.
	if len(sent) != 1 {
		t.Errorf("expected 1 Send call (from StreamWriter), got %d", len(sent))
	}
}

func TestRegistryDispatchNoStreamWriterWithoutEdits(t *testing.T) {
	// Channels without edits:true should not get a StreamWriter.
	var gotSW bool
	done := make(chan struct{})

	handler := func(ctx context.Context, _ string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		gotSW = pkg.StreamWriterFromContext(ctx) != nil
		close(done)
		return pkg.OutboundMessage{ConversationID: msg.ConversationID, Content: "reply"}, nil
	}

	reg := NewRegistry(handler)
	defer reg.StopAll()

	ch := newMockChannel("noedit")
	_ = reg.Register(ch)

	ch.pushMessage(pkg.InboundMessage{
		ChannelID:      "noedit",
		ConversationID: "c1",
		Content:        "hello",
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}

	if gotSW {
		t.Error("StreamWriter should NOT be injected when edits is false")
	}

	time.Sleep(50 * time.Millisecond)
	sent := ch.sentMessages()
	if len(sent) != 1 {
		t.Errorf("expected 1 Send call, got %d", len(sent))
	}
}
