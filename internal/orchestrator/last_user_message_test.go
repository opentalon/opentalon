package orchestrator

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	"github.com/opentalon/opentalon/pkg/plugin/contextargs"
)

// These tests pin when the id handed to plugins as last_user_message_id
// changes. The rule: only a turn that starts with a message the user wrote
// while nothing was waiting for their approval gets a new one. Answers to a
// confirmation prompt (button or typed), hidden system-injected turns and
// clicks on an expired prompt keep the stored value, and a call the user
// approves later carries the id of the message that led to the proposal.

const lumSession = "lum-session"

// lumLLM scripts the agent loop and the tool-less side calls separately.
// Agent-loop requests carry the native tools array; the confirmation
// narration and the reply classifier do not, so each request is routed by
// that and the two scripts cannot steal each other's replies.
type lumLLM struct {
	mu   sync.Mutex
	main []provider.CompletionResponse
	side []string
}

func (l *lumLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(req.Tools) == 0 {
		if len(l.side) == 0 {
			return &provider.CompletionResponse{Content: "Shall I go ahead?"}, nil
		}
		r := l.side[0]
		l.side = l.side[1:]
		return &provider.CompletionResponse{Content: r}, nil
	}
	if len(l.main) == 0 {
		return &provider.CompletionResponse{Content: "Here is the result."}, nil
	}
	r := l.main[0]
	l.main = l.main[1:]
	return &r, nil
}

func (l *lumLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// script replaces both queues before the next turn.
func (l *lumLLM) script(main []provider.CompletionResponse, side ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.main = main
	l.side = side
}

func lumToolCall(id, name string) provider.CompletionResponse {
	return provider.CompletionResponse{ToolCalls: []provider.ToolCall{{ID: id, Name: name, Arguments: map[string]string{}}}}
}

// newLUMOrchestrator wires a "store" plugin with a read-only "inspect" and a
// gated "purge" (the confirmation plugin always asks), both declaring the
// injected arg. Each call builds its own registry, so two orchestrators on one
// session store behave like two instances on one database.
func newLUMOrchestrator(llm LLMClient, exec PluginExecutor, sessions SessionStoreInterface, sink emit.Sink, classifier bool) *Orchestrator {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "store",
		Actions: []Action{
			native(Action{Name: "inspect", Description: "Show what would be removed", ReadOnly: true,
				Parameters:        []Parameter{{Name: "scope", Description: "Which records"}},
				InjectContextArgs: []string{contextargs.LastUserMessageID}}),
			native(Action{Name: "purge", Description: "Remove records",
				InjectContextArgs: []string{contextargs.LastUserMessageID}}),
		},
	}, exec)
	_ = registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})
	return NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:                     sink,
			ConfirmationPlugin:            "conf",
			ConfirmationAction:            "check",
			ConfirmationClassifierEnabled: classifier,
		})
}

type lumHarness struct {
	t        *testing.T
	llm      *lumLLM
	exec     *recordingExecutor
	sink     *recordingEventSink
	sessions *state.SessionStore
	orch     *Orchestrator
}

func newLUMHarness(t *testing.T, classifier bool) *lumHarness {
	t.Helper()
	h := &lumHarness{t: t, llm: &lumLLM{}, exec: &recordingExecutor{}, sink: &recordingEventSink{}, sessions: state.NewSessionStore("")}
	h.sessions.Create(state.SessionParams{ID: lumSession})
	h.orch = newLUMOrchestrator(h.llm, h.exec, h.sessions, h.sink, classifier)
	return h
}

func (h *lumHarness) run(ctx context.Context, msg string) *RunResult {
	h.t.Helper()
	res, err := h.orch.Run(ctx, lumSession, msg)
	if err != nil {
		h.t.Fatalf("Run(%q): %v", msg, err)
	}
	return res
}

func (h *lumHarness) stored() string {
	h.t.Helper()
	return storedLastUserMessageID(h.t, h.sessions, lumSession)
}

