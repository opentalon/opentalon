package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// recordingLocker records every distributed Lock/release call so tests can
// assert the cross-pod turn lease brackets Run's critical section.
type recordingLocker struct {
	mu       sync.Mutex
	locks    []string // session keys passed to Lock, in order
	releases []string // session keys whose release func ran, in order
	lockErr  error    // when set, Lock fails with this error
}

func (r *recordingLocker) Lock(_ context.Context, sessionKey string) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lockErr != nil {
		return nil, r.lockErr
	}
	r.locks = append(r.locks, sessionKey)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.releases = append(r.releases, sessionKey)
	}, nil
}

func (r *recordingLocker) snapshot() (locks, releases []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.locks...), append([]string(nil), r.releases...)
}

func newLockerTestOrch(locker *recordingLocker, llm LLMClient) (*Orchestrator, *state.SessionStore) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{SessionLocker: locker})
	return orch, sessions
}

// The distributed turn lease is acquired once per Run for the session key
// and released when the turn finishes.
func TestRun_AcquiresAndReleasesSessionLocker(t *testing.T) {
	locker := &recordingLocker{}
	orch, sessions := newLockerTestOrch(locker, &fakeLLM{responses: []string{"reply"}})
	sessions.Create("s1", "", "", "")

	if _, err := orch.Run(context.Background(), "s1", "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	locks, releases := locker.snapshot()
	if len(locks) != 1 || locks[0] != "s1" {
		t.Fatalf("expected exactly one distributed Lock for s1, got %v", locks)
	}
	if len(releases) != 1 || releases[0] != "s1" {
		t.Fatalf("expected exactly one distributed release for s1, got %v", releases)
	}
}

// The lease is released even when Run exits on an error path (here: the
// session lookup fails because the session does not exist).
func TestRun_ReleasesSessionLockerOnError(t *testing.T) {
	locker := &recordingLocker{}
	orch, _ := newLockerTestOrch(locker, &fakeLLM{responses: []string{"reply"}})

	if _, err := orch.Run(context.Background(), "missing", "hello"); err == nil {
		t.Fatal("expected Run to fail for a missing session")
	}

	locks, releases := locker.snapshot()
	if len(locks) != 1 || locks[0] != "missing" {
		t.Fatalf("expected exactly one distributed Lock, got %v", locks)
	}
	if len(releases) != 1 || releases[0] != "missing" {
		t.Fatalf("release must run on the error path too, got %v", releases)
	}
}

// seedSummarizableSession configures the summarizer thresholds and commits
// enough messages that maybeSummarizeSession attempts the rewrite.
func seedSummarizableSession(t *testing.T, orch *Orchestrator, sessions *state.SessionStore, sessionID string) {
	t.Helper()
	sessions.Create(sessionID, "", "", "")
	orch.summarizeAfterMessages = 2
	orch.maxMessagesAfterSummary = 1
	for i := 0; i < 3; i++ {
		if err := sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: "m"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}
}

// The summarizer rewrites the session's message rows (SetSummary deletes and
// reinserts), so it must hold the same cross-pod turn lease as Run — and
// release it — even though it runs from a detached background goroutine.
func TestMaybeSummarizeSession_AcquiresAndReleasesSessionLocker(t *testing.T) {
	locker := &recordingLocker{}
	orch, sessions := newLockerTestOrch(locker, &fakeLLM{responses: []string{"a summary"}})
	seedSummarizableSession(t, orch, sessions, "s1")

	orch.maybeSummarizeSession(context.Background(), "s1")

	locks, releases := locker.snapshot()
	if len(locks) != 1 || locks[0] != "s1" {
		t.Fatalf("expected exactly one distributed Lock for s1, got %v", locks)
	}
	if len(releases) != 1 || releases[0] != "s1" {
		t.Fatalf("expected exactly one distributed release for s1, got %v", releases)
	}
	sess, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sess.Summary != "a summary" {
		t.Fatalf("Summary = %q, want the LLM output committed under the lease", sess.Summary)
	}
}

// The lease is released on the summarizer's error path too (the LLM call
// fails after the lease is held).
func TestMaybeSummarizeSession_ReleasesSessionLockerOnLLMError(t *testing.T) {
	locker := &recordingLocker{}
	orch, sessions := newLockerTestOrch(locker, &fakeLLM{}) // no responses → Complete errors
	seedSummarizableSession(t, orch, sessions, "s1")

	orch.maybeSummarizeSession(context.Background(), "s1")

	locks, releases := locker.snapshot()
	if len(locks) != 1 || len(releases) != 1 {
		t.Fatalf("lease must be acquired and released on the LLM-error path, got locks=%v releases=%v", locks, releases)
	}
}

// When the lease cannot be acquired the summarizer must not touch the
// session at all: no LLM call, no message rewrite.
func TestMaybeSummarizeSession_SkipsRewriteWhenLockerFails(t *testing.T) {
	locker := &recordingLocker{lockErr: errors.New("lease wait aborted")}
	llm := &fakeLLM{responses: []string{"a summary"}}
	orch, sessions := newLockerTestOrch(locker, llm)
	seedSummarizableSession(t, orch, sessions, "s1")

	orch.maybeSummarizeSession(context.Background(), "s1")

	if llm.callCount != 0 {
		t.Fatal("summarizer must not call the LLM without the turn lease")
	}
	sess, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(sess.Messages) != 3 || sess.Summary != "" {
		t.Fatalf("session must be untouched without the lease, got %d messages, summary %q", len(sess.Messages), sess.Summary)
	}
}

// When the locker itself fails (context ended while waiting for the lease),
// Run propagates the error without starting the turn.
func TestRun_PropagatesSessionLockerError(t *testing.T) {
	wantErr := errors.New("lease wait aborted")
	locker := &recordingLocker{lockErr: wantErr}
	llm := &fakeLLM{responses: []string{"reply"}}
	orch, sessions := newLockerTestOrch(locker, llm)
	sessions.Create("s1", "", "", "")

	if _, err := orch.Run(context.Background(), "s1", "hello"); !errors.Is(err, wantErr) {
		t.Fatalf("expected locker error to propagate, got %v", err)
	}
	if llm.callCount != 0 {
		t.Fatal("turn must not start when the distributed lock cannot be acquired")
	}
}
