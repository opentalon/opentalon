package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// blockingLLM blocks until unblocked, recording how many calls are in-flight.
type blockingLLM struct {
	inflight int32
	peak     int32
	unblock  chan struct{}
	resp     string
}

func newBlockingLLM(resp string) *blockingLLM {
	return &blockingLLM{unblock: make(chan struct{}), resp: resp}
}

func (b *blockingLLM) releaseAll() { close(b.unblock) }

func (b *blockingLLM) Complete(ctx context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	cur := atomic.AddInt32(&b.inflight, 1)
	defer atomic.AddInt32(&b.inflight, -1)

	// Update peak.
	for {
		old := atomic.LoadInt32(&b.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&b.peak, old, cur) {
			break
		}
	}

	select {
	case <-b.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &provider.CompletionResponse{Content: b.resp}, nil
}

// newConcurrentOrch creates an orchestrator with two sessions pre-created.
func newConcurrentOrch(t *testing.T, maxConcurrent int, llm LLMClient) (*Orchestrator, string, string) {
	t.Helper()
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	sessions.Create("s2", "", "")
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions,
		OrchestratorOpts{MaxConcurrentSessions: maxConcurrent})
	return orch, "s1", "s2"
}

// TestConcurrencyDefault verifies that with the default setting (1), two different
// sessions are processed sequentially — total elapsed time ≈ 2×delay.
func TestConcurrencyDefault(t *testing.T) {
	blk := newBlockingLLM("reply")

	orch, s1, s2 := newConcurrentOrch(t, 0, blk) // 0 → default (1)

	var wg sync.WaitGroup
	wg.Add(2)

	start := time.Now()
	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s1, "hello") //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s2, "hello") //nolint:errcheck
	}()

	// Give both goroutines time to hit the semaphore, then release LLM.
	time.Sleep(20 * time.Millisecond)
	blk.releaseAll()
	wg.Wait()
	_ = time.Since(start)

	// With cap=1, both sessions run one after the other, so at peak only 1 LLM call is in flight.
	if got := atomic.LoadInt32(&blk.peak); got > 1 {
		t.Errorf("peak inflight LLM calls = %d, want ≤1 (sequential)", got)
	}
}

// TestConcurrencyParallel verifies that with MaxConcurrentSessions=2, two different
// sessions are processed in parallel — both LLM calls are in-flight simultaneously.
func TestConcurrencyParallel(t *testing.T) {
	blk := newBlockingLLM("reply")
	orch, s1, s2 := newConcurrentOrch(t, 2, blk)

	bothStarted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s1, "hello") //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s2, "hello") //nolint:errcheck
	}()

	// Poll until both sessions are blocked inside the LLM, or timeout.
	go func() {
		deadline := time.After(2 * time.Second)
		for {
			if atomic.LoadInt32(&blk.inflight) >= 2 {
				close(bothStarted)
				return
			}
			select {
			case <-deadline:
				return
			default:
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	select {
	case <-bothStarted:
		// Both sessions are running concurrently.
	case <-time.After(2 * time.Second):
		t.Error("timed out waiting for both sessions to run concurrently")
	}

	blk.releaseAll()
	wg.Wait()

	if got := atomic.LoadInt32(&blk.peak); got < 2 {
		t.Errorf("peak inflight LLM calls = %d, want ≥2 (parallel)", got)
	}
}

// TestConcurrencySameSessionSerialized verifies that two messages to the same session
// are always serialized, even with MaxConcurrentSessions>1.
func TestConcurrencySameSessionSerialized(t *testing.T) {
	blk := newBlockingLLM("reply")
	orch, s1, _ := newConcurrentOrch(t, 4, blk)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s1, "first") //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		orch.Run(context.Background(), s1, "second") //nolint:errcheck
	}()

	// Give both goroutines time to start, then release.
	time.Sleep(20 * time.Millisecond)
	blk.releaseAll()
	wg.Wait()

	// Both compete for the same session lock, so peak inflight is 1.
	if got := atomic.LoadInt32(&blk.peak); got > 1 {
		t.Errorf("peak inflight LLM calls for same session = %d, want 1 (serialized)", got)
	}
}

