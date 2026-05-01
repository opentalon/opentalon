package channel

import (
	"context"
	"sync"
	"testing"
)

// fakeChannel implements Channel for testing.
type fakeChannel struct {
	mu       sync.Mutex
	caps     Capabilities
	messages []OutboundMessage
}

func (f *fakeChannel) ID() string                                         { return f.caps.ID }
func (f *fakeChannel) Capabilities() Capabilities                         { return f.caps }
func (f *fakeChannel) Start(context.Context, chan<- InboundMessage) error { return nil }
func (f *fakeChannel) Stop() error                                        { return nil }

func (f *fakeChannel) Send(_ context.Context, msg OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return nil
}

func (f *fakeChannel) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeChannel) lastContent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return ""
	}
	return f.messages[len(f.messages)-1].Content
}

// fakeUpdatableChannel implements UpdatableChannel.
type fakeUpdatableChannel struct {
	fakeChannel
	updates   []OutboundMessage
	captureID string // returned by SendAndCapture
}

func (f *fakeUpdatableChannel) SendAndCapture(_ context.Context, msg OutboundMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return f.captureID, nil
}

func (f *fakeUpdatableChannel) SendUpdate(_ context.Context, _ string, msg OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, msg)
	return nil
}

func (f *fakeUpdatableChannel) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updates)
}

func TestStreamWriterContext(t *testing.T) {
	sw := NewStreamWriter(&fakeChannel{}, "conv1", "thread1", nil)

	ctx := context.Background()
	if StreamWriterFromContext(ctx) != nil {
		t.Error("expected nil StreamWriter from empty context")
	}

	ctx = WithStreamWriter(ctx, sw)
	got := StreamWriterFromContext(ctx)
	if got != sw {
		t.Error("expected same StreamWriter from context")
	}
}

func TestStreamWriterDoneFlush(t *testing.T) {
	ch := &fakeChannel{caps: Capabilities{ID: "test", Edits: true}}
	sw := NewStreamWriter(ch, "conv1", "", nil)

	ctx := context.Background()

	// Send a bunch of chunks then done.
	sw.OnChunk(ctx, "Hello", false)
	sw.OnChunk(ctx, " world", false)
	sw.OnChunk(ctx, "!", true) // done

	if !sw.Flushed() {
		t.Error("expected Flushed()=true after done")
	}
	if sw.FullContent() != "Hello world!" {
		t.Errorf("FullContent = %q, want %q", sw.FullContent(), "Hello world!")
	}
	// At least one Send should have happened.
	if ch.sentCount() == 0 {
		t.Error("expected at least one Send call")
	}
}

func TestStreamWriterNonEditableChannel(t *testing.T) {
	// For non-updatable channels, first flush sends, then done sends final.
	ch := &fakeChannel{caps: Capabilities{ID: "test"}}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	sw.SetFlushParams(0, 0)

	ctx := context.Background()

	sw.OnChunk(ctx, "Hello", false)
	sw.OnChunk(ctx, " world", false)
	sw.OnChunk(ctx, "!", true)

	// Non-updatable: first send + final done send = 2 max.
	if ch.sentCount() > 3 {
		t.Errorf("expected at most 3 Send calls for non-updatable channel, got %d", ch.sentCount())
	}
	// Final message should have the complete text without cursor.
	last := ch.lastContent()
	if last != "Hello world!" {
		t.Errorf("final content = %q, want %q", last, "Hello world!")
	}
}

func TestStreamWriterWithUpdatableChannel(t *testing.T) {
	ch := &fakeUpdatableChannel{
		fakeChannel: fakeChannel{caps: Capabilities{ID: "test", Edits: true}},
		captureID:   "1234567890.123456", // simulated Slack ts
	}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	sw.SetFlushParams(0, 1)

	ctx := context.Background()

	sw.OnChunk(ctx, "Hello", false)
	sw.OnChunk(ctx, " world", false)
	sw.OnChunk(ctx, "!", true)

	// First chunk creates via SendAndCapture.
	if ch.sentCount() < 1 {
		t.Error("expected at least one SendAndCapture for initial message")
	}
	// Subsequent chunks go through SendUpdate.
	if ch.updateCount() < 1 {
		t.Error("expected at least one SendUpdate call")
	}
}

