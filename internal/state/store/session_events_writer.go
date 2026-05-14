package store

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// SessionEventWriter buffers session_events on a bounded channel and
// drains them through a single background goroutine. Producers call
// Submit on the orchestrator hot-path; the call is non-blocking and an
// over-full buffer drops the event with a counter.
//
// Why a buffer + drop policy mirrors DebugEventWriter: the orchestrator
// emits multiple events per turn (turn_start, llm_request, llm_response,
// one tool_call_extracted + one tool_call_result per call, …) and we
// must never gate user-visible LLM latency on database flush time.
// Volume is bounded by human conversation rate per session, so a small
// buffer keeps order intact while shielding the hot path.
//
// The buffer cap is intentionally larger than DebugEventWriter's 100:
// session_events is always-on (not /debug-opt-in), so a burst of activity
// across concurrent sessions can fan in more events than the debug path.
type SessionEventWriter struct {
	store    *SessionEventStore
	ch       chan SessionEvent
	dropped  atomic.Int64
	stopOnce sync.Once
	done     chan struct{}
}

// SessionEventBufferCap is the in-memory channel capacity for the writer.
// 1000 covers the worst realistic burst (full orchestrator turn × ~20
// concurrent active sessions × safety margin) while keeping memory usage
// bounded. Not configurable: the right answer is "deep enough that drops
// are theoretical", and exposing a knob just invites misconfiguration.
const SessionEventBufferCap = 1000

// NewSessionEventWriter constructs a writer with the default buffer cap.
// The caller must call Start() once and Stop() during shutdown.
func NewSessionEventWriter(store *SessionEventStore) *SessionEventWriter {
	return &SessionEventWriter{
		store: store,
		ch:    make(chan SessionEvent, SessionEventBufferCap),
		done:  make(chan struct{}),
	}
}

// Start launches the worker goroutine. It returns immediately. The worker
// runs until Stop() is called, draining any events still in the buffer
// before exiting.
func (w *SessionEventWriter) Start(ctx context.Context) {
	go w.run(ctx)
}

// Submit enqueues e for asynchronous insert. Never blocks — if the buffer
// is full the event is dropped and the dropped-counter is bumped. A log
// line is emitted on the first drop and on every subsequent power-of-two
// drop so dashboards see backpressure without log floods.
func (w *SessionEventWriter) Submit(e SessionEvent) {
	select {
	case w.ch <- e:
	default:
		n := w.dropped.Add(1)
		if n == 1 || n&(n-1) == 0 {
			slog.Warn("session event buffer full, dropping",
				"session_id", e.SessionID,
				"event_type", e.EventType,
				"total_dropped", n,
			)
		}
	}
}

// Stop flushes the buffer and stops the worker. Safe to call multiple
// times. Shutdown waits up to flushTimeout for in-flight events to be
// persisted; mirrors DebugEventWriter.Stop.
func (w *SessionEventWriter) Stop(flushTimeout time.Duration) {
	w.stopOnce.Do(func() {
		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(flushTimeout):
			slog.Warn("session event writer flush timeout exceeded", "timeout", flushTimeout)
		}
	})
}

// Dropped returns the cumulative number of events dropped since process
// start. Useful for a /healthz-style observability surface.
func (w *SessionEventWriter) Dropped() int64 { return w.dropped.Load() }

func (w *SessionEventWriter) run(ctx context.Context) {
	defer close(w.done)
	for e := range w.ch {
		insertCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := w.store.Insert(insertCtx, e); err != nil {
			slog.Warn("session event insert failed",
				"session_id", e.SessionID,
				"event_type", e.EventType,
				"error", err,
			)
		}
		cancel()
	}
}
