package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// fakeMessagesAPI stands in for Anthropic's Messages API. It keeps each
// request body as sent and answers a request whose last message is a tool
// result with text, any other with a call to inventory__list-items, so every
// turn takes two rounds and loads no tool.
type fakeMessagesAPI struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (f *fakeMessagesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	n := len(f.bodies)
	f.mu.Unlock()
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	w.Header().Set("Content-Type", "application/json")
	if len(req.Messages) > 0 && bytes.Contains(req.Messages[len(req.Messages)-1].Content, []byte(`"tool_result"`)) {
		_, _ = fmt.Fprintf(w, `{"id":"msg_%d","model":"claude-x","content":[{"type":"text","text":"Two items."}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, n)
		return
	}
	_, _ = fmt.Fprintf(w, `{"id":"msg_%d","model":"claude-x","content":[{"type":"tool_use","id":"toolu_%d",`+
		`"name":"inventory__list-items","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`, n, n)
}

// cachedPrefix holds the parts of a request body Anthropic reads up to the
// first cache marker (tools, model, thinking, first system block), plus the
// messages and the automatic marker.
type cachedPrefix struct {
	Tools        json.RawMessage   `json:"tools"`
	Model        json.RawMessage   `json:"model"`
	Thinking     json.RawMessage   `json:"thinking"`
	System       []json.RawMessage `json:"system"`
	Messages     []json.RawMessage `json:"messages"`
	CacheControl json.RawMessage   `json:"cache_control"`
}

// TestAnthropicCachedPrefixIsStable renders the chat loop's requests on the
// wire through the Anthropic provider with prompt caching on: two turns of
// two rounds in one conversation, then a turn in a second conversation with
// the same plugins and tools. Everything up to the first cache marker is the
// same bytes in every request, and within a turn each request's messages
// begin with the previous request's messages. No round adds a one-round
// message (nudge, retry, warning).
func TestAnthropicCachedPrefixIsStable(t *testing.T) {
	api := &fakeMessagesAPI{}
	server := httptest.NewServer(api)
	defer server.Close()
	llm, err := provider.FromConfig(provider.ProviderConfig{
		ID: "anthropic", API: provider.APIAnthropic, BaseURL: server.URL, APIKey: "key", PromptCache: true,
		Models: []provider.ModelInfo{{ID: "claude-x", Features: []provider.Feature{provider.FeatureTools, provider.FeatureReasoning}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "inventory", Description: "Inventory tools",
		SystemPromptAddition: "Count items with list-items.",
		Actions: []Action{
			native(Action{Name: "list-items", Description: "List items", ReadOnly: true}),
			{Name: "add-to-stock", Description: "Add stock"},
		},
	}, &echoExecutor{}); err != nil {
		t.Fatal(err)
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sessions.Create(state.SessionParams{ID: "s2"})
	orch := NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{Enabled: true},
	})
	conversation := func(id string) context.Context {
		ctx := profile.WithProfile(context.Background(), &profile.Profile{ChannelID: "web", Name: "Ada", Model: "anthropic/claude-x"})
		ctx = actor.WithConversationID(ctx, id)
		return pkgchannel.WithCapabilities(ctx, pkgchannel.Capabilities{ResponseFormat: pkgchannel.FormatMarkdown})
	}

	turns := []struct {
		ctx       context.Context
		sessionID string
		message   string
	}{
		{conversation("conv-1"), "s1", "How many items are low on stock?"},
		{conversation("conv-1"), "s1", "And how many are out of stock?"},
		{conversation("conv-2"), "s2", "How many items are low on stock?"},
	}
	var labels []string
	for i, turn := range turns {
		if _, err := orch.Run(turn.ctx, turn.sessionID, turn.message); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, fmt.Sprintf("turn %d round 1", i+1), fmt.Sprintf("turn %d round 2", i+1))
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.bodies) != len(labels) {
		t.Fatalf("model calls = %d, want %d (two rounds per turn)", len(api.bodies), len(labels))
	}
	requests := make([]cachedPrefix, len(api.bodies))
	for i, body := range api.bodies {
		if err := json.Unmarshal(body, &requests[i]); err != nil {
			t.Fatalf("%s: system is not cache-marked blocks: %v\n%s", labels[i], err, body)
		}
		r := requests[i]
		if len(r.Tools) == 0 || len(r.Thinking) == 0 || len(r.System) == 0 {
			t.Fatalf("%s: want tools, thinking and system blocks:\n%s", labels[i], body)
		}
		var first struct {
			CacheControl *anthropicCacheControl `json:"cache_control"`
		}
		if err := json.Unmarshal(r.System[0], &first); err != nil || first.CacheControl == nil ||
			first.CacheControl.Type != "ephemeral" || string(r.CacheControl) != `{"type":"ephemeral"}` {
			t.Fatalf("%s: want a cache marker on the first system block and the automatic marker:\n%s", labels[i], body)
		}
	}

	want := requests[0]
	for i, r := range requests[1:] {
		label := labels[i+1]
		for _, part := range []struct {
			name      string
			got, want []byte
		}{
			{"tools", r.Tools, want.Tools},
			{"model", r.Model, want.Model},
			{"thinking", r.Thinking, want.Thinking},
			{"first system block", r.System[0], want.System[0]},
		} {
			if !bytes.Equal(part.got, part.want) {
				t.Errorf("%s: %s differs from turn 1 round 1:\n got %s\nwant %s", label, part.name, part.got, part.want)
			}
		}
	}
	for i := 1; i < len(requests); i += 2 {
		prev, cur := requests[i-1].Messages, requests[i].Messages
		if len(cur) < len(prev) {
			t.Errorf("%s: %d messages, fewer than the round before", labels[i], len(cur))
			continue
		}
		for j := range prev {
			if !bytes.Equal(cur[j], prev[j]) {
				t.Errorf("%s: message %d differs from the round before:\n got %s\nwant %s", labels[i], j, cur[j], prev[j])
			}
		}
	}
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type scriptedMessagesAPI struct {
	mu     sync.Mutex
	script []string
	bodies []string
	betas  []string
}

func (a *scriptedMessagesAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	content := `{"type":"text","text":"Answer."}`
	if bytes.Contains(raw, []byte(`"tools":[`)) {
		a.bodies = append(a.bodies, string(raw))
		a.betas = append(a.betas, r.Header.Get("anthropic-beta"))
		if n := len(a.bodies); n <= len(a.script) {
			content = a.script[n-1]
		}
	}
	_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
}

func (a *scriptedMessagesAPI) requests(t *testing.T) []wireRequest {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	reqs := make([]wireRequest, len(a.bodies))
	for i, body := range a.bodies {
		reqs[i] = parseWireRequest(t, body)
	}
	return reqs
}

type callableRecorder struct {
	provider.Provider
	mu       sync.Mutex
	callable [][]string
}

func (r *callableRecorder) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if req.Segments != nil {
		names := []string{}
		for _, td := range req.Tools {
			if td.OnDemand {
				names = append(names, td.Name)
			}
		}
		slices.Sort(names)
		r.mu.Lock()
		r.callable = append(r.callable, names)
		r.mu.Unlock()
	}
	return r.Provider.Complete(ctx, req)
}

type inlineRun struct {
	api      *scriptedMessagesAPI
	llm      *callableRecorder
	orch     *Orchestrator
	ctx      context.Context
	sessions SessionStoreInterface
}

func newInlineRun(t *testing.T, registry *ToolRegistry, opts OrchestratorOpts, script ...string) *inlineRun {
	t.Helper()
	return newInlineRunOn(t, sqliteSessions(t), registry, opts, script...)
}

func newInlineRunOn(t *testing.T, sessions SessionStoreInterface, registry *ToolRegistry, opts OrchestratorOpts, script ...string) *inlineRun {
	t.Helper()
	model := provider.ModelInfo{ID: "claude-x", Features: []provider.Feature{provider.FeatureTools, provider.FeatureInlineTools}}
	return newModelRun(t, sessions, model, registry, opts, script...)
}

func newModelRun(t *testing.T, sessions SessionStoreInterface, model provider.ModelInfo, registry *ToolRegistry, opts OrchestratorOpts,
	script ...string) *inlineRun {
	t.Helper()
	api := &scriptedMessagesAPI{script: script}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	anth, err := provider.FromConfig(provider.ProviderConfig{
		ID: "anthropic", API: provider.APIAnthropic, BaseURL: server.URL, APIKey: "key", PromptCache: true,
		Models: []provider.ModelInfo{model},
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.InjectionStateStore == nil {
		opts.InjectionStateStore = &fakeInjectionStateStore{}
	}
	llm := &callableRecorder{Provider: anth}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, opts)
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), &profile.Profile{Model: "anthropic/claude-x"}), "s1")
	return &inlineRun{api: api, llm: llm, orch: orch, ctx: ctx, sessions: sessions}
}

func inlineRegistry(t *testing.T, actions []Action, exec PluginExecutor) *ToolRegistry {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Description: "inline tools fixtures", Actions: actions}, exec); err != nil {
		t.Fatal(err)
	}
	return registry
}

func (r *inlineRun) turn(t *testing.T, ctx context.Context, message string) {
	t.Helper()
	if _, err := r.orch.Run(ctx, "s1", message); err != nil {
		t.Fatalf("Run %q: %v", message, err)
	}
}

type wireRequest struct {
	Tools    json.RawMessage   `json:"tools"`
	System   []json.RawMessage `json:"system"`
	Messages []json.RawMessage `json:"messages"`
}

func parseWireRequest(t *testing.T, body string) wireRequest {
	t.Helper()
	var w wireRequest
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		t.Fatalf("request body: %v\n%s", err, body)
	}
	return w
}