// lastUserMessageEventID is the id of the most recent user_message event.
func (h *lumHarness) lastUserMessageEventID() string {
	h.t.Helper()
	id := ""
	for _, e := range h.sink.snapshot() {
		if e.EventType == events.TypeUserMessage {
			id = e.ID
		}
	}
	if id == "" {
		h.t.Fatal("no user_message event recorded")
	}
	return id
}

// idsFor returns the injected id of every executed call to action, in order.
func (h *lumHarness) idsFor(action string) []string {
	var out []string
	for _, c := range h.exec.snapshot() {
		if c.Action == action {
			out = append(out, c.Args[contextargs.LastUserMessageID])
		}
	}
	return out
}

func storedLastUserMessageID(t *testing.T, sessions SessionStoreInterface, sid string) string {
	t.Helper()
	sess, err := sessions.Get(sid)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.Metadata[lastUserMessageIDMetaKey]
}

// proposeAfterInspect runs a typed turn in which the model first runs the
// read-only inspect and then proposes purge, which pauses for confirmation.
// Returns the id the typed turn got.
func (h *lumHarness) proposeAfterInspect(msg string) string {
	h.t.Helper()
	h.llm.script([]provider.CompletionResponse{
		lumToolCall("c-inspect", "store__inspect"),
		lumToolCall("c-purge", "store__purge"),
	}, "Shall I remove the 3 records?")
	res := h.run(context.Background(), msg)
	if res.Metadata["type"] != "confirmation" {
		h.t.Fatalf("turn %q did not pause for confirmation: %+v", msg, res)
	}
	if pc, _, _ := loadPendingToolCall(h.sessions, lumSession); pc == nil || pc.Action != "purge" {
		h.t.Fatalf("purge is not the pending call after %q: %+v", msg, pc)
	}
	return h.lastUserMessageEventID()
}

func TestDefaultContextArgProviders_LastUserMessageID(t *testing.T) {
	provide := defaultContextArgProviders(nil, nil)[contextargs.LastUserMessageID]
	if provide == nil {
		t.Fatal("last_user_message_id provider not registered")
	}
	if got := provide(context.Background(), contextargs.LastUserMessageID); got != "" {
		t.Errorf("empty ctx = %q, want empty (a scheduled job or webhook has no chat turn)", got)
	}
	ctx := actor.WithLastUserMessageID(context.Background(), "m-1")
	if got := provide(ctx, contextargs.LastUserMessageID); got != "m-1" {
		t.Errorf("provider = %q, want m-1", got)
	}
}

// A typed turn gets a new id — the turn's user_message event id — and every
// tool call of that turn carries it; the next typed turn gets another one.
func TestLastUserMessageID_TypedTurnGetsNewID(t *testing.T) {
	h := newLUMHarness(t, false)

	second := lumToolCall("c2", "store__inspect")
	second.ToolCalls[0].Arguments = map[string]string{"scope": "old"}
	h.llm.script([]provider.CompletionResponse{lumToolCall("c1", "store__inspect"), second})
	h.run(context.Background(), "what would be removed?")
	first := h.lastUserMessageEventID()
	if got := h.stored(); got != first {
		t.Fatalf("stored id = %q, want the turn's user_message event id %q", got, first)
	}
	if got := h.idsFor("inspect"); len(got) != 2 || got[0] != first || got[1] != first {
		t.Fatalf("inspect ids = %v, want both calls of the turn to carry %q", got, first)
	}

	h.llm.script([]provider.CompletionResponse{lumToolCall("c3", "store__inspect")})
	h.run(context.Background(), "and now?")
	next := h.lastUserMessageEventID()
	if next == first {
		t.Fatal("the second typed turn emitted the same user_message id")
	}
	if got := h.stored(); got != next {
		t.Errorf("stored id = %q, want the second turn's %q", got, next)
	}
	if got := h.idsFor("inspect"); len(got) != 3 || got[2] != next {
		t.Errorf("inspect ids = %v, want the third call to carry %q", got, next)
	}
}

