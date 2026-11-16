package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/sessionlock"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

const (
	testNoteKey = "chapter-2"
	testNote    = "[context] The walkthrough created the item Drill 7 with id 42."
)

// reserveNote takes the reservation of testNoteKey in sessionID at at, as the
// open path does before it stores the note, and returns its claim.
func reserveNote(t *testing.T, sessions noteSessions, sessionID string, at time.Time) state.TurnClaim {
	t.Helper()
	if result, err := sessions.ReserveNote(context.Background(), sessionID, testNoteKey, at, time.Minute); err != nil || result != state.ClaimWon {
		t.Fatalf("ReserveNote(%v) = %v, %v", at, result, err)
	}
	return state.TurnClaim{NoteKey: testNoteKey, At: at}
}

// noteSettled reports whether the reservation of testNoteKey was marked
// stored: a stale reservation is then settled, one under which nothing was
// stored is won again. It takes the reservation when it can, so a test calls it
// last.
func noteSettled(t *testing.T, sessions noteSessions, sessionID string) bool {
	t.Helper()
	result, err := sessions.ReserveNote(context.Background(), sessionID, testNoteKey, time.Now().Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("ReserveNote: %v", err)
	}
	return result == state.ClaimSettled
}

func countContent(msgs []provider.Message, content string) int {
	n := 0
	for _, m := range msgs {
		if m.Content == content {
			n++
		}
	}
	return n
}

func noToolCalls() *fakeParser {
	return &fakeParser{parseFn: func(string) []ToolCall { return nil }}
}

// A note is stored as a hidden user row under its reservation, which it marks
// stored, and emits note_stored with its key, content and length. Nothing of a
// turn happens: no other event, no model call, no usage, no summarisation and
// no title although both are due, and the session's metadata (the pending
// confirmation, the latest-user-message id) stays as it was.
func TestStoreNote_StoresAHiddenRowWithoutATurn(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		sessions := kind.open(t)
		sessions.Create(state.SessionParams{ID: "s1"})
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "Show me how to create an item"})
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleAssistant, Content: "Starting the walkthrough."})
		_ = sessions.SetMetadata("s1", lastUserMessageIDMetaKey, "m-1")
		llm := &promptRecordingLLM{fakeLLM: fakeLLM{responses: []string{"unused"}}}
		sink := &recordingEventSink{}
		usage := &usageSpy{}
		orch := NewWithRules(llm, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink: sink, UsageRecorder: usage,
			SummarizeAfterMessages: 1, MaxMessagesAfterSummary: 1, SessionTitlesEnabled: true,
		})
		claim := reserveNote(t, sessions, "s1", time.Now())
		before, _ := sessions.Get("s1")
		metadata := maps.Clone(before.Metadata)

		if err := orch.StoreNote(context.Background(), "s1", testNote, claim); err != nil {
			t.Fatalf("StoreNote: %v", err)
		}

		sess, _ := sessions.Get("s1")
		if len(sess.Messages) != 3 {
			t.Fatalf("stored messages = %+v, want the two of the conversation and the note", sess.Messages)
		}
		if got := sess.Messages[2]; got.Role != provider.RoleUser || got.Visibility != provider.VisibilityHidden || got.Content != testNote {
			t.Errorf("note row = %+v, want a hidden user row with the note", got)
		}
		if !maps.Equal(sess.Metadata, metadata) {
			t.Errorf("session metadata = %v, want it unchanged at %v", sess.Metadata, metadata)
		}
		evs := sink.snapshot()
		if len(evs) != 1 || evs[0].EventType != events.TypeNoteStored || evs[0].SessionID != "s1" {
			t.Fatalf("events = %+v, want one note_stored of s1", evs)
		}
		var payload events.NoteStoredPayload
		if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload.NoteKey != testNoteKey || payload.Content != testNote || payload.ContentLength != len(testNote) {
			t.Errorf("payload = %+v, want key %q, the note and its length", payload, testNoteKey)
		}
		if len(llm.prompts) != 0 || len(usage.calls) != 0 {
			t.Errorf("model prompts %q, usage %+v, want neither", llm.prompts, usage.calls)
		}
		if sess.Summary != "" || sess.Title != "" {
			t.Errorf("summary %q, title %q, want neither", sess.Summary, sess.Title)
		}
		if !noteSettled(t, sessions, "s1") {
			t.Error("the note's reservation was not marked stored")
		}
	})
}