func wireChanges(msg json.RawMessage) []string {
	var m struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Tool struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tool"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil || m.Role != "system" {
		return nil
	}
	out := []string{}
	for _, b := range m.Content {
		sign := map[string]string{"tool_addition": "+", "tool_removal": "-"}[b.Type]
		if sign == "" || b.Tool.Type != "tool_reference" {
			return []string{"unexpected block " + b.Type + "/" + b.Tool.Type}
		}
		out = append(out, sign+b.Tool.Name)
	}
	return out
}

func toolResultID(msg json.RawMessage) string {
	var m struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil || len(m.Content) != 1 || m.Content[0].Type != "tool_result" {
		return ""
	}
	return m.Content[0].ToolUseID
}

func checkWireAnnouncedIsCallable(t *testing.T, reqs []wireRequest, callable [][]string) {
	t.Helper()
	if len(reqs) != len(callable) {
		t.Fatalf("%d requests on the wire, %d chat-loop requests", len(reqs), len(callable))
	}
	for i, r := range reqs {
		announced := map[string]bool{}
		for _, m := range r.Messages {
			for _, c := range wireChanges(m) {
				if c[0] == '+' {
					announced[c[1:]] = true
				} else {
					delete(announced, c[1:])
				}
			}
		}
		got := slices.Sorted(maps.Keys(announced))
		if !slices.Equal(got, callable[i]) {
			t.Errorf("request %d: announced on the wire %v, callable on demand %v", i+1, got, callable[i])
		}
	}
}

