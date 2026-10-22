package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store"
)

// replayThinking is a response's thinking blocks as Anthropic returns them
// with a tool call.
var replayThinking = []json.RawMessage{
	json.RawMessage(`{"type":"thinking","thinking":"","signature":"sig-1"}`),
	json.RawMessage(`{"type":"redacted_thinking","data":"enc-1"}`),
}

// One response with thinking blocks and two tool calls is stored as one
// assistant message per call: the first carries the blocks, the second does
// not. The final text answer stores none, though its response had blocks too.
func TestRun_ThinkingRidesOnFirstToolCallMessage(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "thinking fixtures",
		Actions: []Action{
			{Name: "read_a", Description: "First read.", ReadOnly: true, AlwaysInclude: true},
			{Name: "read_b", Description: "Second read.", ReadOnly: true, AlwaysInclude: true},
		},
	}, &countingExecutor{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	llm := &scriptedNativeLLM{rounds: []provider.CompletionResponse{
		{Content: "Checking.", Thinking: replayThinking, ThinkingFingerprint: "fp-1", ToolCalls: []provider.ToolCall{
			{ID: "c-a", Name: "p__read_a", Arguments: map[string]string{}},
			{ID: "c-b", Name: "p__read_b", Arguments: map[string]string{}},
		}},
		{Content: "Done.", Thinking: replayThinking, ThinkingFingerprint: "fp-2"},
	}}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{InjectionStateStore: &fakeInjectionStateStore{}})

	if _, err := orch.Run(actor.WithSessionID(context.Background(), "s1"), "s1", "read both"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	got := map[string]responseThinking{}
	for _, m := range s.Messages {
		if m.Role != provider.RoleAssistant {
			continue
		}
		key := m.Content
		if len(m.ToolCalls) > 0 {
			key = m.ToolCalls[0].ID
		}
		got[key] = responseThinking{Blocks: m.Thinking, Fingerprint: m.ThinkingFingerprint}
	}
	want := map[string]responseThinking{"c-a": {Blocks: replayThinking, Fingerprint: "fp-1"}, "c-b": {}, "Done.": {}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("thinking per assistant message:\n got %+v\nwant %+v", got, want)
	}
}

