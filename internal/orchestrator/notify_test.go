package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// convPushCall records one ConversationSender invocation.
type convPushCall struct {
	ChannelID      string
	ConversationID string
	Msg            pkgchannel.OutboundMessage
}

// convPushRecorder captures ConversationSender invocations and can be made to
// fail, so transport errors are exercised alongside the happy path.
type convPushRecorder struct {
	mu    sync.Mutex
	calls []convPushCall
	err   error
}

func (r *convPushRecorder) push(_ context.Context, channelID, conversationID string, msg pkgchannel.OutboundMessage) error {
	r.mu.Lock()
	r.calls = append(r.calls, convPushCall{ChannelID: channelID, ConversationID: conversationID, Msg: msg})
	r.mu.Unlock()
	return r.err
}

func (r *convPushRecorder) snapshot() []convPushCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]convPushCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// notifyTestOrch builds an orchestrator with the notify surface wired and a
// session seeded, returning the orchestrator, its executor, and both recorders.
func notifyTestOrch(t *testing.T, opts OrchestratorOpts) (*Orchestrator, *notifyExecutor, *escPushRecorder, *convPushRecorder) {
	t.Helper()
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("ent1:telegram:42", "ent1", "grp1", "")
	sessionPush := newEscPushRecorder()
	convPush := &convPushRecorder{}
	opts.ChannelSender = sessionPush.push
	opts.ConversationSender = convPush.push
	orch := NewWithRules(&fakeLLM{}, DefaultParser, registry, memory, sessions, opts)
	return orch, &notifyExecutor{orch: orch}, sessionPush, convPush
}

func notifyCall(fromLLM bool, args map[string]string) ToolCall {
	return ToolCall{
		ID:      "n-1",
		Plugin:  notifyPluginName,
		Action:  notifySendAction,
		Args:    args,
		FromLLM: fromLLM,
	}
}

func decodeNotifyStatus(t *testing.T, res ToolResult) notifyResult {
	t.Helper()
	if res.Error != "" {
		t.Fatalf("unexpected executor error: %s", res.Error)
	}
	var got notifyResult
	if err := json.Unmarshal([]byte(res.Content), &got); err != nil {
		t.Fatalf("decode notify status %q: %v", res.Content, err)
	}
	return got
}

func enabledNotify() OrchestratorOpts {
	return OrchestratorOpts{Notify: NotifyConfig{Enabled: true}}
}

func TestNotify_PushesToSessionWithProvenance(t *testing.T) {
	_, exec, sessionPush, conv := notifyTestOrch(t, enabledNotify())
	res := exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "stock for ABC-123 dropped to 8", "session_id": "ent1:telegram:42",
		"entity_id": "ent1", "group_id": "grp1",
		"source": "agent", "agent_id": "ag-7", "trigger": "poll",
	}))
	if got := decodeNotifyStatus(t, res); !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	calls := sessionPush.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected one session push, got %d", len(calls))
	}
	if calls[0].SessionID != "ent1:telegram:42" || calls[0].Msg.Content != "stock for ABC-123 dropped to 8" {
		t.Errorf("wrong push: %+v", calls[0])
	}
	md := calls[0].Msg.Metadata
	if md["type"] != notificationMessageType || md["source"] != "agent" || md["agent_id"] != "ag-7" || md["trigger"] != "poll" {
		t.Errorf("metadata = %+v", md)
	}
	if len(conv.snapshot()) != 0 {
		t.Errorf("session target must not also hit the conversation sender")
	}
}

func TestNotify_PushesToExplicitConversation(t *testing.T) {
	_, exec, sessionPush, conv := notifyTestOrch(t, enabledNotify())
	res := exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "daily report ready", "channel_id": "telegram", "conversation_id": "42",
	}))
	if got := decodeNotifyStatus(t, res); !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	calls := conv.snapshot()
	if len(calls) != 1 || calls[0].ChannelID != "telegram" || calls[0].ConversationID != "42" {
		t.Fatalf("wrong conversation push: %+v", calls)
	}
	if calls[0].Msg.ConversationID != "42" {
		t.Errorf("message should carry the conversation id: %+v", calls[0].Msg)
	}
	if len(sessionPush.snapshot()) != 0 {
		t.Errorf("explicit pair must not go through the session sender")
	}
}

func TestNotify_DisabledIsNotRegisteredAndRefuses(t *testing.T) {
	orch, exec, _, _ := notifyTestOrch(t, OrchestratorOpts{})
	if _, ok := orch.registry.GetExecutor(notifyPluginName); ok {
		t.Error("_notify must not be registered when disabled")
	}
	got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "ent1:telegram:42",
	})))
	if got.Delivered || got.Reason != "disabled" {
		t.Errorf("expected a disabled refusal, got %+v", got)
	}
}

func TestNotify_NotCallableByTheModel(t *testing.T) {
	_, exec, sessionPush, _ := notifyTestOrch(t, enabledNotify())
	res := exec.Execute(context.Background(), notifyCall(true, map[string]string{
		"text": "hi", "session_id": "ent1:telegram:42",
	}))
	if res.Error == "" {
		t.Fatal("an LLM-sourced call must be rejected")
	}
	if len(sessionPush.snapshot()) != 0 {
		t.Error("nothing may be pushed for an LLM-sourced call")
	}
}