func checkStartsWith(t *testing.T, label string, earlier, later wireRequest, wholeSystem bool) {
	t.Helper()
	if !bytes.Equal(later.Tools, earlier.Tools) {
		t.Errorf("%s: tools differ from the request before:\n got %s\nwant %s", label, later.Tools, earlier.Tools)
	}
	if len(later.System) == 0 || len(earlier.System) == 0 || !bytes.Equal(later.System[0], earlier.System[0]) {
		t.Errorf("%s: the fixed system block differs from the request before", label)
	}
	if wholeSystem {
		if len(later.System) != len(earlier.System) {
			t.Errorf("%s: %d system blocks, the request before had %d", label, len(later.System), len(earlier.System))
		}
		for j := 1; j < min(len(later.System), len(earlier.System)); j++ {
			if !bytes.Equal(later.System[j], earlier.System[j]) {
				t.Errorf("%s: system block %d differs from the request before:\n got %s\nwant %s", label, j, later.System[j], earlier.System[j])
			}
		}
	}
	if len(later.Messages) < len(earlier.Messages) {
		t.Errorf("%s: %d messages, fewer than the %d of the request before", label, len(later.Messages), len(earlier.Messages))
		return
	}
	for j, m := range earlier.Messages {
		if !bytes.Equal(later.Messages[j], m) {
			t.Errorf("%s: message %d differs from the request before:\n got %s\nwant %s", label, j, later.Messages[j], m)
		}
	}
}