// TestConcurrencyContextCancelledWhileWaiting verifies that a Run() call waiting for a
// semaphore slot respects context cancellation.
func TestConcurrencyContextCancelledWhileWaiting(t *testing.T) {
	blk := newBlockingLLM("reply")
	orch, s1, s2 := newConcurrentOrch(t, 1, blk) // cap=1, s1 will hold the slot

	// Start s1 to hold the semaphore slot.
	s1Done := make(chan struct{})
	go func() {
		defer close(s1Done)
		orch.Run(context.Background(), s1, "occupying slot") //nolint:errcheck
	}()

	// Wait until s1 is inside the LLM (slot acquired).
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&blk.inflight) == 0 {
		select {
		case <-deadline:
			t.Fatal("s1 never acquired the semaphore")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// s2 tries to acquire but its context is already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := orch.Run(ctx, s2, "should be rejected")
	if err == nil {
		t.Error("expected context error, got nil")
	}

	blk.releaseAll()
	<-s1Done
}

// immediateLLM is a stateless LLM safe for concurrent use.
type immediateLLM struct{ resp string }

func (l *immediateLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{Content: l.resp}, nil
}

// blockingSetSummaryStore wraps SessionStoreInterface, blocking inside SetSummary until
// released by the test. It tracks concurrent write operations via atomic counters so
// the test can detect overlapping AddMessage / SetSummary calls.
type blockingSetSummaryStore struct {
	inner             SessionStoreInterface
	setSummaryStarted chan<- struct{}
	setSummaryRelease <-chan struct{}
	inWrite           int32 // atomic: number of write ops currently in-flight
	maxInWrite        int32 // atomic: peak value of inWrite
}

func (s *blockingSetSummaryStore) writeStart() {
	n := atomic.AddInt32(&s.inWrite, 1)
	for {
		old := atomic.LoadInt32(&s.maxInWrite)
		if n <= old || atomic.CompareAndSwapInt32(&s.maxInWrite, old, n) {
			break
		}
	}
}

func (s *blockingSetSummaryStore) writeEnd() { atomic.AddInt32(&s.inWrite, -1) }

func (s *blockingSetSummaryStore) Get(id string) (*state.Session, error) { return s.inner.Get(id) }
func (s *blockingSetSummaryStore) Create(id, entityID, groupID string) *state.Session {
	return s.inner.Create(id, entityID, groupID)
}
func (s *blockingSetSummaryStore) Delete(id string) error { return s.inner.Delete(id) }
func (s *blockingSetSummaryStore) SetModel(id string, m provider.ModelRef) error {
	return s.inner.SetModel(id, m)
}
func (s *blockingSetSummaryStore) AddMessage(id string, msg provider.Message) error {
	s.writeStart()
	defer s.writeEnd()
	return s.inner.AddMessage(id, msg)
}
func (s *blockingSetSummaryStore) SetMetadata(id, key, value string) error {
	return s.inner.SetMetadata(id, key, value)
}
func (s *blockingSetSummaryStore) SetSummary(id, summary string, msgs []provider.Message) error {
	s.writeStart()
	// Signal the test that we're inside SetSummary, then block until released.
	select {
	case s.setSummaryStarted <- struct{}{}:
	default:
	}
	<-s.setSummaryRelease
	err := s.inner.SetSummary(id, summary, msgs)
	s.writeEnd()
	return err
}

// TestSummarizeGoroutineHoldsSessionLock verifies that the background goroutine spawned
// by Run() to run maybeSummarizeSession acquires and holds the per-session lock for its
// entire execution, preventing a concurrent Run() on the same session from interleaving
// its own session writes with SetSummary.
func TestSummarizeGoroutineHoldsSessionLock(t *testing.T) {
	const sessionID = "sess"

	setSummaryStarted := make(chan struct{}, 1)
	setSummaryRelease := make(chan struct{})

	inner := state.NewSessionStore("")
	inner.Create(sessionID, "", "")
	ts := &blockingSetSummaryStore{
		inner:             inner,
		setSummaryStarted: setSummaryStarted,
		setSummaryRelease: setSummaryRelease,
	}

	orch := NewWithRules(
		&immediateLLM{resp: "reply"},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		NewToolRegistry(), state.NewMemoryStore(""), ts,
		OrchestratorOpts{
			MaxConcurrentSessions:   2, // both goroutines can run in parallel
			SummarizeAfterMessages:  1, // trigger summarization after the first message
			MaxMessagesAfterSummary: 1,
		},
	)

	// First Run: returns quickly, then spawns the background summarize goroutine.
	if _, err := orch.Run(context.Background(), sessionID, "msg1"); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Wait until the summarize goroutine is inside SetSummary (holding the session lock).
	select {
	case <-setSummaryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("summarize goroutine never called SetSummary")
	}

	// Start a second Run on the same session. With the fix it blocks on the per-session
	// lock; without the fix it would race into AddMessage while SetSummary is blocked.
	run2Done := make(chan error, 1)
	go func() {
		_, err := orch.Run(context.Background(), sessionID, "msg2")
		run2Done <- err
	}()

	// Give the second Run time to reach and block on the session lock.
	time.Sleep(20 * time.Millisecond)

	// Release SetSummary; the goroutine finishes and gives up the session lock,
	// allowing the second Run to proceed.
	close(setSummaryRelease)

	select {
	case err := <-run2Done:
		if err != nil {
			t.Fatalf("second Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Run timed out")
	}

	if got := atomic.LoadInt32(&ts.maxInWrite); got > 1 {
		t.Errorf("concurrent session writes detected: maxInWrite = %d, want ≤1", got)
	}
}

// TestConcurrencySessionLockCleanup verifies that per-session mutexes are removed from
// the map after use so it doesn't grow unboundedly.
func TestConcurrencySessionLockCleanup(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")

	// Create many unique sessions.
	for i := 0; i < 20; i++ {
		sessions.Create(string(rune('a'+i)), "", "")
	}

	orch := NewWithRules(&immediateLLM{resp: "ok"}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{MaxConcurrentSessions: 5})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		sid := string(rune('a' + i))
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			orch.Run(context.Background(), id, "msg") //nolint:errcheck
		}(sid)
	}
	wg.Wait()

	orch.sessionMuxMu.Lock()
	remaining := len(orch.sessionMuxes)
	orch.sessionMuxMu.Unlock()

	if remaining != 0 {
		t.Errorf("session mutex map has %d entries after all runs completed, want 0", remaining)
	}
}
