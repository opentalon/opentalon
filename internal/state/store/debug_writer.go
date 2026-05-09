package store

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DebugEventWriter buffers debug events on a bounded channel and drains them
// through a single background goroutine. The LLM hot-path calls Submit, which
// is non-blocking: when the channel is full, the event is dropped and a
// counter is bumped so we can warn periodically without flooding logs.
//
// Why a buffer + drop policy: the alternative — synchronous writes from
// inside the OpenAI request/response path — would couple LLM latency to the
// state-store's tail latency, and a slow Postgres flush could stall every
// agent loop. Writes happen at human conversation rate (~ <1 per second
// per active session), so a small buffer (cap 100) and a single worker keep
// per-session event order intact while shielding the hot path entirely.
type DebugEventWriter struct {
	store    *DebugEventStore
	ch       chan DebugEvent
	dropped  atomic.Int64
	stopOnce sync.Once
	done     chan struct{}
}

// NewDebugEventWriter constructs a writer with a buffer cap of 100 — plenty
// for any realistic /debug load (opt-in per session, worker drains at
// ~1k/s on Postgres). The caller must call Start() once and Stop() during
// shutdown.
func NewDebugEventWriter(store *DebugEventStore) *DebugEventWriter {
	return &DebugEventWriter{
		store: store,
		ch:    make(chan DebugEvent, 100),
		done:  make(chan struct{}),
	}
}

// Start launches the worker goroutine. It returns immediately. The worker
// runs until Stop() is called, draining any events still in the buffer
// before exiting. Drop counters are reported every reportEvery batches so
// dashboards see the pressure without each individual drop generating a
// log line.
func (w *DebugEventWriter) Start(ctx context.Context) {
	go w.run(ctx)
}

// Submit enqueues e for asynchronous insert. Never blocks — if the buffer is
// full the event is dropped and the dropped-counter is bumped.
func (w *DebugEventWriter) Submit(e DebugEvent) {
	select {
	case w.ch <- e:
	default:
		n := w.dropped.Add(1)
		// Warn once per power-of-two so the first drop is visible and large
		// drop bursts do not flood logs.
		if n == 1 || n&(n-1) == 0 {
			slog.Warn("debug event buffer full, dropping",
				"session_id", e.SessionID,
				"direction", e.Direction,
				"total_dropped", n,
			)
		}
	}
}

// Stop flushes the buffer and stops the worker. Safe to call multiple times.
// Shutdown waits up to flushTimeout for in-flight events to be persisted.
func (w *DebugEventWriter) Stop(flushTimeout time.Duration) {
	w.stopOnce.Do(func() {
		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(flushTimeout):
			slog.Warn("debug event writer flush timeout exceeded", "timeout", flushTimeout)
		}
	})
}

// Dropped returns the cumulative number of events dropped since process start.
func (w *DebugEventWriter) Dropped() int64 { return w.dropped.Load() }

func (w *DebugEventWriter) run(ctx context.Context) {
	defer close(w.done)
	for e := range w.ch {
		// Use a short per-insert timeout so a stalled DB doesn't block
		// shutdown forever. Errors are logged but never returned —
		// debug capture is best-effort and must never propagate up.
		insertCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := w.store.Insert(insertCtx, e); err != nil {
			slog.Warn("debug event insert failed",
				"session_id", e.SessionID,
				"direction", e.Direction,
				"error", err,
			)
		}
		cancel()
	}
}
