package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// titlePushRecorder captures every ChannelSender invocation. Used to
// verify both the live-push payload shape and the no-double-call
// guarantee under repeated maybeGenerateTitle invocations.
type titlePushRecorder struct {
	mu    sync.Mutex
	calls []titlePushCall
}

type titlePushCall struct {
	SessionID string
	Msg       pkgchannel.OutboundMessage
}

func (r *titlePushRecorder) push(_ context.Context, sessionID string, msg pkgchannel.OutboundMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, titlePushCall{SessionID: sessionID, Msg: msg})
	return nil
}

func (r *titlePushRecorder) snapshot() []titlePushCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]titlePushCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// titleTestOrch is a small builder that returns an orchestrator with the
// title-generation surface wired and the rest of the dependencies set to
// in-memory test doubles. Returns the orchestrator, the session id seeded
// with the given messages, the recorded event sink, the LLM stub, and the
// channel-push recorder so each test can assert on the slice it needs.
func titleTestOrch(t *testing.T, sessionMessages []provider.Message, llmResponses []string) (
	*Orchestrator, string, *recordingEventSink, *fakeLLM, *titlePushRecorder,
) {
	t.Helper()
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	for _, m := range sessionMessages {
		_ = sessions.AddMessage("sess", m)
	}
	llm := &fakeLLM{responses: llmResponses}
	push := &titlePushRecorder{}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:            sink,
		ChannelSender:        push.push,
		SessionTitlesEnabled: true,
	})
	return orch, "sess", sink, llm, push
}

func TestMaybeGenerateTitle_FirstTurn_EmitsInvokedAndGenerated(t *testing.T) {
	orch, sid, sink, llm, _ := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "How do I reset my password?"},
			{Role: provider.RoleAssistant, Content: "Click the reset link in your account settings."},
		},
		[]string{"Reset forgotten password"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)

	if llm.callCount != 1 {
		t.Fatalf("LLM call count = %d, want 1", llm.callCount)
	}
	evs := sink.snapshot()
	inv := findPayloadsByType(t, evs, events.TypeSessionTitleInvoked)
	gen := findPayloadsByType(t, evs, events.TypeSessionTitleGenerated)
	if len(inv) != 1 {
		t.Fatalf("session_title_invoked count = %d, want 1", len(inv))
	}
	if len(gen) != 1 {
		t.Fatalf("session_title_generated count = %d, want 1", len(gen))
	}
	var gp events.SessionTitleGeneratedPayload
	if err := json.Unmarshal(gen[0], &gp); err != nil {
		t.Fatalf("unmarshal generated payload: %v", err)
	}
	if gp.Title != "Reset forgotten password" {
		t.Errorf("Title = %q, want %q", gp.Title, "Reset forgotten password")
	}
	if gp.LatencyMS < 0 {
		t.Errorf("LatencyMS = %d, want >= 0", gp.LatencyMS)
	}
}

func TestMaybeGenerateTitle_NestsGeneratedUnderInvoked(t *testing.T) {
	// Asserts the parent-id chain: session_title_generated and any
	// provider-emitted llm_response (none from our fakeLLM, but the
	// payload-side wiring is the same) must carry ParentID = invokedID
	// so consumers can reconstruct the title span without scanning by
	// time-window.
	orch, sid, sink, _, _ := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "What's the inventory turnover?"},
			{Role: provider.RoleAssistant, Content: "Currently 4.2 per quarter."},
		},
		[]string{"Inventory turnover question"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)

	evs := sink.snapshot()
	var invokedID, generatedParent string
	for _, e := range evs {
		switch e.EventType {
		case events.TypeSessionTitleInvoked:
			invokedID = e.ID
		case events.TypeSessionTitleGenerated:
			generatedParent = e.ParentID
		}
	}
	if invokedID == "" {
		t.Fatal("session_title_invoked event_id is empty")
	}
	if generatedParent != invokedID {
		t.Errorf("generated.ParentID = %q, want %q (invoked event_id)",
			generatedParent, invokedID)
	}
}

func TestMaybeGenerateTitle_NoAssistantMessage_NoOp(t *testing.T) {
	// Goroutine spawned by Run could race ahead of the assistant message
	// being committed; the gate inside maybeGenerateTitle must skip in
	// that case so we don't generate a title from just the user message.
	orch, sid, sink, llm, _ := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "First question"},
		},
		[]string{"Should not be called"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)

	if llm.callCount != 0 {
		t.Errorf("LLM called %d times, want 0 (no assistant message gate)", llm.callCount)
	}
	if n := len(findPayloadsByType(t, sink.snapshot(), events.TypeSessionTitleInvoked)); n != 0 {
		t.Errorf("session_title_invoked fired %d times, want 0", n)
	}
}

func TestMaybeGenerateTitle_TitleAlreadySet_Idempotent(t *testing.T) {
	// A second maybeGenerateTitle on a session that already has a title
	// must short-circuit at the gate — no LLM call, no events, no push.
	orch, sid, sink, llm, push := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "Generate a title please"},
			{Role: provider.RoleAssistant, Content: "Done."},
		},
		[]string{"Title generation example", "Should not be called again"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)
	orch.maybeGenerateTitle(context.Background(), sid)

	if llm.callCount != 1 {
		t.Errorf("LLM call count = %d, want 1 (second call must short-circuit)", llm.callCount)
	}
	if n := len(findPayloadsByType(t, sink.snapshot(), events.TypeSessionTitleGenerated)); n != 1 {
		t.Errorf("session_title_generated count = %d, want 1", n)
	}
	if n := len(push.snapshot()); n != 1 {
		t.Errorf("channel push count = %d, want 1", n)
	}
}

