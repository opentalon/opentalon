// Package eventwebhook forwards selected session-event types to an
// out-of-process consumer over HTTP. It is the generic push counterpart to
// the api-plugin's since_seq pull: a consumer that wants low-latency
// notifications (e.g. a UI activity indicator that has to survive a page
// reload) subscribes to the event types it cares about instead of polling.
//
// It is an emit.Sink, so it composes with the persistent session-event
// writer via emit.MultiSink — the two tee off the same producer choke
// point and neither knows the other exists.
//
// Delivery contract:
//   - Non-blocking: Emit enqueues onto a bounded buffer and returns. A full
//     buffer drops the event (counter + throttled log) rather than back-
//     pressuring the orchestrator hot path — identical policy to the
//     session-event writer it sits beside.
//   - Best-effort, at-least-once: a background worker POSTs each event with
//     a short per-request timeout and a small bounded retry (network errors,
//     5xx, and 429 only — a 4xx is the consumer's contract error, not worth
//     retrying). Events carry the producer-generated event id, stable across
//     this push and the pull endpoint, so the consumer dedups on it. A
//     permanently-dropped push never corrupts consumer state because the
//     pull endpoint remains the source of truth.
//   - Filtered: only subscribed event types are marshalled and sent;
//     everything else returns from Emit immediately with no allocation.
package eventwebhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// Defaults applied by New when the corresponding Option is unset.
const (
	DefaultTimeout    = 5 * time.Second
	DefaultBufferSize = 1000
	DefaultMaxRetries = 2
	retryBackoff      = 200 * time.Millisecond
)

// Options configures a Sink. URL and at least one EventType are required.
// Header values are expected to be already env-expanded by the caller.
type Options struct {
	URL        string
	EventTypes []string          // allowlist of event types to forward; empty is rejected
	Headers    map[string]string // static headers added to every request
	Timeout    time.Duration     // per-request timeout; <=0 → DefaultTimeout
	BufferSize int               // bounded buffer capacity; <=0 → DefaultBufferSize
	MaxRetries int               // retry attempts beyond the first; <=0 → DefaultMaxRetries
}

// Sink is an emit.Sink that POSTs subscribed events to a URL. Construct it
// with New, then Start the worker and Stop it during shutdown.
type Sink struct {
	url        string
	headers    map[string]string
	eventTypes map[string]struct{}
	client     *http.Client
	maxRetries int

	ch       chan emit.Event
	dropped  atomic.Int64
	stopOnce sync.Once
	done     chan struct{}
}

// New validates opts and constructs a Sink. It rejects an empty URL, an
// empty subscription list, and any event type not in events.AllEventTypes
// — the last one turns an operator's typo (which would otherwise silently
// forward nothing) into a boot-time error.
func New(opts Options) (*Sink, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("eventwebhook: url is required")
	}
	if len(opts.EventTypes) == 0 {
		return nil, fmt.Errorf("eventwebhook: at least one event_type is required")
	}

	types := make(map[string]struct{}, len(opts.EventTypes))
	var unknown []string
	for _, t := range opts.EventTypes {
		if !events.IsKnownEventType(t) {
			unknown = append(unknown, t)
			continue
		}
		types[t] = struct{}{}
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("eventwebhook: unknown event_type(s): %v", unknown)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	bufSize := opts.BufferSize
	if bufSize <= 0 {
		bufSize = DefaultBufferSize
	}
	maxRetries := opts.MaxRetries
	if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}

	// Copy headers so a later config mutation can't race the worker.
	headers := make(map[string]string, len(opts.Headers))
	for k, v := range opts.Headers {
		headers[k] = v
	}

	return &Sink{
		url:        opts.URL,
		headers:    headers,
		eventTypes: types,
		client:     &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		ch:         make(chan emit.Event, bufSize),
		done:       make(chan struct{}),
	}, nil
}

// Emit enqueues evt for delivery when its type is subscribed. Non-blocking:
// a subscribed event that finds the buffer full is dropped (counter +
// throttled log). The incoming ctx is intentionally ignored — it is the
// orchestrator's per-turn ctx, which is often cancelled the moment the turn
// returns, and the POST must outlive it. Satisfies emit.Sink.
func (s *Sink) Emit(_ context.Context, evt emit.Event) {
	if _, ok := s.eventTypes[evt.EventType]; !ok {
		return
	}
	select {
	case s.ch <- evt:
	default:
		n := s.dropped.Add(1)
		if n == 1 || n&(n-1) == 0 {
			slog.Warn("event webhook buffer full, dropping",
				"event_type", evt.EventType,
				"session_id", evt.SessionID,
				"total_dropped", n,
			)
		}
	}
}

// Start launches the delivery worker. Pass a long-lived context (e.g.
// context.Background()) so a graceful Stop can flush in-flight events even
// after the application context is cancelled — the per-request timeout
// bounds each POST regardless.
func (s *Sink) Start(ctx context.Context) {
	go s.run(ctx)
}

// Stop closes the buffer and waits up to flushTimeout for the worker to
// drain it. Safe to call multiple times.
func (s *Sink) Stop(flushTimeout time.Duration) {
	s.stopOnce.Do(func() {
		close(s.ch)
		select {
		case <-s.done:
		case <-time.After(flushTimeout):
			slog.Warn("event webhook flush timeout exceeded", "timeout", flushTimeout)
		}
	})
}

// Dropped returns the cumulative number of events dropped since start.
func (s *Sink) Dropped() int64 { return s.dropped.Load() }

func (s *Sink) run(ctx context.Context) {
	defer close(s.done)
	for evt := range s.ch {
		s.deliver(ctx, evt)
	}
}

// deliver POSTs one event, retrying on transient failures up to maxRetries.
func (s *Sink) deliver(ctx context.Context, evt emit.Event) {
	body, err := json.Marshal(toEnvelope(evt))
	if err != nil {
		slog.Warn("event webhook: envelope marshal failed",
			"event_type", evt.EventType, "error", err)
		return
	}

	for attempt := 0; ; attempt++ {
		status, err := s.post(ctx, body)
		if err == nil && status >= 200 && status < 300 {
			return
		}
		retryable := err != nil || status >= 500 || status == http.StatusTooManyRequests
		if !retryable || attempt >= s.maxRetries {
			slog.Warn("event webhook: delivery failed",
				"event_type", evt.EventType,
				"session_id", evt.SessionID,
				"attempts", attempt+1,
				"status", status,
				"error", err,
			)
			return
		}
		time.Sleep(retryBackoff * time.Duration(attempt+1))
	}
}

func (s *Sink) post(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body so the connection can be reused by keep-alive.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// envelope is the JSON body POSTed for one event. It mirrors the fields a
// consumer also sees from the api-plugin pull endpoint, minus seq — seq is
// assigned at persist time and not known at push time, so the event id is
// the dedup key across both delivery paths. Payload is the raw event-type-
// specific JSON, forwarded verbatim.
type envelope struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	EventType  string          `json:"event_type"`
	ParentID   string          `json:"parent_id,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Payload    json.RawMessage `json:"payload"`
}

func toEnvelope(evt emit.Event) envelope {
	return envelope{
		ID:         evt.ID,
		SessionID:  evt.SessionID,
		EventType:  evt.EventType,
		ParentID:   evt.ParentID,
		DurationMS: evt.DurationMS,
		Payload:    evt.Payload,
	}
}