func TestStreamWriterSendStatus(t *testing.T) {
	ch := &fakeUpdatableChannel{
		fakeChannel: fakeChannel{caps: Capabilities{ID: "test", Edits: true}},
		captureID:   "msg-status-1",
	}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	ctx := context.Background()

	// First status creates the message via SendAndCapture.
	sw.SendStatus(ctx, "Thinking\u2026")
	if ch.sentCount() != 1 {
		t.Errorf("expected 1 send for initial status, got %d", ch.sentCount())
	}
	if ch.lastContent() != "_Thinking\u2026_" {
		t.Errorf("status content = %q, want italic label", ch.lastContent())
	}

	// Second status updates the existing message.
	sw.SendStatus(ctx, "Calling timly.list-items\u2026")
	if ch.updateCount() < 1 {
		t.Error("expected SendUpdate for second status")
	}
	last := ch.updates[len(ch.updates)-1]
	if last.Content != "_Calling timly.list-items\u2026_" {
		t.Errorf("updated status = %q, want italic label", last.Content)
	}

	// Real content replaces the status.
	sw.OnChunk(ctx, "The answer is 42.", true)
	lastUpdate := ch.updates[len(ch.updates)-1]
	if lastUpdate.Content != "The answer is 42." {
		t.Errorf("final content = %q, want real answer", lastUpdate.Content)
	}
}

func TestStreamWriterFinalUpdate(t *testing.T) {
	ch := &fakeUpdatableChannel{
		fakeChannel: fakeChannel{caps: Capabilities{ID: "test", Edits: true}},
		captureID:   "msg-ts-456",
	}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	sw.SetFlushParams(0, 1)

	ctx := context.Background()

	// Simulate streaming with tool call blocks in the raw output.
	sw.OnChunk(ctx, "[tool_call]{\"tool\":\"x.y\"}[/tool_call]", false)
	sw.OnChunk(ctx, "The answer is 42.", true)

	updatesBefore := ch.updateCount()

	// FinalUpdate replaces the streamed content with the clean response.
	err := sw.FinalUpdate(ctx, "The answer is 42.")
	if err != nil {
		t.Fatalf("FinalUpdate returned error: %v", err)
	}
	if ch.updateCount() != updatesBefore+1 {
		t.Error("expected FinalUpdate to trigger a SendUpdate")
	}
	last := ch.updates[len(ch.updates)-1]
	if last.Content != "The answer is 42." {
		t.Errorf("FinalUpdate content = %q, want %q", last.Content, "The answer is 42.")
	}
}

func TestStreamWriterFinalUpdateNoopWhenSame(t *testing.T) {
	ch := &fakeUpdatableChannel{
		fakeChannel: fakeChannel{caps: Capabilities{ID: "test", Edits: true}},
		captureID:   "msg-ts-789",
	}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	sw.SetFlushParams(0, 1)

	ctx := context.Background()

	sw.OnChunk(ctx, "Hello", true)
	updatesBefore := ch.updateCount()

	// Content matches lastSent — should be a no-op.
	err := sw.FinalUpdate(ctx, "Hello")
	if err != nil {
		t.Fatalf("FinalUpdate returned error: %v", err)
	}
	if ch.updateCount() != updatesBefore {
		t.Error("expected FinalUpdate to be a no-op when content matches")
	}
}

func TestStreamWriterCapturesMessageID(t *testing.T) {
	ch := &fakeUpdatableChannel{
		fakeChannel: fakeChannel{caps: Capabilities{ID: "test", Edits: true}},
		captureID:   "msg-ts-123",
	}
	sw := NewStreamWriter(ch, "conv1", "", nil)
	sw.SetFlushParams(0, 1)

	ctx := context.Background()

	// First chunk triggers SendAndCapture.
	sw.OnChunk(ctx, "partial", false)
	// Second chunk should use SendUpdate with the captured ID.
	sw.OnChunk(ctx, " text", false)
	sw.OnChunk(ctx, "", true)

	if ch.sentCount() != 1 {
		t.Errorf("expected exactly 1 SendAndCapture, got %d sends", ch.sentCount())
	}
	if ch.updateCount() < 1 {
		t.Errorf("expected at least 1 update, got %d", ch.updateCount())
	}
}