// requestRecordingLLM is fakeLLM that keeps the messages of every request.
type requestRecordingLLM struct {
	fakeLLM
	requests [][]provider.Message
}

func (l *requestRecordingLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.requests = append(l.requests, slices.Clone(req.Messages))
	return l.fakeLLM.Complete(ctx, req)
}

// The next turn sends the note to the model before the user's new message,
// while every derivation of what the user said skips it: the latest user
// message, the planner's context and the reply language follow the user's
// own German request, not the English note after it.
func TestStoreNote_NextTurnReadsTheNote(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		const asked = "Bitte zeige mir, wie ich einen neuen Artikel im Lager anlege."
		const next = "Lösche ihn bitte wieder."
		sessions := kind.open(t)
		sessions.Create(state.SessionParams{ID: "s1"})
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: asked})
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleAssistant, Content: "Ich starte die Führung."})
		llm := &requestRecordingLLM{fakeLLM: fakeLLM{responses: []string{"Erledigt."}}}
		orch := NewWithRules(llm, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})

		if err := orch.StoreNote(context.Background(), "s1", testNote, reserveNote(t, sessions, "s1", time.Now())); err != nil {
			t.Fatalf("StoreNote: %v", err)
		}

		sess, _ := sessions.Get("s1")
		if got := lastUserMessage(sess.Messages); got != asked {
			t.Errorf("latest user message = %q, want the user's request", got)
		}
		if got := buildPlannerConversationContext(sess); strings.Contains(got, testNote) {
			t.Errorf("planner context carries the note:\n%s", got)
		}
		if got := newDetectorOrchestrator().replyLanguageDirectiveFromHistory(sess.Messages); !strings.Contains(got, "Reply in German.") {
			t.Errorf("reply language = %q, want the user's German", got)
		}

		if _, err := orch.Run(context.Background(), "s1", next); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(llm.requests) != 1 {
			t.Fatalf("model requests = %d, want 1", len(llm.requests))
		}
		req := llm.requests[0]
		at := slices.IndexFunc(req, func(m provider.Message) bool { return m.Content == testNote })
		if at < 0 || at >= len(req)-1 || !strings.Contains(req[len(req)-1].Content, next) {
			t.Errorf("request = %+v, want the note before the user's new message", req)
		}
	})
}

// pausingStore holds the first assistant row a turn stores, with the turn's
// session lock held, until resume is closed: in the turns below that row is a
// tool call whose result comes in the next write.
type pausingStore struct {
	noteSessions
	once   sync.Once
	paused chan struct{}
	resume chan struct{}
}

func (s *pausingStore) AddMessage(id string, msg provider.Message) error {
	err := s.noteSessions.AddMessage(id, msg)
	if msg.Role == provider.RoleAssistant {
		s.once.Do(func() {
			close(s.paused)
			<-s.resume
		})
	}
	return err
}

