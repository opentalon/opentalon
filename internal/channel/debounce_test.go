package channel

import (
	"sync"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

func TestDebouncerMergesRapidMessages(t *testing.T) {
	var mu sync.Mutex
	var dispatched []pkg.InboundMessage

	d := newSessionDebouncer(100*time.Millisecond, func(_ string, merged pkg.InboundMessage) {
		mu.Lock()
		dispatched = append(dispatched, merged)
		mu.Unlock()
	})

	d.submit("s1", pkg.InboundMessage{Content: "hello", ConversationID: "c1"})
	d.submit("s1", pkg.InboundMessage{Content: "world", ConversationID: "c1"})
	d.submit("s1", pkg.InboundMessage{Content: "!", ConversationID: "c1"})

	// Wait for debounce timer to fire.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 merged dispatch, got %d", len(dispatched))
	}
	if dispatched[0].Content != "hello\nworld\n!" {
		t.Errorf("merged content = %q, want %q", dispatched[0].Content, "hello\nworld\n!")
	}
}

func TestDebouncerConfirmationBypassesDebounce(t *testing.T) {
	var mu sync.Mutex
	var dispatched []pkg.InboundMessage

	d := newSessionDebouncer(100*time.Millisecond, func(_ string, merged pkg.InboundMessage) {
		mu.Lock()
		dispatched = append(dispatched, merged)
		mu.Unlock()
	})

	msg := pkg.InboundMessage{
		Content:  "yes",
		Metadata: map[string]string{"confirmation": "approve"},
	}
	debounced := d.submit("s1", msg)
	if debounced {
		t.Error("confirmation message should bypass debounce")
	}

	// No dispatch should happen from debouncer.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) != 0 {
		t.Errorf("expected 0 dispatches for bypassed message, got %d", len(dispatched))
	}
}

func TestDebouncerSeparateSessions(t *testing.T) {
	var mu sync.Mutex
	dispatched := make(map[string]pkg.InboundMessage)

	d := newSessionDebouncer(100*time.Millisecond, func(key string, merged pkg.InboundMessage) {
		mu.Lock()
		dispatched[key] = merged
		mu.Unlock()
	})

	d.submit("s1", pkg.InboundMessage{Content: "a", ConversationID: "c1"})
	d.submit("s2", pkg.InboundMessage{Content: "b", ConversationID: "c2"})

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) != 2 {
		t.Fatalf("expected 2 dispatches (one per session), got %d", len(dispatched))
	}
	if dispatched["s1"].Content != "a" {
		t.Errorf("s1 content = %q, want %q", dispatched["s1"].Content, "a")
	}
	if dispatched["s2"].Content != "b" {
		t.Errorf("s2 content = %q, want %q", dispatched["s2"].Content, "b")
	}
}

func TestDebouncerSingleMessageDispatchesAfterWindow(t *testing.T) {
	const window = 50 * time.Millisecond

	var mu sync.Mutex
	var dispatched []pkg.InboundMessage

	d := newSessionDebouncer(window, func(_ string, merged pkg.InboundMessage) {
		mu.Lock()
		dispatched = append(dispatched, merged)
		mu.Unlock()
	})

	d.submit("s1", pkg.InboundMessage{Content: "solo"})

	// Wait long enough for the timer to fire, even under CI load.
	time.Sleep(3 * window)

	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].Content != "solo" {
		t.Errorf("content = %q, want %q", dispatched[0].Content, "solo")
	}
}

func TestMergeMessagesMetadata(t *testing.T) {
	messages := []pkg.InboundMessage{
		{Content: "first", Metadata: map[string]string{"a": "1", "b": "old"}},
		{Content: "second", Metadata: map[string]string{"b": "new", "c": "3"}},
	}
	merged := mergeMessages(messages)
	if merged.Metadata["a"] != "1" {
		t.Errorf("a = %q, want %q", merged.Metadata["a"], "1")
	}
	if merged.Metadata["b"] != "new" {
		t.Errorf("b = %q, want %q (last wins)", merged.Metadata["b"], "new")
	}
	if merged.Metadata["c"] != "3" {
		t.Errorf("c = %q, want %q", merged.Metadata["c"], "3")
	}
}

func TestMergeMessagesFiles(t *testing.T) {
	messages := []pkg.InboundMessage{
		{Content: "a", Files: []pkg.FileAttachment{{Name: "f1"}}},
		{Content: "b", Files: []pkg.FileAttachment{{Name: "f2"}, {Name: "f3"}}},
	}
	merged := mergeMessages(messages)
	if len(merged.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(merged.Files))
	}
}
