package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// isVisibleUserMessage is the single predicate every "what did the user say"
// derivation routes through, so a hidden system-injected note (stored as
// role=user but model-only) can never be mistaken for the user's own words.
func TestIsVisibleUserMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  provider.Message
		want bool
	}{
		{"visible user", provider.Message{Role: provider.RoleUser, Content: "hi"}, true},
		{"hidden user (system note)", provider.Message{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[system] done"}, false},
		{"assistant", provider.Message{Role: provider.RoleAssistant, Content: "hello"}, false},
		{"tool result", provider.Message{Role: provider.RoleTool, Content: "result"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVisibleUserMessage(tc.msg); got != tc.want {
				t.Errorf("isVisibleUserMessage(%+v) = %v, want %v", tc.msg, got, tc.want)
			}
		})
	}
}

// lastUserMessage must return the most recent VISIBLE user message, skipping a
// hidden system-injected note that sits after the real user turn — otherwise
// the RAG search enrichment and reply-language fallback inherit the note.
func TestLastUserMessage_SkipsHiddenNote(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "create a ticket for the broken drill"},
		{Role: provider.RoleAssistant, Content: "working on it"},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[system] job finished"},
	}
	if got := lastUserMessage(msgs); got != "create a ticket for the broken drill" {
		t.Errorf("lastUserMessage = %q, want the visible user turn (hidden note must be skipped)", got)
	}
}

// buildPlannerConversationContext must exclude hidden system-injected notes so
// the planner never treats a machine note as the user's intent — matching its
// own contract ("no ... system messages").
func TestBuildPlannerConversationContext_ExcludesHiddenNote(t *testing.T) {
	sess := &state.Session{Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "list all defective devices"},
		{Role: provider.RoleAssistant, Content: "here they are"},
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden,
			Content: "[system] SECRET job note that must not leak into planner context"},
	}}
	got := buildPlannerConversationContext(sess)
	if strings.Contains(got, "SECRET job note") {
		t.Errorf("planner context must exclude the hidden system note, got:\n%s", got)
	}
	if !strings.Contains(got, "User: list all defective devices") {
		t.Errorf("planner context should include the visible user turn, got:\n%s", got)
	}
}

// promptRecordingLLM is fakeLLM that keeps the last message of every request.
type promptRecordingLLM struct {
	fakeLLM
	prompts []string
}

func (l *promptRecordingLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.prompts = append(l.prompts, req.Messages[len(req.Messages)-1].Content)
	return l.fakeLLM.Complete(ctx, req)
}

// The session title describes what the user wrote: a hidden turn the assistant
// answered (the context a server opened the session with, a job note) leaves
// the session untitled, and the user's first visible message is the source.
func TestMaybeGenerateTitle_IgnoresHiddenTurns(t *testing.T) {
	opened := []provider.Message{
		{Role: provider.RoleUser, Visibility: provider.VisibilityHidden, Content: "[context] the setup created three samples"},
		{Role: provider.RoleAssistant, Content: "Your account is ready. Shall I show you the next tour?"},
	}
	replied := append(slices.Clone(opened),
		provider.Message{Role: provider.RoleUser, Content: "Yes, show me how tickets work"},
		provider.Message{Role: provider.RoleAssistant, Content: "Here is how tickets work."},
	)
	for _, tc := range []struct {
		name    string
		msgs    []provider.Message
		prompts []string
	}{
		{"hidden turn only", opened, nil},
		{"after the user's reply", replied, []string{"Yes, show me how tickets work"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "sess"})
			for _, m := range tc.msgs {
				_ = sessions.AddMessage("sess", m)
			}
			llm := &promptRecordingLLM{fakeLLM: fakeLLM{responses: []string{"Tickets tour"}}}
			orch := NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions,
				OrchestratorOpts{SessionTitlesEnabled: true})

			orch.maybeGenerateTitle(context.Background(), "sess")

			if !slices.Equal(llm.prompts, tc.prompts) {
				t.Errorf("title prompts = %q, want %q", llm.prompts, tc.prompts)
			}
		})
	}
}