// Without an event sink there is no user_message event id; the new id is then
// a random 32-character hex string, still different on every typed turn.
func TestLastUserMessageID_TypedTurnWithoutEventSinkUsesRandomID(t *testing.T) {
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: lumSession})
	orch := newLUMOrchestrator(llm, exec, sessions, nil, false)

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	var seen []string
	for _, msg := range []string{"first", "second"} {
		if _, err := orch.Run(context.Background(), lumSession, msg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		id := storedLastUserMessageID(t, sessions, lumSession)
		if !hex32.MatchString(id) {
			t.Fatalf("stored id = %q, want 32 hex characters", id)
		}
		seen = append(seen, id)
	}
	if seen[0] == seen[1] {
		t.Errorf("two typed turns kept the same id %q", seen[0])
	}
}

// The case the id exists for: the user writes once (id A), the model runs a
// first call and proposes a second in the same turn, and the user clicks
// Approve. The approved call must still carry A — the click is not a new
// message.
func TestLastUserMessageID_ApproveClickCarriesProposingTurnID(t *testing.T) {
	h := newLUMHarness(t, false)
	a := h.proposeAfterInspect("clean up the old records")

	h.llm.script([]provider.CompletionResponse{{Content: "Removed 3 records."}})
	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")

	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Fatalf("approved purge ids = %v, want [%s] (the proposing turn's id)", got, a)
	}
	if got := h.idsFor("inspect"); len(got) != 1 || got[0] != a {
		t.Errorf("inspect ids = %v, want [%s]", got, a)
	}
	if got := h.stored(); got != a {
		t.Errorf("stored id after the click = %q, want it unchanged at %q", got, a)
	}
}

// The good case: after the first call the model answers, the user writes a
// reply with nothing pending (id B), and the call proposed then carries B.
func TestLastUserMessageID_ReplyWithNothingPendingGivesApprovedCallNewID(t *testing.T) {
	h := newLUMHarness(t, false)

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-inspect", "store__inspect"), {Content: "3 records would go."}})
	h.run(context.Background(), "what would be removed?")
	a := h.lastUserMessageEventID()

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-purge", "store__purge")}, "Shall I remove the 3 records?")
	h.run(context.Background(), "ok, remove them")
	b := h.lastUserMessageEventID()
	if a == b {
		t.Fatal("the reply did not get its own user_message id")
	}

	h.llm.script([]provider.CompletionResponse{{Content: "Removed."}})
	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")

	if got := h.idsFor("inspect"); len(got) != 1 || got[0] != a {
		t.Errorf("inspect ids = %v, want [%s]", got, a)
	}
	if got := h.idsFor("purge"); len(got) != 1 || got[0] != b {
		t.Errorf("approved purge ids = %v, want [%s] (the reply's id)", got, b)
	}
}

// A typed "yes" the classifier reads as approve is an answer to the prompt,
// not a new message: the approved call carries the proposing turn's id.
func TestLastUserMessageID_TypedApproveCarriesProposingTurnID(t *testing.T) {
	h := newLUMHarness(t, true)
	a := h.proposeAfterInspect("clean up the old records")

	h.llm.script([]provider.CompletionResponse{{Content: "Removed 3 records."}},
		`{"decision":"approve","requests_change":false}`)
	h.run(context.Background(), "ja")

	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Fatalf("purge ids = %v, want [%s] (typed approval must not change the id)", got, a)
	}
	if got := h.stored(); got != a {
		t.Errorf("stored id = %q, want it unchanged at %q", got, a)
	}
}

// A correction to the prompt keeps the id: the re-proposed call, approved
// afterwards, still carries the id of the message that started it all.
func TestLastUserMessageID_AmendKeepsID(t *testing.T) {
	h := newLUMHarness(t, true)
	a := h.proposeAfterInspect("clean up the old records")

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-purge-2", "store__purge")},
		`{"decision":"amend","requests_change":true,"reason":"only the two oldest"}`,
		"Shall I remove the 2 oldest records?")
	res := h.run(context.Background(), "only the two oldest")
	if res.Metadata["type"] != "confirmation" {
		t.Fatalf("the amended turn did not re-propose: %+v", res)
	}
	if got := h.stored(); got != a {
		t.Fatalf("stored id after the correction = %q, want it unchanged at %q", got, a)
	}

	h.llm.script([]provider.CompletionResponse{{Content: "Removed 2 records."}})
	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")
	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Errorf("purge ids = %v, want [%s]", got, a)
	}
}

