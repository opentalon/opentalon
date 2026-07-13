package eventwebhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

func drainStop(t *testing.T, s *Sink) {
	t.Helper()
	s.Stop(2 * time.Second)
}

func TestNew_RejectsBadConfig(t *testing.T) {
	if _, err := New(Options{EventTypes: []string{events.TypeTurnFinished}}); err == nil {
		t.Error("empty URL: want error, got nil")
	}
	if _, err := New(Options{URL: "http://x"}); err == nil {
		t.Error("empty EventTypes: want error, got nil")
	}
	if _, err := New(Options{URL: "http://x", EventTypes: []string{"turn_finsihed"}}); err == nil {
		t.Error("unknown event type: want error, got nil")
	}
	if _, err := New(Options{URL: "http://x", EventTypes: []string{events.TypeTurnFinished}}); err != nil {
		t.Errorf("valid config: want nil, got %v", err)
	}
}

func TestSink_DeliversSubscribedAndSkipsRest(t *testing.T) {
	received := make(chan envelope, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Secret"); got != "s3cr3t" {
			t.Errorf("header X-Secret = %q, want s3cr3t", got)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var env envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("decode body: %v", err)
		}
		received <- env
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Options{
		URL:        srv.URL,
		EventTypes: []string{events.TypeUserMessage, events.TypeTurnFinished},
		Headers:    map[string]string{"X-Secret": "s3cr3t"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Start(context.Background())
	defer drainStop(t, s)

	// Subscribed.
	s.Emit(context.Background(), emit.Event{
		ID: "evt-1", SessionID: "e:c:conv", GroupID: "acct-42", EventType: events.TypeTurnFinished,
		Payload: json.RawMessage(`{"v":1,"outcome":"answered"}`),
	})
	// Not subscribed — must be filtered out, never delivered.
	s.Emit(context.Background(), emit.Event{
		ID: "evt-2", SessionID: "e:c:conv", EventType: events.TypeLLMResponse,
		Payload: json.RawMessage(`{"v":1}`),
	})

	select {
	case env := <-received:
		if env.ID != "evt-1" || env.EventType != events.TypeTurnFinished {
			t.Errorf("unexpected envelope: %+v", env)
		}
		if env.SessionID != "e:c:conv" {
			t.Errorf("session_id = %q, want e:c:conv", env.SessionID)
		}
		if env.GroupID != "acct-42" {
			t.Errorf("group_id = %q, want acct-42", env.GroupID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscribed event not delivered within timeout")
	}

	// The filtered event must not arrive.
	select {
	case env := <-received:
		t.Errorf("filtered event was delivered: %+v", env)
	case <-time.After(150 * time.Millisecond):
	}

	// Counters reflect exactly one successful delivery and no failures.
	waitCounter(t, "Delivered", s.Delivered, 1)
	if got := s.Failed(); got != 0 {
		t.Errorf("Failed() = %d, want 0", got)
	}
}

// waitCounter polls a cumulative counter until it reaches want — the worker
// bumps it just after the HTTP round-trip, so an immediate assert may race
// the handler that unblocked the test.
func waitCounter(t *testing.T, name string, read func() int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if read() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%s() = %d, want %d", name, read(), want)
}

func TestSink_RetriesOnServerError(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		close(done)
	}))
	defer srv.Close()

	s, err := New(Options{
		URL:        srv.URL,
		EventTypes: []string{events.TypeTurnFinished},
		MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Start(context.Background())
	defer drainStop(t, s)

	s.Emit(context.Background(), emit.Event{
		ID: "evt-1", EventType: events.TypeTurnFinished,
		Payload: json.RawMessage(`{"v":1}`),
	})

	select {
	case <-done:
		if got := calls.Load(); got != 3 {
			t.Errorf("server calls = %d, want 3 (2 failures + 1 success)", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("did not succeed after retries; calls=%d", calls.Load())
	}

	// A retried-through delivery counts once as delivered, never as failed.
	waitCounter(t, "Delivered", s.Delivered, 1)
	if got := s.Failed(); got != 0 {
		t.Errorf("Failed() = %d, want 0", got)
	}
}

// TestSink_CountsGivenUpDeliveriesAsFailed pins the failure counter: a
// non-retryable status (400) is given up immediately and must increment
// Failed, not Delivered — the delivered/failed pair is what makes a dead
// or rejected webhook visible on /metrics without log archaeology.
func TestSink_CountsGivenUpDeliveriesAsFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	s, err := New(Options{URL: srv.URL, EventTypes: []string{events.TypeTurnFinished}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Start(context.Background())
	defer drainStop(t, s)

	s.Emit(context.Background(), emit.Event{
		ID: "evt-1", EventType: events.TypeTurnFinished,
		Payload: json.RawMessage(`{"v":1}`),
	})

	waitCounter(t, "Failed", s.Failed, 1)
	if got := s.Delivered(); got != 0 {
		t.Errorf("Delivered() = %d, want 0", got)
	}
}

func TestSink_DropsWhenBufferFull(t *testing.T) {
	// A server that blocks forever so the worker is stuck on the first event
	// and the tiny buffer overflows on the rest.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(block)

	s, err := New(Options{
		URL:        srv.URL,
		EventTypes: []string{events.TypeTurnFinished},
		BufferSize: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Start(context.Background())

	for i := 0; i < 50; i++ {
		s.Emit(context.Background(), emit.Event{
			ID: "e", EventType: events.TypeTurnFinished, Payload: json.RawMessage(`{"v":1}`),
		})
	}
	// The worker holds one, the buffer holds one; the rest must have dropped.
	if s.Dropped() == 0 {
		t.Error("expected some drops with a full buffer, got 0")
	}
}