// A claimed turn's user message is stored under its claim (WithTurnClaim),
// which marks the claim stored: a stale claim can then never be taken again to
// run the turn twice. That holds for the first turn of an opened session and
// for a keyed note; an unclaimed turn of the same session marks neither.
func TestRun_StoresAClaimedTurnUnderItsClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claim   *state.TurnClaim
		settled string // the claim the run must settle: "opener", "note" or ""
	}{
		{"first turn", &state.TurnClaim{}, "opener"},
		{"keyed note", &state.TurnClaim{NoteKey: "chapter-2"}, "note"},
		{"unclaimed turn", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			if _, _, err := sessions.OpenSession(context.Background(), state.SessionParams{ID: "s1", EntityID: "u1", OpenedBy: "origin", OpenKey: "event-1"}); err != nil {
				t.Fatalf("OpenSession: %v", err)
			}
			orch := NewWithRules(&fakeLLM{responses: []string{"Noted."}},
				&fakeParser{parseFn: func(string) []ToolCall { return nil }},
				NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			now := time.Now()
			if result, err := sessions.ClaimOpener(context.Background(), "s1", now, time.Minute); err != nil || result != state.ClaimWon {
				t.Fatalf("ClaimOpener = %v, %v", result, err)
			}
			if result, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", now, time.Minute); err != nil || result != state.ClaimWon {
				t.Fatalf("ReserveNote = %v, %v", result, err)
			}

			ctx := actor.WithVisibility(context.Background(), provider.VisibilityHidden)
			if tc.claim != nil {
				c := *tc.claim
				c.At = now
				ctx = WithTurnClaim(ctx, c)
			}
			if _, err := orch.Run(ctx, "s1", "[context] chapter two was caught up"); err != nil {
				t.Fatalf("Run: %v", err)
			}

			later := now.Add(time.Hour)
			opener, err := sessions.ClaimOpener(context.Background(), "s1", later, time.Minute)
			if err != nil || (opener == state.ClaimWon) != (tc.settled != "opener") {
				t.Errorf("stale opener claim taken again = %v (%v), want won %v", opener, err, tc.settled != "opener")
			}
			note, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", later, time.Minute)
			if err != nil || (note == state.ClaimWon) != (tc.settled != "note") {
				t.Errorf("stale note reservation taken again = %v (%v), want won %v", note, err, tc.settled != "note")
			}
		})
	}
}

// A claimed turn whose claim another run took over (it went stale, the other
// run re-claimed it) stops at its store with state.ErrClaimLost: it stores
// nothing and never reaches the model, so the turn runs once.
func TestRun_ClaimTakenOverStopsTheTurn(t *testing.T) {
	sessions := state.NewSessionStore("")
	if _, _, err := sessions.OpenSession(context.Background(), state.SessionParams{ID: "s1", EntityID: "u1", OpenedBy: "origin", OpenKey: "event-1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	llm := &promptRecordingLLM{fakeLLM: fakeLLM{responses: []string{"Hello."}}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	lost := time.Now()
	for _, at := range []time.Time{lost, lost.Add(2 * time.Minute)} {
		if result, err := sessions.ClaimOpener(context.Background(), "s1", at, time.Minute); err != nil || result != state.ClaimWon {
			t.Fatalf("ClaimOpener(%v) = %v, %v", at, result, err)
		}
	}

	ctx := WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), state.TurnClaim{At: lost})
	_, err := orch.Run(ctx, "s1", "[context] the setup created three samples")

	if !errors.Is(err, state.ErrClaimLost) {
		t.Errorf("Run = %v, want state.ErrClaimLost", err)
	}
	if sess, _ := sessions.Get("s1"); len(sess.Messages) != 0 {
		t.Errorf("stored messages = %+v, want none", sess.Messages)
	}
	if len(llm.prompts) != 0 {
		t.Errorf("model called with %q, want no call", llm.prompts)
	}
}

// A turn queued behind a long turn of the same session, whose deadline passed
// while it waited for the session, never starts and stores nothing: a keyed
// note's deadline bounds how late it may store, which keeps a stale reservation
// from being taken while the original can still store the same note.
func TestRun_QueuedTurnPastItsDeadlineStoresNothing(t *testing.T) {
	llm := newBlockingLLM("done")
	orch, sid, _ := newConcurrentOrch(t, 2, llm)

	long := make(chan error, 1)
	go func() {
		_, err := orch.Run(context.Background(), sid, "a long user turn")
		long <- err
	}()
	for atomic.LoadInt32(&llm.inflight) == 0 {
		time.Sleep(time.Millisecond)
	}

	const note = "[context] chapter two was caught up"
	ctx, cancel := context.WithTimeout(
		WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), state.TurnClaim{NoteKey: "chapter-2", At: time.Now()}), 200*time.Millisecond)
	defer cancel()
	queued := make(chan error, 1)
	go func() {
		_, err := orch.Run(ctx, sid, note)
		queued <- err
	}()
	<-ctx.Done()
	llm.releaseAll()

	if err := <-long; err != nil {
		t.Fatalf("long turn: %v", err)
	}
	if err := <-queued; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued turn returned %v, want its deadline", err)
	}
	sess, _ := orch.sessions.Get(sid)
	for _, m := range sess.Messages {
		if m.Content == note {
			t.Errorf("the queued turn stored its message after its deadline: %+v", m)
		}
	}
}

