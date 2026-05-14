package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

// fakeUserMessagePayload builds a small valid payload for writer tests.
func fakeUserMessagePayload(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(events.UserMessagePayload{
		Header: events.Header{V: events.UserMessageVersion}, Content: "x", ContentLength: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestSessionEventWriter_SubmitFlushesOnStop(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	w := NewSessionEventWriter(store)
	w.Start(context.Background())

	payload := fakeUserMessagePayload(t)
	for i := 0; i < 5; i++ {
		w.Submit(SessionEvent{
			SessionID: "sess-writer",
			EventType: events.TypeUserMessage,
			Payload:   payload,
		})
	}
	w.Stop(2 * time.Second)

	n, err := store.CountForSession(context.Background(), "sess-writer")
	if err != nil {
		t.Fatalf("CountForSession: %v", err)
	}
	if n != 5 {
		t.Errorf("post-Stop count = %d, want 5 (Stop must flush)", n)
	}
	if d := w.Dropped(); d != 0 {
		t.Errorf("dropped = %d, want 0 (buffer was not pressured)", d)
	}
}

func TestSessionEventWriter_DropsWhenBufferFull(t *testing.T) {
	// Build a writer with a deliberately tiny buffer and DO NOT start the
	// drain — every Submit beyond the cap must drop and increment the
	// counter without blocking. Starting the drain would let events leave
	// the channel and defeat the test.
	w := &SessionEventWriter{
		store: nil, // not used: drain never runs
		ch:    make(chan SessionEvent, 2),
		done:  make(chan struct{}),
	}

	payload := json.RawMessage(`{"v":1}`)
	for i := 0; i < 10; i++ {
		w.Submit(SessionEvent{
			SessionID: "sess-overflow",
			EventType: events.TypeUserMessage,
			Payload:   payload,
		})
	}

	dropped := w.Dropped()
	if dropped != 8 {
		t.Errorf("Dropped() = %d, want 8 (buffer cap 2, submitted 10)", dropped)
	}
}

func TestSessionEventWriter_StopIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	store := NewSessionEventStore(db)
	w := NewSessionEventWriter(store)
	w.Start(context.Background())
	w.Stop(1 * time.Second)
	// Second Stop must not panic on double-close of the channel.
	w.Stop(1 * time.Second)
}