// A note that arrives while a turn sits between storing a tool call and its
// result waits for the turn and is stored after all of the turn's rows, also
// when the turn fails after the tool round.
func TestStoreNote_WaitsForATurnInFlight(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		const toolResponse = "[tool] repo.analyze"
		for _, tc := range []struct {
			name      string
			responses []string
			wantErr   bool
		}{
			{"turn answers", []string{toolResponse, "The code looks good."}, false},
			{"turn fails", []string{toolResponse}, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inner := kind.open(t)
				inner.Create(state.SessionParams{ID: "s1"})
				store := &pausingStore{noteSessions: inner, paused: make(chan struct{}), resume: make(chan struct{})}
				registry := NewToolRegistry()
				_ = registry.Register(PluginCapability{Name: "repo", Actions: []Action{{Name: "analyze"}}}, &echoExecutor{})
				parser := &fakeParser{parseFn: func(r string) []ToolCall {
					if r == toolResponse {
						return []ToolCall{{ID: "call_1", Plugin: "repo", Action: "analyze"}}
					}
					return nil
				}}
				orch := NewWithRules(&fakeLLM{responses: tc.responses}, parser, registry, state.NewMemoryStore(""), store, OrchestratorOpts{})
				claim := reserveNote(t, inner, "s1", time.Now())

				turn := make(chan error, 1)
				go func() {
					_, err := orch.Run(context.Background(), "s1", "Analyze my code")
					turn <- err
				}()
				<-store.paused
				stored := make(chan error, 1)
				go func() { stored <- orch.StoreNote(context.Background(), "s1", testNote, claim) }()
				time.Sleep(20 * time.Millisecond)
				select {
				case err := <-stored:
					t.Fatalf("StoreNote returned (%v) while the turn held the session", err)
				default:
				}
				close(store.resume)

				if err := <-turn; (err != nil) != tc.wantErr {
					t.Fatalf("turn returned %v, want an error: %v", err, tc.wantErr)
				}
				if err := <-stored; err != nil {
					t.Fatalf("StoreNote: %v", err)
				}
				sess, _ := inner.Get("s1")
				msgs := sess.Messages
				if countContent(msgs, testNote) != 1 || msgs[len(msgs)-1].Content != testNote {
					t.Fatalf("stored messages = %+v, want the note once, after the turn's rows", msgs)
				}
				call := slices.IndexFunc(msgs, func(m provider.Message) bool { return strings.HasPrefix(m.Content, "[tool_call]") })
				if call < 0 || !strings.Contains(msgs[call+1].Content, "executed repo.analyze") {
					t.Errorf("stored messages = %+v, want the tool call directly followed by its result", msgs)
				}
			})
		}
	})
}

// R5 across pods: each pod has its own in-pod mutex, so only the turn lease in Redis keeps the note out of the other pod's tool round.
func TestStoreNote_WaitsForATurnOnAnotherPod(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		const toolResponse = "[tool] repo.analyze"
		shared := kind.open(t)
		shared.Create(state.SessionParams{ID: "s1"})
		client := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
		t.Cleanup(func() { _ = client.Close() })
		lease := func() sessionlock.Locker {
			return sessionlock.NewRedis(client, sessionlock.WithPollInterval(10*time.Millisecond))
		}
		paused := &pausingStore{noteSessions: shared, paused: make(chan struct{}), resume: make(chan struct{})}
		registry := NewToolRegistry()
		_ = registry.Register(PluginCapability{Name: "repo", Actions: []Action{{Name: "analyze"}}}, &echoExecutor{})
		parser := &fakeParser{parseFn: func(r string) []ToolCall {
			if r == toolResponse {
				return []ToolCall{{ID: "call_1", Plugin: "repo", Action: "analyze"}}
			}
			return nil
		}}
		turnPod := NewWithRules(&fakeLLM{responses: []string{toolResponse, "The code looks good."}}, parser, registry,
			state.NewMemoryStore(""), paused, OrchestratorOpts{SessionLocker: lease()})
		notePod := NewWithRules(&fakeLLM{}, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), shared,
			OrchestratorOpts{SessionLocker: lease()})
		claim := reserveNote(t, shared, "s1", time.Now())

		turn := make(chan error, 1)
		go func() {
			_, err := turnPod.Run(context.Background(), "s1", "Analyze my code")
			turn <- err
		}()
		<-paused.paused
		stored := make(chan error, 1)
		go func() { stored <- notePod.StoreNote(context.Background(), "s1", testNote, claim) }()
		time.Sleep(100 * time.Millisecond)
		select {
		case err := <-stored:
			t.Fatalf("StoreNote returned (%v) while the other pod's turn held the session", err)
		default:
		}
		close(paused.resume)

		if err := <-turn; err != nil {
			t.Fatalf("turn: %v", err)
		}
		if err := <-stored; err != nil {
			t.Fatalf("StoreNote: %v", err)
		}
		sess, _ := shared.Get("s1")
		msgs := sess.Messages
		if countContent(msgs, testNote) != 1 || msgs[len(msgs)-1].Content != testNote {
			t.Fatalf("stored messages = %+v, want the note once, after the other pod's rows", msgs)
		}
		call := slices.IndexFunc(msgs, func(m provider.Message) bool { return strings.HasPrefix(m.Content, "[tool_call]") })
		if call < 0 || !strings.Contains(msgs[call+1].Content, "executed repo.analyze") {
			t.Errorf("stored messages = %+v, want the tool call directly followed by its result", msgs)
		}
	})
}

