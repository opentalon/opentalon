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
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
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
		{Content: "Checking.", Thinking: replayThinking, ToolCalls: []provider.ToolCall{
			{ID: "c-a", Name: "p__read_a", Arguments: map[string]string{}},
			{ID: "c-b", Name: "p__read_b", Arguments: map[string]string{}},
		}},
		{Content: "Done.", Thinking: replayThinking},
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
	got := map[string][]json.RawMessage{}
	for _, m := range s.Messages {
		if m.Role != provider.RoleAssistant {
			continue
		}
		key := m.Content
		if len(m.ToolCalls) > 0 {
			key = m.ToolCalls[0].ID
		}
		got[key] = m.Thinking
	}
	want := map[string][]json.RawMessage{"c-a": replayThinking, "c-b": nil, "Done.": nil}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("thinking per assistant message:\n got %q\nwant %q", got, want)
	}
}

// A tool call that needs approval leaves the agent loop before its message is
// stored. The pending call keeps the response's thinking blocks, so an approval
// handled by a fresh orchestrator (a restart) stores them with the call, and
// the next Anthropic request replays them ahead of its tool_use.
func TestRun_ApprovedCallReplaysThinkingAfterRestart(t *testing.T) {
	const toolUse = `{"type":"tool_use","id":"toolu_1","name":"p__delete","input":{"id":"7"}}`
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		content := `{"type":"text","text":"Delete item 7?"}` // the confirmation question
		switch {
		case strings.Contains(body, `"tool_result"`):
			content = `{"type":"text","text":"Deleted."}`
		case strings.Contains(body, `"tools":[`):
			content = string(replayThinking[0]) + "," + string(replayThinking[1]) + "," + toolUse
		}
		_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()

	exec := &countingExecutor{}
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "p", Actions: []Action{{
			Name: "delete", Description: "Delete an item.", AlwaysInclude: true,
			Parameters: []Parameter{{Name: "id", Description: "Item id.", Required: true}},
		}},
	}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "conf", Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{}); err != nil {
		t.Fatalf("register conf: %v", err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
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

	if _, err := newOrch().Run(ctx, "s1", "delete item 7"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exec.count != 0 {
		t.Fatalf("the write ran before the approval (%d times)", exec.count)
	}
	if _, _, _, blocks := loadPendingToolCall(sessions, "s1"); !reflect.DeepEqual(blocks, replayThinking) {
		t.Fatalf("pending call thinking = %q, want the response's blocks", blocks)
	}

	if _, err := newOrch().Run(actor.WithConfirmationDecision(ctx, "approve"), "s1", "approve"); err != nil {
		t.Fatalf("approval Run: %v", err)
	}
	if exec.count != 1 {
		t.Fatalf("the approved write ran %d times, want 1", exec.count)
	}
	s, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	var stored []json.RawMessage
	for _, m := range s.Messages {
		if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "toolu_1" {
			stored = m.Thinking
		}
	}
	if !reflect.DeepEqual(stored, replayThinking) {
		t.Errorf("approved call message thinking = %q, want the response's blocks", stored)
	}

	mu.Lock()
	defer mu.Unlock()
	var next string
	for _, b := range bodies {
		if strings.Contains(b, `"tool_result"`) {
			next = b
		}
	}
	var sent struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(next), &sent); err != nil {
		t.Fatalf("request after the approval: %v (%q)", err, next)
	}
	want := `[` + string(replayThinking[0]) + `,` + string(replayThinking[1]) + `,` + toolUse + `]`
	for _, m := range sent.Messages {
		if m.Role == "assistant" && string(m.Content) == want {
			return
		}
	}
	t.Errorf("request after the approval does not replay %s:\n%s", want, next)
}