// A reject keeps the id; only the next typed message changes it.
func TestLastUserMessageID_RejectKeepsIDUntilNextTypedMessage(t *testing.T) {
	h := newLUMHarness(t, false)
	a := h.proposeAfterInspect("clean up the old records")

	res := h.run(actor.WithConfirmationDecision(context.Background(), "reject"), "reject")
	if res.Metadata["action"] != "confirmation_rejected" {
		t.Fatalf("want a rejected frame, got %+v", res)
	}
	if got := h.stored(); got != a {
		t.Fatalf("stored id after reject = %q, want it unchanged at %q", got, a)
	}
	if got := h.idsFor("purge"); len(got) != 0 {
		t.Fatalf("rejected purge ran: %v", got)
	}

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-inspect-2", "store__inspect")})
	h.run(context.Background(), "show me again")
	b := h.lastUserMessageEventID()
	if b == a || h.stored() != b {
		t.Fatalf("stored id after the next typed message = %q, want the new %q (old %q)", h.stored(), b, a)
	}
	if got := h.idsFor("inspect"); len(got) != 2 || got[1] != b {
		t.Errorf("inspect ids = %v, want the second call to carry %q", got, b)
	}
}

// A hidden (system-injected) turn is not the user's message: it keeps the id,
// and a tool call it makes carries the stored one.
func TestLastUserMessageID_HiddenTurnKeepsID(t *testing.T) {
	h := newLUMHarness(t, false)
	h.run(context.Background(), "hello")
	a := h.lastUserMessageEventID()

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-inspect", "store__inspect")})
	h.run(actor.WithVisibility(context.Background(), provider.VisibilityHidden), "[system] Your background job finished.")

	if got := h.stored(); got != a {
		t.Errorf("stored id after a hidden turn = %q, want it unchanged at %q", got, a)
	}
	if got := h.idsFor("inspect"); len(got) != 1 || got[0] != a {
		t.Errorf("inspect ids in the hidden turn = %v, want [%s]", got, a)
	}
}

// A hidden turn while a call is pending leaves both the pending call and the
// id alone; the later click still runs the call with the proposing turn's id.
func TestLastUserMessageID_HiddenTurnWhilePendingKeepsID(t *testing.T) {
	h := newLUMHarness(t, false)
	a := h.proposeAfterInspect("clean up the old records")

	h.run(actor.WithVisibility(context.Background(), provider.VisibilityHidden), "[system] Your background job finished.")
	if got := h.stored(); got != a {
		t.Fatalf("stored id after a hidden turn = %q, want %q", got, a)
	}

	h.llm.script([]provider.CompletionResponse{{Content: "Removed."}})
	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")
	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Errorf("purge ids = %v, want [%s]", got, a)
	}
}

// A click on a prompt that is no longer active changes nothing.
func TestLastUserMessageID_ExpiredClickKeepsID(t *testing.T) {
	h := newLUMHarness(t, false)
	h.run(context.Background(), "hello")
	a := h.lastUserMessageEventID()

	res := h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")
	if res.Metadata["action"] != "confirmation_expired" {
		t.Fatalf("want an expired frame, got %+v", res)
	}
	if got := h.stored(); got != a {
		t.Errorf("stored id after an expired click = %q, want it unchanged at %q", got, a)
	}
}