// A note that arrives while a summarisation rewrites the session's rows waits
// for it and is stored after the rewrite, which would otherwise drop it.
func TestStoreNote_WaitsForASummarisationInFlight(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		inner := kind.open(t)
		inner.Create(state.SessionParams{ID: "s1"})
		summarising := make(chan struct{}, 1)
		release := make(chan struct{})
		store := &blockingSetSummaryStore{inner: inner, setSummaryStarted: summarising, setSummaryRelease: release}
		orch := NewWithRules(&immediateLLM{resp: "reply"}, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), store,
			OrchestratorOpts{SummarizeAfterMessages: 1, MaxMessagesAfterSummary: 1})
		claim := reserveNote(t, inner, "s1", time.Now())

		if _, err := orch.Run(context.Background(), "s1", "msg1"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		select {
		case <-summarising:
		case <-time.After(2 * time.Second):
			t.Fatal("the summarisation never started")
		}
		stored := make(chan error, 1)
		go func() { stored <- orch.StoreNote(context.Background(), "s1", testNote, claim) }()
		time.Sleep(20 * time.Millisecond)
		select {
		case err := <-stored:
			t.Fatalf("StoreNote returned (%v) while the summarisation held the session", err)
		default:
		}
		close(release)

		if err := <-stored; err != nil {
			t.Fatalf("StoreNote: %v", err)
		}
		sess, _ := inner.Get("s1")
		if n := len(sess.Messages); n == 0 || sess.Messages[n-1].Content != testNote || countContent(sess.Messages, testNote) != 1 {
			t.Errorf("stored messages = %+v, want the note once, after the summarised rows", sess.Messages)
		}
		if peak := atomic.LoadInt32(&store.maxInWrite); peak > 1 {
			t.Errorf("peak concurrent writes = %d, want 1", peak)
		}
	})
}

// A note whose deadline passed while it waited for a long turn stores nothing
// when the turn frees the session, emits nothing and leaves its reservation
// unstored.
func TestStoreNote_PastItsDeadlineStoresNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		llm := newBlockingLLM("done")
		sessions := kind.open(t)
		sessions.Create(state.SessionParams{ID: "s1"})
		sink := &recordingEventSink{}
		orch := NewWithRules(llm, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{EventSink: sink})
		claim := reserveNote(t, sessions, "s1", time.Now())

		long := make(chan error, 1)
		go func() {
			_, err := orch.Run(context.Background(), "s1", "a long user turn")
			long <- err
		}()
		for atomic.LoadInt32(&llm.inflight) == 0 {
			time.Sleep(time.Millisecond)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		stored := make(chan error, 1)
		go func() { stored <- orch.StoreNote(ctx, "s1", testNote, claim) }()
		<-ctx.Done()
		llm.releaseAll()

		if err := <-long; err != nil {
			t.Fatalf("long turn: %v", err)
		}
		if err := <-stored; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("StoreNote returned %v, want its deadline", err)
		}
		sess, _ := sessions.Get("s1")
		if countContent(sess.Messages, testNote) != 0 {
			t.Errorf("the note was stored after its deadline: %+v", sess.Messages)
		}
		for _, e := range sink.snapshot() {
			if e.EventType == events.TypeNoteStored {
				t.Errorf("note_stored emitted for a note that was not stored: %+v", e)
			}
		}
		if noteSettled(t, sessions, "s1") {
			t.Error("the reservation was marked stored")
		}
	})
}

