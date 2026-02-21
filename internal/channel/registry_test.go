package channel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type mockChannel struct {
	id    string
	caps  Capabilities
	mu    sync.Mutex
	sent  []OutboundMessage
	stop  bool
	inbox chan<- InboundMessage
}

func newMockChannel(id string) *mockChannel {
	return &mockChannel{
		id:   id,
		caps: Capabilities{ID: id, Name: id, Threads: true},
	}
}

func (m *mockChannel) ID() string                 { return m.id }
func (m *mockChannel) Capabilities() Capabilities { return m.caps }

func (m *mockChannel) Start(_ context.Context, inbox chan<- InboundMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inbox = inbox
	return nil
}

func (m *mockChannel) Send(_ context.Context, msg OutboundMessage) error {
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

func (m *mockChannel) pushMessage(msg InboundMessage) {
	m.mu.Lock()
	inbox := m.inbox
	m.mu.Unlock()
	if inbox != nil {
		inbox <- msg
	}
}

func (m *mockChannel) sentMessages() []OutboundMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]OutboundMessage, len(m.sent))
	copy(out, m.sent)
	return out
}

func (m *mockChannel) stopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stop
}

type failStartChannel struct{ mockChannel }

func (f *failStartChannel) Start(_ context.Context, _ chan<- InboundMessage) error {
	return fmt.Errorf("start failed")
}

func echoHandler(_ context.Context, sessionKey string, msg InboundMessage) (OutboundMessage, error) {
	return OutboundMessage{
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
		got := SessionKey(tt.ch, tt.conv, tt.thread)
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

	ch := &failStartChannel{mockChannel: mockChannel{id: "bad"}}
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

	msg := OutboundMessage{
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

	if err := reg.Send(context.Background(), "nope", OutboundMessage{}); err == nil {
		t.Fatal("expected error sending to unregistered channel")
	}
}

func TestRegistryDispatch(t *testing.T) {
	reg := NewRegistry(echoHandler)
	defer reg.StopAll()

	ch := newMockChannel("discord")
	_ = reg.Register(ch)

	ch.pushMessage(InboundMessage{
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

func TestRegistryDispatchHandlerError(t *testing.T) {
	errHandler := func(_ context.Context, _ string, _ InboundMessage) (OutboundMessage, error) {
		return OutboundMessage{}, fmt.Errorf("handler error")
	}

	reg := NewRegistry(errHandler)
	defer reg.StopAll()

	ch := newMockChannel("errch")
	_ = reg.Register(ch)

	ch.pushMessage(InboundMessage{
		ConversationID: "c1",
		Content:        "fail",
	})

	time.Sleep(100 * time.Millisecond)
	sent := ch.sentMessages()
	if len(sent) != 0 {
		t.Errorf("expected no sent messages on handler error, got %d", len(sent))
	}
}