// claimedTurnFixture is a server-opened session s1 whose first-turn claim and
// chapter-2 reservation were both taken at now, and an orchestrator with the
// planner on whose model answers responses in order.
func claimedTurnFixture(t *testing.T, registry *ToolRegistry, opts OrchestratorOpts, responses ...string) (*Orchestrator, *state.SessionStore, time.Time) {
	t.Helper()
	sessions := state.NewSessionStore("")
	if _, _, err := sessions.OpenSession(context.Background(), state.SessionParams{ID: "s1", EntityID: "u1", OpenedBy: "origin", OpenKey: "event-1"}); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	now := time.Now()
	if result, err := sessions.ClaimOpener(context.Background(), "s1", now, time.Minute); err != nil || result != state.ClaimWon {
		t.Fatalf("ClaimOpener = %v, %v", result, err)
	}
	if result, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", now, time.Minute); err != nil || result != state.ClaimWon {
		t.Fatalf("ReserveNote = %v, %v", result, err)
	}
	opts.PipelineEnabled = true
	orch := NewWithRules(&fakeLLM{responses: responses}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, opts)
	return orch, sessions, now
}

// A claimed turn stores its input before the planner may run a tool or ask for
// a confirmation. A single write step that needs the user's approval ends the
// turn with the confirmation prompt; the input is stored once, before it, and
// the claim is settled, so a stale re-claim cannot run the turn and ask for the
// same write again.
func TestRun_ClaimedTurnStoredBeforeASingleStepConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim state.TurnClaim
	}{
		{"first turn", state.TurnClaim{}},
		{"keyed note", state.TurnClaim{NoteKey: "chapter-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewToolRegistry()
			_ = registry.Register(PluginCapability{Name: "gitlab", Actions: []Action{{Name: "analyze_code"}}}, &echoExecutor{})
			_ = registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})
			orch, sessions, now := claimedTurnFixture(t, registry,
				OrchestratorOpts{ConfirmationPlugin: "conf", ConfirmationAction: "check"},
				`{"type":"pipeline","steps":[{"id":"1","name":"Analyze","plugin":"gitlab","action":"analyze_code"}]}`,
				"Shall I analyze the repository?")
			claim := tc.claim
			claim.At = now
			const input = "[context] chapter two was caught up"

			result, err := orch.Run(WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), claim), "s1", input)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Metadata["type"] != "confirmation" {
				t.Fatalf("result = %+v, want the single step's confirmation", result)
			}
			sess, _ := sessions.Get("s1")
			if len(sess.Messages) != 2 || sess.Messages[0].Content != input || sess.Messages[1].Role != provider.RoleAssistant {
				t.Errorf("stored messages = %+v, want the input, then the confirmation prompt", sess.Messages)
			}
			later := now.Add(time.Hour)
			reclaim := func() (state.ClaimResult, error) {
				return sessions.ClaimOpener(context.Background(), "s1", later, time.Minute)
			}
			if claim.NoteKey != "" {
				reclaim = func() (state.ClaimResult, error) {
					return sessions.ReserveNote(context.Background(), "s1", claim.NoteKey, later, time.Minute)
				}
			}
			if again, err := reclaim(); err != nil || again != state.ClaimSettled {
				t.Errorf("stale claim = %v (%v), want settled: the turn ran", again, err)
			}
		})
	}
}

// A claimed turn whose claim was taken over stops before the planner's single
// step runs its tool.
func TestRun_ClaimTakenOverRunsNoPlannedTool(t *testing.T) {
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{Name: "gitlab", Actions: []Action{{Name: "analyze_code"}}}, exec)
	orch, sessions, now := claimedTurnFixture(t, registry, OrchestratorOpts{},
		`{"type":"pipeline","steps":[{"id":"1","name":"Analyze","plugin":"gitlab","action":"analyze_code"}]}`,
		"Analyzed.")
	if result, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", now.Add(2*time.Minute), time.Minute); err != nil || result != state.ClaimWon {
		t.Fatalf("re-claim = %v, %v", result, err)
	}

	ctx := WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), state.TurnClaim{NoteKey: "chapter-2", At: now})
	_, err := orch.Run(ctx, "s1", "[context] chapter two was caught up")

	if !errors.Is(err, state.ErrClaimLost) {
		t.Errorf("Run = %v, want state.ErrClaimLost", err)
	}
	if exec.count != 0 {
		t.Errorf("planned tool ran %d times, want 0", exec.count)
	}
}

