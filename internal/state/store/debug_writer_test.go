package store

import (
	"context"
	"testing"
	"time"
)

// TestDebugEventWriter_FlushesAllOnStop is the happy path: events sent
// during normal operation reach the underlying store before Stop returns.
func TestDebugEventWriter_FlushesAllOnStop(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	w := newDebugEventWriterSized(store, 10)
	w.Start(context.Background())

	for i := 0; i < 5; i++ {
		w.Submit(DebugEvent{SessionID: "sess", Direction: "request", Body: "{}"})
	}
	w.Stop(2 * time.Second)

	n, err := store.CountForSession(context.Background(), "sess")
	if err != nil {
		t.Fatalf("CountForSession: %v", err)
	}
	if n != 5 {
		t.Errorf("flushed count = %d, want 5", n)
	}
	if w.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0 (buffer was sized for the load)", w.Dropped())
	}
}

// TestDebugEventWriter_DropsOnFullBuffer drives the channel past capacity
// before the worker can drain it. The drop counter must go up and the
// per-row count in the store stays bounded by what got through.
func TestDebugEventWriter_DropsOnFullBuffer(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	// Tiny buffer; do not Start so nothing drains until we submit a flood.
	w := newDebugEventWriterSized(store, 2)

	// Submit 10 events without a worker — only 2 fit in the buffer; the
	// other 8 must be dropped.
	for i := 0; i < 10; i++ {
		w.Submit(DebugEvent{SessionID: "sess", Direction: "request", Body: "{}"})
	}
	if got := w.Dropped(); got != 8 {
		t.Errorf("Dropped = %d, want 8", got)
	}

	// Start the worker now and let it drain; Stop should empty the channel.
	w.Start(context.Background())
	w.Stop(2 * time.Second)
	n, _ := store.CountForSession(context.Background(), "sess")
	if n != 2 {
		t.Errorf("post-flush count = %d, want 2 (8 were dropped before worker ran)", n)
	}
}

// TestDebugEventWriter_StopIsIdempotent ensures multiple Stop calls do not
// panic on a closed channel.
func TestDebugEventWriter_StopIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	w := newDebugEventWriterSized(NewDebugEventStore(db), 4)
	w.Start(context.Background())
	w.Stop(time.Second)
	w.Stop(time.Second)
	w.Stop(time.Second)
}