// A note whose reservation another call took over (it went stale and was
// reserved again) stores nothing, emits nothing and answers the lost claim.
func TestStoreNote_ReservationTakenOverStoresNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		sessions := kind.open(t)
		sessions.Create(state.SessionParams{ID: "s1"})
		sink := &recordingEventSink{}
		orch := NewWithRules(&fakeLLM{}, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{EventSink: sink})
		lost := reserveNote(t, sessions, "s1", time.Now())
		reserveNote(t, sessions, "s1", lost.At.Add(2*time.Minute))

		err := orch.StoreNote(context.Background(), "s1", testNote, lost)

		if !errors.Is(err, state.ErrClaimLost) {
			t.Errorf("StoreNote = %v, want state.ErrClaimLost", err)
		}
		if sess, _ := sessions.Get("s1"); len(sess.Messages) != 0 {
			t.Errorf("stored messages = %+v, want none", sess.Messages)
		}
		if evs := sink.snapshot(); len(evs) != 0 {
			t.Errorf("events = %+v, want none", evs)
		}
	})
}

// agentRequestLog is lumLLM that keeps the messages of every agent-loop
// request (those that carry the native tools array).
type agentRequestLog struct {
	*lumLLM
	mu       sync.Mutex
	requests [][]provider.Message
}

func (l *agentRequestLog) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Tools) > 0 {
		l.mu.Lock()
		l.requests = append(l.requests, slices.Clone(req.Messages))
		l.mu.Unlock()
	}
	return l.lumLLM.Complete(ctx, req)
}

// A note stored while a tool call waits for the user's approval leaves the
// confirmation pending; the user's answer then resolves it and runs the call,
// and the model input that follows carries the note but not the resolved
// prompt and its answer, which the note now stands between.
func TestStoreNote_PendingConfirmationStaysPending(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		sessions := kind.open(t)
		h := &lumHarness{t: t, llm: &lumLLM{}, exec: &recordingExecutor{}, sink: &recordingEventSink{}, sessions: sessions}
		h.sessions.Create(state.SessionParams{ID: lumSession})
		log := &agentRequestLog{lumLLM: h.llm}
		h.orch = newLUMOrchestrator(log, h.exec, h.sessions, h.sink, false)
		h.proposeAfterInspect("clean up the old records")

		if err := h.orch.StoreNote(context.Background(), lumSession, testNote, reserveNote(t, sessions, lumSession, time.Now())); err != nil {
			t.Fatalf("StoreNote: %v", err)
		}
		if pc, _, _, _ := loadPendingToolCall(h.sessions, lumSession); pc == nil || pc.Action != "purge" {
			t.Fatalf("pending call after the note = %+v, want purge still pending", pc)
		}

		h.llm.script([]provider.CompletionResponse{{Content: "Removed 3 records."}})
		h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")

		if got := h.idsFor("purge"); len(got) != 1 {
			t.Fatalf("purge ran %d times, want once on the user's answer", len(got))
		}
		sess, _ := h.sessions.Get(lumSession)
		prompt := slices.IndexFunc(sess.Messages, func(m provider.Message) bool { return m.PromptType == provider.PromptTypeToolConfirmation })
		if prompt < 0 {
			t.Fatalf("stored messages = %+v, want the confirmation prompt", sess.Messages)
		}
		last := log.requests[len(log.requests)-1]
		for _, m := range last {
			if m.Content == sess.Messages[prompt].Content || (m.Role == provider.RoleUser && m.Content == "approve") {
				t.Errorf("the resolved exchange reached the model: %+v", m)
			}
		}
		if countContent(last, testNote) != 1 {
			t.Errorf("request after the answer = %+v, want the note in it", last)
		}
	})
}
