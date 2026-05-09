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
	w := NewDebugEventWriter(store)
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

// TestDebugEventWriter_DropsOnFullBuffer drives the channel past the
// hardcoded capacity before the worker can drain it. The drop counter
// must go up and the per-row count in the store stays bounded by what
// got through.
func TestDebugEventWriter_DropsOnFullBuffer(t *testing.T) {
	db := openTestDB(t)
	store := NewDebugEventStore(db)
	// Do not Start so nothing drains until we submit a flood that exceeds
	// the writer's buffer (cap 100). 150 ensures we also exercise the
	// power-of-two warn cadence (1, 2, 4, 8, 16, 32 → 6 warn lines).
	w := NewDebugEventWriter(store)

	for i := 0; i < 150; i++ {
		w.Submit(DebugEvent{SessionID: "sess", Direction: "request", Body: "{}"})
	}
	if got := w.Dropped(); got != 50 {
		t.Errorf("Dropped = %d, want 50 (150 - 100 buffer cap)", got)
	}

	// Start the worker now and let it drain; Stop should empty the channel.
	w.Start(context.Background())
	w.Stop(2 * time.Second)
	n, _ := store.CountForSession(context.Background(), "sess")
	if n != 100 {
		t.Errorf("post-flush count = %d, want 100 (50 were dropped before worker ran)", n)
	}
}

// TestDebugEventWriter_StopIsIdempotent ensures multiple Stop calls do not
// panic on a closed channel.
func TestDebugEventWriter_StopIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	w := NewDebugEventWriter(NewDebugEventStore(db))
	w.Start(context.Background())
	w.Stop(time.Second)
	w.Stop(time.Second)
	w.Stop(time.Second)
}