func TestMaybeGenerateTitle_PersistsToSessionStore(t *testing.T) {
	orch, sid, _, _, _ := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "Hello"},
			{Role: provider.RoleAssistant, Content: "Hi."},
		},
		[]string{"Greeting exchange"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)

	sess, err := orch.sessions.Get(sid)
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if sess.Title != "Greeting exchange" {
		t.Errorf("session.Title = %q, want %q", sess.Title, "Greeting exchange")
	}
}

func TestMaybeGenerateTitle_PushesSessionTitleFrame(t *testing.T) {
	orch, sid, _, _, push := titleTestOrch(t,
		[]provider.Message{
			{Role: provider.RoleUser, Content: "What's GDPR retention?"},
			{Role: provider.RoleAssistant, Content: "Depends on the data class — typically 6 years for invoices."},
		},
		[]string{"GDPR retention question"},
	)

	orch.maybeGenerateTitle(context.Background(), sid)

	calls := push.snapshot()
	if len(calls) != 1 {
		t.Fatalf("channel push count = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.SessionID != sid {
		t.Errorf("push.SessionID = %q, want %q", c.SessionID, sid)
	}
	if c.Msg.Metadata["type"] != "session.title" {
		t.Errorf("push.Metadata[type] = %q, want \"session.title\"", c.Msg.Metadata["type"])
	}
	if c.Msg.Metadata["title"] != "GDPR retention question" {
		t.Errorf("push.Metadata[title] = %q", c.Msg.Metadata["title"])
	}
	if c.Msg.ConversationID != "" {
		t.Errorf("push.ConversationID = %q, want empty (adapter derives it from sessionID)", c.Msg.ConversationID)
	}
}

func TestMaybeGenerateTitle_NoChannelSender_StillPersists(t *testing.T) {
	// ChannelSender nil is supported (CLI mode, channels not registered).
	// Title must still persist and the event still emit.
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleUser, Content: "Hi"})
	_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleAssistant, Content: "Hello."})
	llm := &fakeLLM{responses: []string{"Brief greeting"}}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:            sink,
		SessionTitlesEnabled: true,
		// ChannelSender intentionally nil.
	})

	orch.maybeGenerateTitle(context.Background(), "sess")

	sess, _ := sessions.Get("sess")
	if sess.Title != "Brief greeting" {
		t.Errorf("Title = %q, want %q", sess.Title, "Brief greeting")
	}
	if n := len(findPayloadsByType(t, sink.snapshot(), events.TypeSessionTitleGenerated)); n != 1 {
		t.Errorf("session_title_generated count = %d, want 1", n)
	}
}

func TestMaybeGenerateTitle_LLMError_NoTitlePersisted(t *testing.T) {
	// LLM failure: invoked fires, generated does not, nothing persists.
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleUser, Content: "Q"})
	_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleAssistant, Content: "A"})
	orch := NewWithRules(&erroringLLM{err: errors.New("upstream timeout")},
		DefaultParser, NewToolRegistry(), memory, sessions, OrchestratorOpts{
			EventSink:            sink,
			SessionTitlesEnabled: true,
		})
	// Re-register memory + sessions with the actual orch we just built
	// to avoid recreating; the above already wired them.
	_ = registry

	orch.maybeGenerateTitle(context.Background(), "sess")

	sess, _ := sessions.Get("sess")
	if sess.Title != "" {
		t.Errorf("Title = %q, want empty after LLM error", sess.Title)
	}
	evs := sink.snapshot()
	if n := len(findPayloadsByType(t, evs, events.TypeSessionTitleInvoked)); n != 1 {
		t.Errorf("invoked count = %d, want 1", n)
	}
	if n := len(findPayloadsByType(t, evs, events.TypeSessionTitleGenerated)); n != 0 {
		t.Errorf("generated count = %d, want 0 on LLM error", n)
	}
}

// erroringLLM is a fakeLLM variant that always returns err. Defined
// here rather than in the shared test helpers because no other test
// needs an always-failing LLM today.
type erroringLLM struct{ err error }

func (l *erroringLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return nil, l.err
}

func TestNormalizeSessionTitle(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Reset password", "Reset password"},
		{"trailing whitespace", "Reset password   ", "Reset password"},
		{"trailing period", "Reset password.", "Reset password"},
		{"trailing exclaim", "Help me!", "Help me"},
		{"straight double-quotes", `"Reset password"`, "Reset password"},
		{"smart double-quotes", "“Reset password”", "Reset password"},
		{"single quotes", "'Reset password'", "Reset password"},
		{"empty after trim", "   ", ""},
		{"unicode preserved", "Inventarliste filtern", "Inventarliste filtern"},
		{"length cap", strings.Repeat("a", maxSessionTitleChars+50), strings.Repeat("a", maxSessionTitleChars)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSessionTitle(c.in)
			if got != c.want {
				t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