// An approved pipeline runs its steps with the stored id, and a value the
// planner wrote under the reserved name is replaced by the host's.
func TestLastUserMessageID_PipelineApproveCarriesStoredID(t *testing.T) {
	h := newLUMHarness(t, false)
	h.run(context.Background(), "clean up the old records")
	a := h.lastUserMessageEventID()

	plan := pipeline.NewPipeline([]*pipeline.Step{{
		ID: "1", Name: "purge", MaxRetries: -1,
		Command: &pipeline.PluginCommand{Plugin: "store", Action: "purge",
			Args: map[string]any{contextargs.LastUserMessageID: "written-by-planner"}},
	}}, pipeline.DefaultConfig())
	h.orch.pendingMu.Lock()
	h.orch.pendingPipelines[lumSession] = pendingPipeline{plan: plan}
	h.orch.pendingMu.Unlock()

	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")

	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Fatalf("pipeline purge ids = %v, want [%s]", got, a)
	}
	if got := h.stored(); got != a {
		t.Errorf("stored id after the pipeline approval = %q, want %q", got, a)
	}
}

// Two instances on one session store: the approval lands on the other
// instance and still runs the call with the proposing turn's id.
func TestLastUserMessageID_SecondInstanceReadsSameID(t *testing.T) {
	h := newLUMHarness(t, false)
	a := h.proposeAfterInspect("clean up the old records")

	otherLLM := &lumLLM{}
	otherLLM.script([]provider.CompletionResponse{{Content: "Removed 3 records."}})
	other := newLUMOrchestrator(otherLLM, h.exec, h.sessions, h.sink, false)
	if _, err := other.Run(actor.WithConfirmationDecision(context.Background(), "approve"), lumSession, "approve"); err != nil {
		t.Fatalf("Run on the second instance: %v", err)
	}

	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Fatalf("purge ids = %v, want [%s] from the shared store", got, a)
	}
}

// metaCountingStore counts writes of the id's metadata key, returns detached
// copies from Get like a database-backed store, and can make writes fail in
// three ways: before anything is written (fail), after the write has
// landed (failAfterWrite: a commit that applied but did not report success),
// and with the read that follows a failed write failing too (failReadBack).
type metaCountingStore struct {
	*state.SessionStore
	mu             sync.Mutex
	writes         int
	fail           bool
	failAfterWrite bool
	failReadBack   bool
	failNextGet    bool
}

func (s *metaCountingStore) SetMetadata(id, key, value string) error {
	if key != lastUserMessageIDMetaKey {
		return s.SessionStore.SetMetadata(id, key, value)
	}
	s.mu.Lock()
	s.writes++
	fail, failAfterWrite := s.fail, s.failAfterWrite
	if fail || failAfterWrite {
		s.failNextGet = s.failReadBack
	}
	s.mu.Unlock()
	if fail {
		return errors.New("metadata write failed")
	}
	if err := s.SessionStore.SetMetadata(id, key, value); err != nil {
		return err
	}
	if failAfterWrite {
		return errors.New("commit outcome unknown")
	}
	return nil
}

func (s *metaCountingStore) Get(id string) (*state.Session, error) {
	s.mu.Lock()
	failGet := s.failNextGet
	s.failNextGet = false
	s.mu.Unlock()
	if failGet {
		return nil, errors.New("store unreachable")
	}
	live, err := s.SessionStore.Get(id)
	if err != nil {
		return nil, err
	}
	// Hand out a detached copy, as a database-backed store does. The
	// in-memory store returns its live session, whose metadata map the
	// turn's cached copy would then share — and a read that wrongly went
	// through that cache would still see the latest write.
	cp := *live
	cp.Metadata = make(map[string]string, len(live.Metadata))
	for k, v := range live.Metadata {
		cp.Metadata[k] = v
	}
	return &cp, nil
}