// A claimed turn whose content is empty once prepared (a request holding only a
// [knowledge_context] block) ends before the planner, whether its claim is
// still held or was taken over: no planned tool runs and nothing is stored.
func TestRun_ClaimedTurnWithEmptyPreparedContentRunsNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		lost bool
	}{{"claim held", false}, {"claim taken over", true}} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &countingExecutor{}
			registry := NewToolRegistry()
			_ = registry.Register(PluginCapability{Name: "gitlab", Actions: []Action{{Name: "analyze_code"}}}, exec)
			orch, sessions, now := claimedTurnFixture(t, registry, OrchestratorOpts{},
				`{"type":"pipeline","steps":[{"id":"1","name":"Analyze","plugin":"gitlab","action":"analyze_code"}]}`,
				"Analyzed.")
			if tc.lost {
				if result, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", now.Add(2*time.Minute), time.Minute); err != nil || result != state.ClaimWon {
					t.Fatalf("re-claim = %v, %v", result, err)
				}
			}

			ctx := WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), state.TurnClaim{NoteKey: "chapter-2", At: now})
			result, err := orch.Run(ctx, "s1", "[knowledge_context]chapter two[/knowledge_context]")

			if err != nil || result.Metadata["error_code"] != "empty_content" {
				t.Errorf("Run = %+v, %v; want the empty-content answer", result, err)
			}
			if exec.count != 0 {
				t.Errorf("planned tool ran %d times, want 0", exec.count)
			}
			if sess, _ := sessions.Get("s1"); len(sess.Messages) != 0 {
				t.Errorf("stored messages = %+v, want none", sess.Messages)
			}
		})
	}
}

// invokingPreparer is a trusted content preparer that answers every message by
// invoking gitlab.analyze_code, whose runs exec counts.
func invokingPreparer(exec *countingExecutor) (*ToolRegistry, OrchestratorOpts) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{Name: "invoker", Actions: []Action{{Name: "prepare"}}},
		&fixedResultExecutor{content: `{"send_to_llm": false, "invoke": {"plugin": "gitlab", "action": "analyze_code"}}`})
	_ = registry.Register(PluginCapability{Name: "gitlab", Actions: []Action{{Name: "analyze_code"}}}, exec)
	return registry, OrchestratorOpts{ContentPreparers: []ContentPreparerEntry{{Plugin: "invoker", Action: "prepare"}}}
}

// A claimed turn a preparer answers with actions stores its input before the
// actions run, so the claim is settled and a stale re-claim cannot run them
// again.
func TestRun_ClaimedTurnStoredBeforeAPreparerInvokes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim state.TurnClaim
	}{
		{"first turn", state.TurnClaim{}},
		{"keyed note", state.TurnClaim{NoteKey: "chapter-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := &countingExecutor{}
			registry, opts := invokingPreparer(exec)
			orch, sessions, now := claimedTurnFixture(t, registry, opts)
			claim := tc.claim
			claim.At = now
			const input = "[context] chapter two was caught up"

			ctx := WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), claim)
			if _, err := orch.Run(ctx, "s1", input); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if exec.count != 1 {
				t.Errorf("invoked action ran %d times, want 1", exec.count)
			}
			sess, _ := sessions.Get("s1")
			if len(sess.Messages) != 1 || sess.Messages[0].Content != input {
				t.Errorf("stored messages = %+v, want the input", sess.Messages)
			}
			later := now.Add(time.Hour)
			reclaim := func() (state.ClaimResult, error) {
				return sessions.ClaimOpener(context.Background(), "s1", later, time.Minute)
			}
			if claim.NoteKey != "" {
				reclaim = func() (state.ClaimResult, error) {
					return sessions.ReserveNote(context.Background(), "s1", claim.NoteKey, later, time.Minute)
				}
			}
			if again, err := reclaim(); err != nil || again != state.ClaimSettled {
				t.Errorf("stale claim = %v (%v), want settled: the turn ran", again, err)
			}
		})
	}
}

// A claimed turn whose claim was taken over runs no action a preparer invokes.
func TestRun_ClaimTakenOverRunsNoPreparerInvoke(t *testing.T) {
	exec := &countingExecutor{}
	registry, opts := invokingPreparer(exec)
	orch, sessions, now := claimedTurnFixture(t, registry, opts)
	if result, err := sessions.ReserveNote(context.Background(), "s1", "chapter-2", now.Add(2*time.Minute), time.Minute); err != nil || result != state.ClaimWon {
		t.Fatalf("re-claim = %v, %v", result, err)
	}

	ctx := WithTurnClaim(actor.WithVisibility(context.Background(), provider.VisibilityHidden), state.TurnClaim{NoteKey: "chapter-2", At: now})
	_, err := orch.Run(ctx, "s1", "[context] chapter two was caught up")

	if !errors.Is(err, state.ErrClaimLost) {
		t.Errorf("Run = %v, want state.ErrClaimLost", err)
	}
	if exec.count != 0 {
		t.Errorf("invoked action ran %d times, want 0", exec.count)
	}
}