func TestAnthropicInlineToolsKeepThePrefixAcrossLoads(t *testing.T) {
	run := newInlineRun(t, inlineRegistry(t, announceActions(), &countingExecutor{}), OrchestratorOpts{},
		`{"type":"tool_use","id":"toolu_a","name":"p__always","input":{}}`,
		`{"type":"tool_use","id":"toolu_l","name":"_meta__load_tools","input":{"names":"p__other"}}`,
		`{"type":"tool_use","id":"toolu_c","name":"p__catalog","input":{"q":"x"}}`,
		`{"type":"tool_use","id":"toolu_r","name":"p__catalog","input":{"q":"x"}}`,
	)
	run.turn(t, run.ctx, "How many items are low on stock?")
	run.turn(t, run.ctx, "And how many are out of stock?")

	reqs := run.api.requests(t)
	labels := []string{"the first tool result", "the round after load_tools", "the round after the load on call",
		"the round after the loaded call", "the next turn"}
	if len(reqs) != len(labels)+1 {
		t.Fatalf("chat-loop requests = %d, want %d", len(reqs), len(labels)+1)
	}
	var tools []struct {
		Name         string `json:"name"`
		DeferLoading bool   `json:"defer_loading"`
	}
	if err := json.Unmarshal(reqs[0].Tools, &tools); err != nil {
		t.Fatal(err)
	}
	var fixed, hidden []string
	for _, td := range tools {
		if td.DeferLoading {
			hidden = append(hidden, td.Name)
			continue
		}
		if len(hidden) > 0 {
			t.Errorf("fixed tool %s follows the hidden ones: %s", td.Name, reqs[0].Tools)
		}
		fixed = append(fixed, td.Name)
	}
	if want := []string{"p__catalog", "p__list", "p__lookup", "p__other"}; !slices.Equal(hidden, want) ||
		!slices.Contains(fixed, "p__always") || !slices.Contains(fixed, toolFQN(metaPluginName, metaLoadTools)) {
		t.Fatalf("tools = fixed %v then hidden %v, want the fixed tools then every on-demand tool hidden in name order %v", fixed, hidden, want)
	}
	for i, beta := range run.api.betas {
		if !strings.Contains(beta, "inline-tools-2026-09-15") {
			t.Errorf("request %d anthropic-beta = %q, want the inline-tools beta", i+1, beta)
		}
	}
	for i, label := range labels {
		checkStartsWith(t, label, reqs[i], reqs[i+1], i > 0)
	}
	type announcement struct {
		after   string
		changes string
	}
	var got []announcement
	last := reqs[len(reqs)-1].Messages
	for j, m := range last {
		if changes := wireChanges(m); changes != nil {
			got = append(got, announcement{toolResultID(last[j-1]), strings.Join(changes, " ")})
		}
	}
	want := []announcement{{"toolu_l", "+p__other"}, {"toolu_c", "+p__catalog +p__list +p__lookup"}}
	if !slices.Equal(got, want) {
		t.Errorf("announcements in the last request (after the tool result of, blocks) = %v, want %v", got, want)
	}
	checkWireAnnouncedIsCallable(t, reqs, run.llm.callable)
}