func (s *metaCountingStore) set(fn func(*metaCountingStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *metaCountingStore) setFail(v bool) { s.set(func(s *metaCountingStore) { s.fail = v }) }

func (s *metaCountingStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// A session without an id gets exactly one on its first turn that is not a
// new message, and later such turns reuse it instead of minting more.
func TestLastUserMessageID_SessionWithoutIDGetsExactlyOne(t *testing.T) {
	store := &metaCountingStore{SessionStore: state.NewSessionStore("")}
	store.Create(state.SessionParams{ID: lumSession})
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	orch := newLUMOrchestrator(llm, exec, store, &recordingEventSink{}, false)
	hidden := actor.WithVisibility(context.Background(), provider.VisibilityHidden)

	llm.script([]provider.CompletionResponse{lumToolCall("c1", "store__inspect")})
	if _, err := orch.Run(hidden, lumSession, "[system] note one"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	id := storedLastUserMessageID(t, store, lumSession)
	if id == "" {
		t.Fatal("the session still has no id after its first turn")
	}
	if n := store.writeCount(); n != 1 {
		t.Fatalf("id writes = %d, want exactly 1", n)
	}

	llm.script([]provider.CompletionResponse{lumToolCall("c2", "store__inspect")})
	if _, err := orch.Run(hidden, lumSession, "[system] note two"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := storedLastUserMessageID(t, store, lumSession); got != id {
		t.Errorf("stored id = %q, want it kept at %q", got, id)
	}
	if n := store.writeCount(); n != 1 {
		t.Errorf("id writes = %d after a second hidden turn, want still 1", n)
	}
	for i, c := range exec.snapshot() {
		if got := c.Args[contextargs.LastUserMessageID]; got != id {
			t.Errorf("call %d carried %q, want %q", i, got, id)
		}
	}
}

// When the new id cannot be saved the turn keeps the old one: a plugin that
// waits for the user to write again keeps waiting rather than seeing an id the
// next turn would not repeat.
func TestLastUserMessageID_FailedSaveKeepsOldID(t *testing.T) {
	store := &metaCountingStore{SessionStore: state.NewSessionStore("")}
	store.Create(state.SessionParams{ID: lumSession})
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	sink := &recordingEventSink{}
	orch := newLUMOrchestrator(llm, exec, store, sink, false)

	if _, err := orch.Run(context.Background(), lumSession, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	a := storedLastUserMessageID(t, store, lumSession)
	if a == "" {
		t.Fatal("no id after the first typed turn")
	}

	store.setFail(true)
	llm.script([]provider.CompletionResponse{lumToolCall("c1", "store__inspect")})
	if _, err := orch.Run(context.Background(), lumSession, "and now?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := storedLastUserMessageID(t, store, lumSession); got != a {
		t.Errorf("stored id = %q, want the old %q", got, a)
	}
	calls := exec.snapshot()
	if len(calls) != 1 || calls[0].Args[contextargs.LastUserMessageID] != a {
		t.Errorf("calls = %+v, want one call carrying the old id %q", calls, a)
	}
}

// An id that could not be saved at all is never handed out: the plugin sees
// the argument absent (and fails closed) — neither an id no later turn would
// repeat, nor one the incoming context happened to carry.
func TestLastUserMessageID_UnsavedFirstIDIsNeverInjected(t *testing.T) {
	store := &metaCountingStore{SessionStore: state.NewSessionStore("")}
	store.Create(state.SessionParams{ID: lumSession})
	store.setFail(true)
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	orch := newLUMOrchestrator(llm, exec, store, &recordingEventSink{}, false)

	llm.script([]provider.CompletionResponse{lumToolCall("c1", "store__inspect")})
	ctx := actor.WithLastUserMessageID(context.Background(), "inherited-from-elsewhere")
	if _, err := orch.Run(ctx, lumSession, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := exec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if got, present := calls[0].Args[contextargs.LastUserMessageID]; present {
		t.Errorf("an id reached the plugin although none could be saved: %q", got)
	}
}

// A save can apply and still report an error. The turn must then use what
// the store holds, not the old id: otherwise the first step of this turn
// would carry the old id while the approval turn reads the new one, and a
// plugin would take the difference for a new message from the user.
func TestLastUserMessageID_WriteThatLandedDespiteErrorIsUsed(t *testing.T) {
	store := &metaCountingStore{SessionStore: state.NewSessionStore("")}
	store.Create(state.SessionParams{ID: lumSession})
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	sink := &recordingEventSink{}
	orch := newLUMOrchestrator(llm, exec, store, sink, false)

	if _, err := orch.Run(context.Background(), lumSession, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	a := storedLastUserMessageID(t, store, lumSession)

	store.set(func(s *metaCountingStore) { s.failAfterWrite = true })
	llm.script([]provider.CompletionResponse{
		lumToolCall("c-inspect", "store__inspect"),
		lumToolCall("c-purge", "store__purge"),
	}, "Shall I remove the 3 records?")
	if _, err := orch.Run(context.Background(), lumSession, "clean up the old records"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	b := storedLastUserMessageID(t, store, lumSession)
	if b == a {
		t.Fatalf("the write did not land in the test store (still %q)", a)
	}

	store.set(func(s *metaCountingStore) { s.failAfterWrite = false })
	llm.script([]provider.CompletionResponse{{Content: "Removed."}})
	if _, err := orch.Run(actor.WithConfirmationDecision(context.Background(), "approve"), lumSession, "approve"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var inspectID, purgeID string
	for _, c := range exec.snapshot() {
		switch c.Action {
		case "inspect":
			inspectID = c.Args[contextargs.LastUserMessageID]
		case "purge":
			purgeID = c.Args[contextargs.LastUserMessageID]
		}
	}
	if inspectID != b || purgeID != b {
		t.Errorf("inspect id = %q, purge id = %q, want both to carry the stored %q", inspectID, purgeID, b)
	}
}

// When a save reports an error and the store cannot be read back either, the
// turn withholds the argument instead of guessing.
func TestLastUserMessageID_UnverifiableSaveWithholdsID(t *testing.T) {
	store := &metaCountingStore{SessionStore: state.NewSessionStore("")}
	store.Create(state.SessionParams{ID: lumSession})
	llm := &lumLLM{}
	exec := &recordingExecutor{}
	orch := newLUMOrchestrator(llm, exec, store, &recordingEventSink{}, false)

	if _, err := orch.Run(context.Background(), lumSession, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store.set(func(s *metaCountingStore) { s.failAfterWrite, s.failReadBack = true, true })
	llm.script([]provider.CompletionResponse{lumToolCall("c1", "store__inspect")})
	if _, err := orch.Run(context.Background(), lumSession, "and now?"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := exec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if got, present := calls[0].Args[contextargs.LastUserMessageID]; present {
		t.Errorf("an unverified id reached the plugin: %q", got)
	}
}

// A backend-opened (system) run is not the user's writing even when it is
// visible: it keeps the id, and a call it makes carries the stored one.
func TestLastUserMessageID_VisibleSystemRunKeepsID(t *testing.T) {
	h := newLUMHarness(t, false)
	h.run(context.Background(), "hello")
	a := h.lastUserMessageEventID()

	h.llm.script([]provider.CompletionResponse{lumToolCall("c-inspect", "store__inspect")})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "u1", Kind: profile.KindSystem, SystemSource: "job_note"})
	h.run(ctx, "Your export is ready.")

	if got := h.stored(); got != a {
		t.Errorf("stored id after a visible system run = %q, want it unchanged at %q", got, a)
	}
	if got := h.idsFor("inspect"); len(got) != 1 || got[0] != a {
		t.Errorf("inspect ids in the system run = %v, want [%s]", got, a)
	}

	// A chat-kind profile is a person's turn and does get a new id.
	h.run(profile.WithProfile(context.Background(), &profile.Profile{EntityID: "u1", Kind: profile.KindChat}), "thanks")
	if got := h.stored(); got == a || got != h.lastUserMessageEventID() {
		t.Errorf("stored id after a chat turn = %q, want the new user_message id (old %q)", got, a)
	}
}

// The value is host-owned. A model that sends the name fails argument
// validation and never reaches the plugin; a host-built call carrying it gets
// the host's value, or loses the key when there is none; and even an action
// that (wrongly) also lists it as a parameter receives the host's value.
func TestExecuteCall_LastUserMessageIDIsHostOwned(t *testing.T) {
	var captured []ToolCall
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "probe",
		Actions: []Action{
			{Name: "run", Parameters: []Parameter{{Name: "text"}},
				InjectContextArgs: []string{contextargs.LastUserMessageID}},
			{Name: "declared", Parameters: []Parameter{{Name: "text"}, {Name: contextargs.LastUserMessageID}},
				InjectContextArgs: []string{contextargs.LastUserMessageID}},
		},
	}, &capturingExecutor{fn: func(call ToolCall) ToolResult {
		captured = append(captured, call)
		return ToolResult{CallID: call.ID, Content: "ok"}
	}})
	orch := New(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), state.NewSessionStore(""))
	forged := func() map[string]string {
		return map[string]string{"text": "x", contextargs.LastUserMessageID: "forged"}
	}
	withID := actor.WithLastUserMessageID(context.Background(), "host-id")

	t.Run("model call is refused", func(t *testing.T) {
		captured = nil
		res := orch.executeCall(withID, ToolCall{ID: "c1", Plugin: "probe", Action: "run", Args: forged(), FromLLM: true})
		if !res.ArgsInvalid || len(captured) != 0 {
			t.Fatalf("want an argument-validation refusal before the plugin, got %+v (plugin calls: %d)", res, len(captured))
		}
	})
	t.Run("host-built call gets the host value", func(t *testing.T) {
		captured = nil
		orch.executeCall(withID, ToolCall{ID: "c2", Plugin: "probe", Action: "run", Args: forged()})
		if len(captured) != 1 || captured[0].Args[contextargs.LastUserMessageID] != "host-id" || captured[0].Args["text"] != "x" {
			t.Fatalf("captured = %+v, want host-id and the caller's own text", captured)
		}
	})
	t.Run("host-built call without a host value loses the key", func(t *testing.T) {
		captured = nil
		orch.executeCall(context.Background(), ToolCall{ID: "c3", Plugin: "probe", Action: "run", Args: forged()})
		if len(captured) != 1 {
			t.Fatalf("plugin calls = %d, want 1", len(captured))
		}
		if got, present := captured[0].Args[contextargs.LastUserMessageID]; present {
			t.Errorf("key present as %q, want it removed", got)
		}
	})
	t.Run("declared as a parameter, the model value is still replaced", func(t *testing.T) {
		captured = nil
		orch.executeCall(withID, ToolCall{ID: "c4", Plugin: "probe", Action: "declared", Args: forged(), FromLLM: true})
		if len(captured) != 1 || captured[0].Args[contextargs.LastUserMessageID] != "host-id" {
			t.Fatalf("captured = %+v, want host-id", captured)
		}
	})
}

// The id lives in session metadata, which summarisation and clear_session
// leave alone: after both, an approved call still carries the stored id.
func TestLastUserMessageID_SurvivesSetSummaryAndClearMessages(t *testing.T) {
	h := newLUMHarness(t, false)
	a := h.proposeAfterInspect("clean up the old records")

	sess, _ := h.sessions.Get(lumSession)
	if err := h.sessions.SetSummary(lumSession, "summary", sess.Messages[len(sess.Messages)-1:]); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	if got := h.stored(); got != a {
		t.Fatalf("stored id after SetSummary = %q, want %q", got, a)
	}

	h.llm.script([]provider.CompletionResponse{{Content: "Removed."}})
	h.run(actor.WithConfirmationDecision(context.Background(), "approve"), "approve")
	if got := h.idsFor("purge"); len(got) != 1 || got[0] != a {
		t.Fatalf("purge ids = %v, want [%s]", got, a)
	}

	if err := h.sessions.ClearMessages(lumSession); err != nil {
		t.Fatalf("ClearMessages: %v", err)
	}
	if got := h.stored(); got != a {
		t.Errorf("stored id after ClearMessages = %q, want %q", got, a)
	}
}