func TestNotify_RejectsCrossEntityTarget(t *testing.T) {
	_, exec, sessionPush, _ := notifyTestOrch(t, enabledNotify())
	got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "someone-else:telegram:99", "entity_id": "ent1",
	})))
	if got.Delivered || got.Reason != "entity_mismatch" {
		t.Fatalf("expected an entity_mismatch refusal, got %+v", got)
	}
	if len(sessionPush.snapshot()) != 0 {
		t.Error("a mismatched target must not be delivered to")
	}
}

func TestNotify_AnonymousSessionKeyHasNoOwnerToCheck(t *testing.T) {
	// A 2-part key carries no entity segment; refusing here would break every
	// profile-less deployment.
	_, exec, sessionPush, _ := notifyTestOrch(t, enabledNotify())
	got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "telegram:42", "entity_id": "ent1",
	})))
	if !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	if len(sessionPush.snapshot()) != 1 {
		t.Error("expected the push to go out")
	}
}

func TestNotify_ArgumentErrors(t *testing.T) {
	_, exec, _, _ := notifyTestOrch(t, enabledNotify())
	cases := []struct {
		name string
		args map[string]string
	}{
		{"no text", map[string]string{"session_id": "ent1:telegram:42"}},
		{"blank text", map[string]string{"text": "   ", "session_id": "ent1:telegram:42"}},
		{"no target at all", map[string]string{"text": "hi"}},
		{"channel without conversation", map[string]string{"text": "hi", "channel_id": "telegram"}},
		{"conversation without channel", map[string]string{"text": "hi", "conversation_id": "42"}},
	}
	for _, c := range cases {
		res := exec.Execute(context.Background(), notifyCall(false, c.args))
		if res.Error == "" {
			t.Errorf("%s: expected an error, got content %q", c.name, res.Content)
		}
	}
}

func TestNotify_TargetDefaultsToCallerSession(t *testing.T) {
	_, exec, sessionPush, _ := notifyTestOrch(t, enabledNotify())
	ctx := actor.WithSessionID(context.Background(), "ent1:telegram:42")
	if got := decodeNotifyStatus(t, exec.Execute(ctx, notifyCall(false, map[string]string{"text": "hi"}))); !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	calls := sessionPush.snapshot()
	if len(calls) != 1 || calls[0].SessionID != "ent1:telegram:42" {
		t.Fatalf("expected the caller's session to be used: %+v", calls)
	}
}

func TestNotify_TransportFailureIsAnError(t *testing.T) {
	// A refusal ("we chose not to") and a broken channel ("we tried") must not
	// look the same to the caller: one is a status, the other an error.
	_, exec, _, conv := notifyTestOrch(t, enabledNotify())
	conv.err = fmt.Errorf("telegram 400 chat not found")
	res := exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "channel_id": "telegram", "conversation_id": "42",
	}))
	if res.Error == "" {
		t.Fatalf("expected a transport error, got content %q", res.Content)
	}
}

func TestNotify_NoSenderWiredIsRefusedNotCrashed(t *testing.T) {
	registry := NewToolRegistry()
	sessions := state.NewSessionStore("")
	orch := NewWithRules(&fakeLLM{}, DefaultParser, registry, state.NewMemoryStore(""), sessions, enabledNotify())
	exec := &notifyExecutor{orch: orch}

	got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "ent1:telegram:42",
	})))
	if got.Delivered || got.Reason != "no_channel_sender" {
		t.Errorf("session path without a sender: %+v", got)
	}
	got = decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "channel_id": "telegram", "conversation_id": "42",
	})))
	if got.Delivered || got.Reason != "no_conversation_sender" {
		t.Errorf("conversation path without a sender: %+v", got)
	}
}

func TestNotify_AppendsToSessionTranscript(t *testing.T) {
	orch, exec, _, _ := notifyTestOrch(t, enabledNotify())
	if got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "stock dropped to 8", "session_id": "ent1:telegram:42",
	}))); !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	sess, err := orch.sessions.Get("ent1:telegram:42")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(sess.Messages) != 1 || sess.Messages[0].Content != "stock dropped to 8" {
		t.Fatalf("notification should land in the transcript, got %+v", sess.Messages)
	}
	if sess.Messages[0].Role != "assistant" {
		t.Errorf("role = %q, want assistant", sess.Messages[0].Role)
	}
}

func TestNotify_UnknownSessionStillDelivers(t *testing.T) {
	// The transcript append is best-effort: a target session that no longer
	// exists must not swallow the alert.
	_, exec, sessionPush, _ := notifyTestOrch(t, enabledNotify())
	if got := decodeNotifyStatus(t, exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "ent1:telegram:does-not-exist",
	}))); !got.Delivered {
		t.Fatalf("expected delivery, got %+v", got)
	}
	if len(sessionPush.snapshot()) != 1 {
		t.Error("expected the push to go out despite the missing session")
	}
}

func TestNotify_StatusIsAlsoStructuredContent(t *testing.T) {
	// Plugins decode the JSON status from StructuredContent; leaving it empty
	// would make every refusal read as a success on the caller's side.
	_, exec, _, _ := notifyTestOrch(t, enabledNotify())
	res := exec.Execute(context.Background(), notifyCall(false, map[string]string{
		"text": "hi", "session_id": "ent1:telegram:42",
	}))
	if res.StructuredContent == "" {
		t.Fatal("status must be exposed as structured content")
	}
	var got notifyResult
	if err := json.Unmarshal([]byte(res.StructuredContent), &got); err != nil || !got.Delivered {
		t.Fatalf("structured status = %q (%v)", res.StructuredContent, err)
	}
}