func TestAnthropicInlineToolsWithdrawOnTheNextTurn(t *testing.T) {
	var capped []Action
	var names []string
	for i := range maxStickyTools + 5 {
		capped = append(capped, Action{Name: fmt.Sprintf("t%02d", i), Description: "Capped tool.", ReadOnly: true})
		names = append(names, fmt.Sprintf("p__t%02d", i))
	}
	for _, tc := range []struct {
		name      string
		opts      OrchestratorOpts
		exec      PluginExecutor
		actions   []Action
		script    []string
		withdrawn []string
	}{
		{"more tools loaded in one turn than the loaded set holds", OrchestratorOpts{}, &countingExecutor{}, capped,
			[]string{`{"type":"tool_use","id":"toolu_l","name":"_meta__load_tools","input":{"names":"` + strings.Join(names, ",") + `"}}`},
			names[maxStickyTools:]},
		{"a tool demoted in the turn that loaded it",
			OrchestratorOpts{ToolErrorHandling: ToolErrorHandlingConfig{StickyDemotionThreshold: 1}}, &errorExecutor{msg: "boom"}, announceActions(),
			[]string{`{"type":"tool_use","id":"toolu_l","name":"_meta__load_tools","input":{"names":"p__other"}}`,
				`{"type":"tool_use","id":"toolu_o","name":"p__other","input":{"q":"x"}}`},
			[]string{"p__other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newInlineRun(t, inlineRegistry(t, tc.actions, tc.exec), tc.opts, tc.script...)
			run.turn(t, run.ctx, "turn 1")
			opening := len(run.api.requests(t))
			run.turn(t, run.ctx, "turn 2")

			reqs := run.api.requests(t)
			msgs := reqs[opening].Messages
			var withdrawn []string
			for _, name := range tc.withdrawn {
				withdrawn = append(withdrawn, "-"+name)
			}
			if len(msgs) < 2 || string(msgs[len(msgs)-2]) != `{"role":"user","content":"turn 2"}` ||
				!slices.Equal(wireChanges(msgs[len(msgs)-1]), withdrawn) {
				t.Fatalf("the turn's first request must end with its user message and then %v, ends with %s", withdrawn, msgs[max(0, len(msgs)-2):])
			}
			checkStartsWith(t, "the turn's first request", reqs[opening-1], reqs[opening], false)
			checkWireAnnouncedIsCallable(t, reqs, run.llm.callable)
		})
	}
}

func TestAnthropicInlineToolsUnreadableStateWithdrawsForOneRequest(t *testing.T) {
	store := &fakeInjectionStateStore{}
	run := newInlineRun(t, inlineRegistry(t, announceActions(), &countingExecutor{}), OrchestratorOpts{InjectionStateStore: store},
		`{"type":"tool_use","id":"toolu_l","name":"_meta__load_tools","input":{"names":"p__other"}}`,
		`{"type":"text","text":"Loaded."}`,
		`{"type":"tool_use","id":"toolu_o","name":"p__other","input":{"q":"x"}}`,
	)
	run.turn(t, run.ctx, "turn 1")
	store.failGetErr = errors.New("simulated db read failure")
	run.turn(t, run.ctx, "turn 2")
	store.failGetErr = nil
	run.turn(t, run.ctx, "turn 3")

	reqs := run.api.requests(t)
	if len(reqs) != 5 {
		t.Fatalf("chat-loop requests = %d, want 5 (load, answer, the call, answer, next turn)", len(reqs))
	}
	unread := reqs[2].Messages
	if string(unread[len(unread)-2]) != `{"role":"user","content":"turn 2"}` ||
		!slices.Equal(wireChanges(unread[len(unread)-1]), []string{"-p__other"}) {
		t.Errorf("the request without a read must end with its user message and the withdrawal, ends with %s", unread[len(unread)-2:])
	}
	answer := reqs[3].Messages[len(reqs[3].Messages)-1]
	if toolResultID(answer) != "toolu_o" || !strings.Contains(string(answer), "was NOT run: it was not loaded") {
		t.Errorf("the call to the withdrawn tool must be answered as unloaded, got %s", answer)
	}
	if got, want := storedRecords(t, run.sessions), []string{"tool toolu_l +p__other"}; !slices.Equal(got, want) {
		t.Errorf("stored records = %q, want %q", got, want)
	}
	for _, m := range reqs[4].Messages {
		if strings.Contains(string(m), `"tool_removal"`) {
			t.Errorf("the next turn with a readable state carries a withdrawal: %s", m)
		}
	}
	if !slices.Contains(run.llm.callable[4], "p__other") {
		t.Errorf("the next turn must offer p__other again, offers %v", run.llm.callable[4])
	}
	checkWireAnnouncedIsCallable(t, reqs, run.llm.callable)
}