// A tool call that needs approval leaves the agent loop before its message is
// stored. The pending call keeps the response's thinking blocks and their
// fingerprint, so an approval handled by a fresh orchestrator (a restart) on
// the SQLite store stores both with the call, and the next Anthropic request
// replays the blocks ahead of its tool_use. The call comes in the second
// round, after the system prompt's per-round part has changed, so its block
// is still valid after the approval.
func TestRun_ApprovedCallReplaysThinkingAfterRestart(t *testing.T) {
	const toolUse = `{"type":"tool_use","id":"toolu_1","name":"p__delete","input":{"id":"7"}}`
	var mu sync.Mutex
	var bodies []string
	chatRounds := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, body)
		content := `{"type":"text","text":"Delete item 7?"}` // the confirmation question
		if strings.Contains(body, `"tools":[`) {
			chatRounds++
			switch chatRounds {
			case 1:
				content = `{"type":"thinking","thinking":"","signature":"sig-0"},` +
					`{"type":"tool_use","id":"toolu_0","name":"p__show","input":{"id":"7"}}`
			case 2:
				content = string(replayThinking[0]) + "," + string(replayThinking[1]) + "," + toolUse
			default:
				content = `{"type":"text","text":"Deleted."}`
			}
		}
		_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	exec := &countingExecutor{}
	registry := NewToolRegistry()
	itemID := []Parameter{{Name: "id", Description: "Item id.", Required: true}}
	if err := registry.Register(PluginCapability{
		Name: "p", Actions: []Action{
			{Name: "show", Description: "Show an item.", AlwaysInclude: true, ReadOnly: true, Parameters: itemID},
			{Name: "delete", Description: "Delete an item.", AlwaysInclude: true, Parameters: itemID},
		},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := sqliteSessions(t)
	// Each orchestrator gets its own provider over the shared session store,
	// as an instance started after a restart does.
	newOrch := func() *Orchestrator {
		anth, err := provider.FromConfig(provider.ProviderConfig{
			ID: "anthropic", API: provider.APIAnthropic, BaseURL: server.URL, APIKey: "key",
			Models: []provider.ModelInfo{{ID: "claude-x", Thinking: "adaptive", Features: []provider.Feature{provider.FeatureTools}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return NewWithRules(anth, DefaultParser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			ConfirmationPlugin:  "conf",
			ConfirmationAction:  "check",
			InjectionStateStore: &fakeInjectionStateStore{},
		})
	}
	// The profile names the model, so its adaptive thinking applies.
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), &profile.Profile{Model: "anthropic/claude-x"}), "s1")

	if _, err := newOrch().Run(ctx, "s1", "show item 7, then delete it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 1 {
		t.Fatalf("only the read may run before the approval, ran %d calls", exec.count)
	}
	_, _, _, pending := loadPendingToolCall(sessions, "s1")
	if !reflect.DeepEqual(pending.Blocks, replayThinking) || pending.Fingerprint == "" {
		t.Fatalf("pending call thinking = %+v, want the response's blocks with a fingerprint", pending)
	}

	if _, err := newOrch().Run(actor.WithConfirmationDecision(ctx, "approve"), "s1", "approve"); err != nil {
		t.Fatalf("approval Run: %v", err)
	}
	if exec.count != 2 {
		t.Fatalf("the approved write did not run once (%d calls in all)", exec.count)
	}
	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	var stored responseThinking
	for _, m := range s.Messages {
		if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "toolu_1" {
			stored = responseThinking{Blocks: m.Thinking, Fingerprint: m.ThinkingFingerprint}
		}
	}
	if !reflect.DeepEqual(stored, pending) {
		t.Errorf("approved call message thinking = %+v, want the pending call's %+v", stored, pending)
	}

	var next string
	mu.Lock()
	for _, b := range bodies {
		if strings.Contains(b, `"id":"toolu_1"`) && strings.Contains(b, `"tool_result"`) {
			next = b
		}
	}
	mu.Unlock()
	want := `[` + string(replayThinking[0]) + `,` + string(replayThinking[1]) + `,` + toolUse + `]`
	for _, m := range sentMessages(t, next) {
		if string(m) == `{"role":"assistant","content":`+want+`}` {
			return
		}
	}
	t.Errorf("request after the approval does not replay %s:\n%s", want, next)
}

// sqliteSessions returns a SQLite session store holding session s1, so a Run
// loads the history the previous Run stored.
func sqliteSessions(t *testing.T) *store.SessionStore {
	t.Helper()
	db, err := store.Open(config.DBConfig{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := store.NewSessionStore(db, 0, 0)
	sessions.Create(state.SessionParams{ID: "s1"})
	return sessions
}

// sentMessages returns the messages of an Anthropic request body, each as
// the bytes sent.
func sentMessages(t *testing.T, body string) []json.RawMessage {
	t.Helper()
	var sent struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("request body: %v (%q)", err, body)
	}
	return sent.Messages
}

// sentSystem returns the system field of an Anthropic request body.
func sentSystem(t *testing.T, body string) string {
	t.Helper()
	var sent struct {
		System json.RawMessage `json:"system"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("request body: %v (%q)", err, body)
	}
	return string(sent.System)
}

// thinkingServer answers the n-th chat-loop request (one with tools) with
// replies[n-1], or a text answer past the end, and records every request.
func thinkingServer(t *testing.T, replies ...string) (url string, chatBodies func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		content := `{"type":"text","text":"Answer."}`
		if strings.Contains(string(raw), `"tools":[`) {
			bodies = append(bodies, string(raw))
			if n := len(bodies); n <= len(replies) {
				content = replies[n-1]
			}
		}
		_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// thinkingOrch returns an orchestrator on a real Anthropic provider over url
// with read-only tools p__read_a, p__read_b and p__read_c.
func thinkingOrch(t *testing.T, url string, model provider.ModelInfo, sessions SessionStoreInterface) *Orchestrator {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Description: "thinking fixtures",
		Actions: []Action{
			{Name: "read_a", Description: "First read.", ReadOnly: true, AlwaysInclude: true},
			{Name: "read_b", Description: "Second read.", ReadOnly: true, AlwaysInclude: true},
			{Name: "read_c", Description: "Third read.", ReadOnly: true, AlwaysInclude: true},
		},
	}, &countingExecutor{}); err != nil {
		t.Fatal(err)
	}
	anth, err := provider.FromConfig(provider.ProviderConfig{
		ID: "anthropic", API: provider.APIAnthropic, BaseURL: url, APIKey: "key", PromptCache: true,
		Models: []provider.ModelInfo{model},
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewWithRules(anth, DefaultParser, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{InjectionStateStore: &fakeInjectionStateStore{}})
}

const (
	thinkingSig1 = `{"type":"thinking","thinking":"","signature":"sig-1"}`
	thinkingSig2 = `{"type":"thinking","thinking":"","signature":"sig-2"}`
)

// With adaptive thinking a block goes back only while what Anthropic binds it
// to is unchanged. The round-1 block was produced before the system prompt's
// per-round part changed, so neither round 2 nor the next turn sends it, and
// the two requests share their messages byte for byte. The round-2 block was
// produced under the current prompt, so round 3 and the next turn send it.
func TestRun_AdaptiveThinkingReplaysOnlyUnchangedPrefix(t *testing.T) {
	url, chatBodies := thinkingServer(t,
		thinkingSig1+`,{"type":"tool_use","id":"toolu_a","name":"p__read_a","input":{}},`+
			`{"type":"tool_use","id":"toolu_b","name":"p__read_b","input":{}}`,
		thinkingSig2+`,{"type":"tool_use","id":"toolu_c","name":"p__read_c","input":{}}`,
	)
	model := provider.ModelInfo{ID: "claude-x", Thinking: "adaptive", ReasoningEffort: "high",
		Features: []provider.Feature{provider.FeatureTools}}
	orch := thinkingOrch(t, url, model, sqliteSessions(t))
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), &profile.Profile{Model: "anthropic/claude-x"}), "s1")
	if _, err := orch.Run(ctx, "s1", "read all three"); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Run(ctx, "s1", "and again"); err != nil {
		t.Fatal(err)
	}

	bodies := chatBodies()
	if len(bodies) != 4 {
		t.Fatalf("chat-loop requests = %d, want 4 (three rounds, then the next turn)", len(bodies))
	}
	if sentSystem(t, bodies[0]) == sentSystem(t, bodies[1]) {
		t.Fatal("the system prompt must change between round 1 and round 2 for this test")
	}
	const callC = `{"role":"assistant","content":[` + thinkingSig2 + `,{"type":"tool_use","id":"toolu_c","name":"p__read_c","input":{}}]}`
	for i, name := range []string{"round 2", "round 3", "next turn"} {
		body := bodies[i+1]
		if strings.Contains(body, "sig-1") {
			t.Errorf("%s sends the block produced under the earlier system prompt", name)
		}
		if i > 0 && !containsMessage(sentMessages(t, body), callC) {
			t.Errorf("%s does not replay the round-2 block ahead of its call %s", name, callC)
		}
	}
	nextTurn := sentMessages(t, bodies[3])
	for i, name := range []string{"round 2", "round 3"} {
		earlier := sentMessages(t, bodies[i+1])
		if len(earlier) > len(nextTurn) {
			t.Fatalf("%s has more messages than the next turn", name)
		}
		for j, m := range earlier {
			if string(m) != string(nextTurn[j]) {
				t.Errorf("message %d differs between %s and the next turn:\n %s\n %s", j, name, m, nextTurn[j])
			}
		}
	}
}

// With a thinking budget the unfinished turn's block goes back as before,
// although the system prompt changed after the first tool result: those
// models do not check the prefix and need the block to continue the turn.
func TestRun_BudgetThinkingReplaysUnfinishedTurn(t *testing.T) {
	url, chatBodies := thinkingServer(t,
		thinkingSig1+`,{"type":"tool_use","id":"toolu_a","name":"p__read_a","input":{}}`)
	model := provider.ModelInfo{ID: "claude-x", Features: []provider.Feature{provider.FeatureTools, provider.FeatureReasoning}}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := thinkingOrch(t, url, model, sessions)
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), &profile.Profile{Model: "anthropic/claude-x"}), "s1")
	if _, err := orch.Run(ctx, "s1", "read it"); err != nil {
		t.Fatal(err)
	}

	bodies := chatBodies()
	if len(bodies) != 2 {
		t.Fatalf("chat-loop requests = %d, want 2", len(bodies))
	}
	if sentSystem(t, bodies[0]) == sentSystem(t, bodies[1]) {
		t.Fatal("the system prompt must change between round 1 and round 2 for this test")
	}
	const callA = `{"role":"assistant","content":[` + thinkingSig1 + `,{"type":"tool_use","id":"toolu_a","name":"p__read_a","input":{}}]}`
	if !containsMessage(sentMessages(t, bodies[1]), callA) {
		t.Errorf("round 2 does not replay the block ahead of its call %s:\n%s", callA, bodies[1])
	}
}

func containsMessage(msgs []json.RawMessage, want string) bool {
	for _, m := range msgs {
		if string(m) == want {
			return true
		}
	}
	return false
}