func TestAnthropicInlineToolsAnnouncementMovesBehindAFollowingUserMessage(t *testing.T) {
	exec := &countingExecutor{}
	registry := inlineRegistry(t, []Action{
		{Name: "always", Description: "Always-include tool.", AlwaysInclude: true, ReadOnly: true},
		{Name: "delete", Description: "Delete an item.", Parameters: []Parameter{{Name: "id", Description: "Item id.", Required: true}}},
	}, exec)
	if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
		t.Fatal(err)
	}
	run := newInlineRun(t, registry, OrchestratorOpts{ConfirmationPlugin: "conf", ConfirmationAction: "check"},
		`{"type":"tool_use","id":"toolu_l","name":"_meta__load_tools","input":{"names":"p__delete"}}`,
		`{"type":"tool_use","id":"toolu_d","name":"p__delete","input":{"id":"7"}}`,
	)
	run.turn(t, run.ctx, "Delete item 7.")
	const note = "The export finished."
	if err := run.orch.StoreNote(context.Background(), "s1", note, reserveNote(t, run.sessions.(noteSessions), "s1", time.Now())); err != nil {
		t.Fatal(err)
	}
	run.turn(t, actor.WithConfirmationDecision(run.ctx, "approve"), "approve")

	reqs := run.api.requests(t)
	if len(reqs) != 3 || exec.count != 1 {
		t.Fatalf("want 3 chat-loop requests and the approved delete run once, got %d requests and %d runs", len(reqs), exec.count)
	}
	before, after := reqs[1].Messages, reqs[2].Messages
	if len(before) != 4 || !slices.Equal(wireChanges(before[3]), []string{"+p__delete"}) || toolResultID(before[2]) != "toolu_l" {
		t.Fatalf("the request before the confirmation must end with the load's result and its announcement, got %s", before)
	}
	want := append(slices.Clone(before[:3]), json.RawMessage(`{"role":"user","content":"`+note+`"}`), before[3])
	if len(after) < len(want) {
		t.Fatalf("the request after the approval has %d messages, want at least %d", len(after), len(want))
	}
	for j, m := range want {
		if !bytes.Equal(after[j], m) {
			t.Errorf("message %d after the approval:\n got %s\nwant %s", j, after[j], m)
		}
	}
	if !bytes.Equal(after[len(want)-1], before[3]) {
		t.Errorf("the announcement must keep its bytes behind the note")
	}
	checkWireAnnouncedIsCallable(t, reqs, run.llm.callable)
}

func TestAnthropicInlineToolsKeepTheSentRecordOfAMessageARequestEndsOnAgain(t *testing.T) {
	load := func(id, names string) string {
		return `{"type":"tool_use","id":"` + id + `","name":"_meta__load_tools","input":{"names":"` + names + `"}}`
	}
	lost := func(id string, withResult bool) func(provider.Message) bool {
		return func(m provider.Message) bool {
			return (len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == id) || (withResult && m.Role == provider.RoleTool && m.ToolCallID == id)
		}
	}
	capped := []Action{{Name: "delete", Description: "Delete an item.", Parameters: []Parameter{{Name: "id", Description: "Item id.", Required: true}}}}
	names := []string{"p__delete"}
	for i := range maxStickyTools + 5 {
		capped = append(capped, Action{Name: fmt.Sprintf("t%02d", i), Description: "Capped tool.", ReadOnly: true})
		names = append(names, fmt.Sprintf("p__t%02d", i))
	}
	var withdrawn []string
	for _, name := range names[maxStickyTools:] {
		withdrawn = append(withdrawn, "-"+name)
	}
	loadsCatalog := "tool toolu_c +p__catalog +p__list +p__lookup"
	for _, tc := range []struct {
		name    string
		actions []Action
		lose    func(provider.Message) bool
		script  []string
		turns   []string
		record  string
		change  []string
	}{
		{"the assistant message of a later load is not stored", announceActions(), lost("toolu_o", false),
			[]string{load("toolu_c", "p__catalog"), load("toolu_o", "p__other")},
			[]string{"turn 1", "turn 2"}, loadsCatalog, []string{"+p__other"}},
		{"neither message of a later load is stored", announceActions(), lost("toolu_o", true),
			[]string{load("toolu_c", "p__catalog"), load("toolu_o", "p__other")},
			[]string{"turn 1", "turn 2"}, loadsCatalog, []string{"+p__other"}},
		{"neither message of an approved call is stored", capped, lost("toolu_d", true),
			[]string{load("toolu_l", strings.Join(names, ",")), `{"type":"tool_use","id":"toolu_d","name":"p__delete","input":{"id":"7"}}`},
			[]string{"turn 1", "approve", "turn 3"}, "tool toolu_l +" + strings.Join(names, " +"), withdrawn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := inlineRegistry(t, tc.actions, &countingExecutor{})
			if err := registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}); err != nil {
				t.Fatal(err)
			}
			run := newInlineRunOn(t, &lostCallStore{SessionStoreInterface: sqliteSessions(t), lose: tc.lose}, registry,
				OrchestratorOpts{ConfirmationPlugin: "conf", ConfirmationAction: "check"}, tc.script...)
			for _, message := range tc.turns {
				ctx := run.ctx
				if message == "approve" {
					ctx = actor.WithConfirmationDecision(ctx, "approve")
				}
				run.turn(t, ctx, message)
			}

			reqs := run.api.requests(t)
			if len(reqs) != 4 {
				t.Fatalf("chat-loop requests = %d, want 4", len(reqs))
			}
			sent, again, next := reqs[1].Messages, reqs[2].Messages, reqs[3].Messages
			checkStartsWith(t, "the request that ends again on the same message", reqs[1], reqs[2], true)
			if len(again) != len(sent)+1 || !slices.Equal(wireChanges(again[len(again)-1]), tc.change) {
				t.Errorf("the request that ends again must add exactly %v after the sent announcement, ends with %s", tc.change, again[max(0, len(again)-2):])
			}
			last := tc.turns[len(tc.turns)-1]
			if len(next) != len(sent)+3 || string(next[len(next)-2]) != `{"role":"user","content":"`+last+`"}` ||
				!slices.Equal(wireChanges(next[len(next)-1]), tc.change) {
				t.Errorf("the next request must end with the reply, %q and %v, got %d messages ending with %s",
					last, tc.change, len(next), next[max(0, len(next)-3):])
			}
			for j, m := range sent[:min(len(sent), len(next))] {
				if !bytes.Equal(next[j], m) {
					t.Errorf("message %d of the next request differs from the sent one:\n got %s\nwant %s", j, next[j], m)
				}
			}
			checkWireAnnouncedIsCallable(t, reqs, run.llm.callable)
			want := []string{tc.record, "user " + last + " " + strings.Join(tc.change, " ")}
			if got := storedRecords(t, run.sessions); !slices.Equal(got, want) {
				t.Errorf("stored records = %q, want %q", got, want)
			}
		})
	}
}
