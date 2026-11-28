package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// nativeLLM gives a test model native tool calling, the only path a sub-agent
// runs on, so a fake written for another test can drive one.
type nativeLLM struct{ LLMClient }

func (nativeLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// subagentLLM scripts a parent agent loop and its sub-agents apart. A request
// whose system message is the sub-agent preamble is a sub-agent's: it is
// recorded and gets the next reply scripted for its task (its first user
// message). Every other request is the parent's and gets the next parent
// reply. Either answers "done" once its script is used up. Parallel sub-agents
// call it concurrently.
type subagentLLM struct {
	mu       sync.Mutex
	parent   []provider.CompletionResponse
	children map[string][]provider.CompletionResponse
	requests []provider.CompletionRequest // the sub-agents' requests, in arrival order
}

func (l *subagentLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(req.Messages) < 2 || req.Messages[0].Role != provider.RoleSystem ||
		!strings.HasPrefix(req.Messages[0].Content, prompts.SubprocessPreamble) {
		return popReply(&l.parent), nil
	}
	rec := *req
	rec.Messages = slices.Clone(req.Messages)
	l.requests = append(l.requests, rec)
	task := req.Messages[1].Content
	script := l.children[task]
	reply := popReply(&script)
	if l.children != nil {
		l.children[task] = script
	}
	return reply, nil
}

func (*subagentLLM) SupportsFeature(f provider.Feature) bool { return f == provider.FeatureTools }

// popReply takes the first reply off script, or answers "done" when it is empty.
func popReply(script *[]provider.CompletionResponse) *provider.CompletionResponse {
	if len(*script) == 0 {
		return &provider.CompletionResponse{Content: "done"}
	}
	r := (*script)[0]
	*script = (*script)[1:]
	return &r
}

func callReply(id, name string, args map[string]string) provider.CompletionResponse {
	return provider.CompletionResponse{ToolCalls: []provider.ToolCall{{ID: id, Name: name, Arguments: args}}}
}

// searchCalls is n replies that each call search__query again.
func searchCalls(n int) []provider.CompletionResponse {
	replies := make([]provider.CompletionResponse, n)
	for i := range replies {
		replies[i] = callReply(fmt.Sprintf("c%d", i+1), "search__query", map[string]string{"q": fmt.Sprintf("attempt %d", i+1)})
	}
	return replies
}

func toolNames(req provider.CompletionRequest) []string {
	names := make([]string, len(req.Tools))
	for i, td := range req.Tools {
		names[i] = td.Name
	}
	return names
}

func (l *subagentLLM) assertTools(t *testing.T, requests int, want ...string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, req := range l.requests {
		if got := toolNames(req); !slices.Equal(got, want) {
			t.Errorf("sub-agent request %d offered %v, want %v", i, got, want)
		}
	}
	if len(l.requests) != requests {
		t.Fatalf("sub-agent requests = %d, want %d", len(l.requests), requests)
	}
}

// subprocessResultOf returns the content of the turn's one _subprocess call result.
func subprocessResultOf(t *testing.T, result *RunResult) string {
	t.Helper()
	for i, call := range result.ToolCalls {
		if call.Plugin == "_subprocess" {
			if result.Results[i].Error != "" {
				t.Fatalf("_subprocess call failed: %s", result.Results[i].Error)
			}
			return result.Results[i].Content
		}
	}
	t.Fatalf("no _subprocess call in the turn: %+v", result.ToolCalls)
	return ""
}

func setupSubprocessOrchestrator(llm LLMClient) *Orchestrator {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:        "search",
		Description: "Search tools",
		Actions: []Action{
			{Name: "query", Description: "Search for something", Parameters: []Parameter{
				{Name: "q", Description: "Search query", Required: true},
			}},
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:        "math",
		Description: "Math tools",
		Actions: []Action{
			{Name: "calculate", Description: "Calculate expression", Parameters: []Parameter{
				{Name: "expr", Description: "Expression", Required: true},
			}},
		},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")

	return NewWithRules(nativeLLM{llm}, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{
			Enabled:        true,
			MaxDepth:       2,
			MaxIterations:  5,
			DefaultTimeout: 30 * time.Second,
		},
	})
}

func TestSubprocessBasicFork(t *testing.T) {
	// The parent loads _subprocess__run and calls it; the sub-agent answers
	// directly; the parent answers from the sub-agent's result.
	const task = "What is the capital of France?"
	llm := &subagentLLM{
		parent: []provider.CompletionResponse{
			callReply("p1", toolFQN(metaPluginName, metaLoadTools), map[string]string{"names": "_subprocess__run"}),
			callReply("p2", "_subprocess__run", map[string]string{"task": task}),
			{Content: "Based on my research: Paris is the capital of France."},
		},
		children: map[string][]provider.CompletionResponse{task: {{Content: "The capital of France is Paris."}}},
	}

	orch := setupSubprocessOrchestrator(llm)
	sess := orch.sessions.Create(state.SessionParams{ID: "test-basic"})

	result, err := orch.Run(context.Background(), sess.ID, task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := subprocessResultOf(t, result); got != "The capital of France is Paris." {
		t.Errorf("_subprocess result = %q, want the sub-agent's answer", got)
	}
	if len(llm.requests) != 1 {
		t.Errorf("sub-agent requests = %d, want 1", len(llm.requests))
	}
}

func TestSubprocess_ClearsInheritedNativeSentSet(t *testing.T) {
	// A subprocess derives its ctx from the caller, which — on a native parent
	// round — carries the parent's sent native tool set. The subprocess sends
	// its own tools and dispatches child calls as FromLLM, so the tool-load gate
	// must NOT enforce the parent's set against the subprocess's tools.
	// Regression guard: a catalog tool the parent never promoted must still run
	// inside the subprocess, not be answered with a load on call.
	exec := &countingExecutor{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "search",
		Actions: []Action{{Name: "query", Description: "Search", Parameters: []Parameter{
			{Name: "q", Description: "query", Required: true},
		}}},
	}, exec)
	sessions := state.NewSessionStore("")
	llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"search it": {
		callReply("c1", "search__query", map[string]string{"q": "x"}),
		{Content: "found it"},
	}}}
	orch := NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), sessions,
		OrchestratorOpts{Subprocess: SubprocessConfig{Enabled: true, MaxDepth: 2}})

	// Simulate a native parent round whose sent set does NOT contain
	// search__query (a catalog tool the parent never loaded).
	ctx := withSentNativeTools(context.Background(), map[string]struct{}{"_subprocess__run": {}})
	req := subprocessRequest{Task: "search it", AllowedTools: []string{"search__query"}}

	res, err := orch.runSubprocess(ctx, req, 1)
	if err != nil {
		t.Fatalf("runSubprocess: %v", err)
	}
	if exec.count != 1 {
		t.Errorf("subprocess tool must run despite the parent sent set not containing it, ran %d time(s) (resp=%q)", exec.count, res.Response)
	}
}

// A sub-agent's structured calls run, and its messages keep each call and its
// result in the main loop's native shapes: an assistant message with the call
// (the response's text and thinking on the first only) and a tool-role result.
func TestSubprocessWithToolCalls(t *testing.T) {
	const task = "Search for refund policy"
	thinking := []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"look it up"}`)}
	llm := &subagentLLM{
		parent: []provider.CompletionResponse{
			callReply("p1", toolFQN(metaPluginName, metaLoadTools), map[string]string{"names": "_subprocess__run"}),
			callReply("p2", "_subprocess__run", map[string]string{"task": task, "tools": "search__query"}),
			{Content: "Our refund policy allows returns within 30 days."},
		},
		children: map[string][]provider.CompletionResponse{task: {
			{Content: "Looking it up.", Thinking: thinking, ThinkingFingerprint: "fp-1", ToolCalls: []provider.ToolCall{
				{ID: "c1", Name: "search__query", Arguments: map[string]string{"q": "refund policy"}},
				{ID: "c2", Name: "search__query", Arguments: map[string]string{"q": "returns"}},
			}},
			{Content: "The refund policy allows returns within 30 days."},
		}},
	}

	orch := setupSubprocessOrchestrator(llm)
	sess := orch.sessions.Create(state.SessionParams{ID: "test-tools"})

	result, err := orch.Run(context.Background(), sess.ID, "What is our refund policy?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := subprocessResultOf(t, result); got != "The refund policy allows returns within 30 days." {
		t.Errorf("_subprocess result = %q, want the sub-agent's answer", got)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("sub-agent requests = %d, want 2", len(llm.requests))
	}
	want := []provider.Message{
		{Role: provider.RoleAssistant, Content: "Looking it up.", Thinking: thinking, ThinkingFingerprint: "fp-1",
			ToolCalls: []provider.ToolCall{{ID: "c1", Name: "search__query", Arguments: map[string]string{"q": "refund policy"}}}},
		{Role: provider.RoleTool, Content: "executed search.query", ToolCallID: "c1"},
		{Role: provider.RoleAssistant,
			ToolCalls: []provider.ToolCall{{ID: "c2", Name: "search__query", Arguments: map[string]string{"q": "returns"}}}},
		{Role: provider.RoleTool, Content: "executed search.query", ToolCallID: "c2"},
	}
	if got := llm.requests[1].Messages[2:]; !reflect.DeepEqual(got, want) {
		t.Errorf("sub-agent messages after the task =\n%+v\nwant\n%+v", got, want)
	}
	for i, req := range llm.requests {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "[tool_call]") || strings.Contains(m.Content, "[plugin_output]") {
				t.Errorf("request %d carries text protocol: %q", i, m.Content)
			}
		}
	}
}

func TestSubprocessDepthLimit(t *testing.T) {
	// Subprocess executor checks depth. At depth >= MaxDepth, it returns an error.
	orch := setupSubprocessOrchestrator(&fakeLLM{})

	exec := &subprocessExecutor{orch: orch}
	ctx := withSubprocessDepth(context.Background(), 2)
	tr := exec.Execute(ctx, ToolCall{ID: "1", Args: map[string]string{"task": "nested task"}})
	if tr.Error == "" {
		t.Fatal("expected depth limit error")
	}
	if !strings.Contains(tr.Error, "depth limit") {
		t.Errorf("expected depth limit error, got: %s", tr.Error)
	}
}

func TestSubprocessIterationLimit(t *testing.T) {
	// Subprocess LLM always returns tool calls, never a final answer.
	llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"keep searching": searchCalls(20)}}

	orch := setupSubprocessOrchestrator(llm)
	orch.subprocessConfig.MaxIterations = 3

	req := subprocessRequest{Task: "keep searching", MaxIterations: 3}
	result, err := orch.runSubprocess(context.Background(), req, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Response, "iteration limit") {
		t.Errorf("expected iteration limit message, got: %s", result.Response)
	}
	if result.Iterations != 3 {
		t.Errorf("expected 3 iterations, got: %d", result.Iterations)
	}
}

// Every request of a sub-agent offers exactly its permitted tools: those the
// caller may call, without _subprocess, the _meta plugin, preparer, guard and
// user-only actions. Its prompt lists none.
func TestSubprocessRequestOffersThePermittedTools(t *testing.T) {
	registry := NewToolRegistry()
	query := []Parameter{{Name: "q", Description: "Search query", Required: true}}
	_ = registry.Register(PluginCapability{Name: "search", Actions: []Action{{Name: "query", Description: "Search", Parameters: query}}}, &echoExecutor{})
	_ = registry.Register(PluginCapability{Name: "math", Actions: []Action{{Name: "calculate", Description: "Calculate"}}}, &echoExecutor{})
	_ = registry.Register(PluginCapability{Name: "store", Actions: []Action{
		{Name: "inspect", Description: "Inspect"},
		{Name: "purge", Description: "Purge", UserOnly: true},
	}}, &echoExecutor{})
	_ = registry.Register(PluginCapability{Name: "prep", Actions: []Action{{Name: "enrich"}}}, &prefixingExecutor{})
	_ = registry.Register(PluginCapability{Name: "guard", Actions: []Action{{Name: "scan"}}}, &prefixingExecutor{})
	llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"look": {
		callReply("c1", "search__query", map[string]string{"q": "x"}),
		{Content: "found"},
	}}}
	orch := NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), state.NewSessionStore(""), OrchestratorOpts{
		ContentPreparers: []ContentPreparerEntry{{Plugin: "prep", Action: "enrich"}, {Plugin: "guard", Action: "scan", Guard: true}},
		Subprocess:       SubprocessConfig{Enabled: true},
	})

	res := (&subprocessExecutor{orch: orch}).Execute(context.Background(),
		ToolCall{ID: "o1", Plugin: "_subprocess", Action: "run", Args: map[string]string{"task": "look"}})
	if res.Error != "" || res.Content != "found" {
		t.Fatalf("result = %+v, want the sub-agent's answer", res)
	}
	llm.assertTools(t, 2, "math__calculate", "search__query", "store__inspect")
	for i, req := range llm.requests {
		if got := req.Messages[0].Content; got != prompts.SubprocessPreamble {
			t.Errorf("request %d system prompt = %q, want the preamble alone", i, got)
		}
	}
}

// A structured call outside the sub-agent's permitted set does not run: its
// result is an error naming the tool. The allowlist only narrows what the
// caller may use, and _subprocess is never callable inside a sub-agent.
func TestSubprocessToolAllowlist(t *testing.T) {
	for _, tt := range []struct {
		name      string
		plugins   []string // the caller's profile plugin list; nil = unrestricted
		tools     string
		call      string
		wantTools []string
	}{
		{name: "allowlist narrows the tools", tools: "search__query", call: "math__calculate",
			wantTools: []string{"search__query"}},
		{name: "allowlist adds no plugin the caller may not use", plugins: []string{"search"},
			tools: "search__query,math__calculate", call: "math__calculate", wantTools: []string{"search__query"}},
		{name: "_subprocess stays uncallable", call: "_subprocess__run",
			wantTools: []string{"math__calculate", "search__query"}},
		{name: "_subprocess stays uncallable when listed", tools: "_subprocess__run,search__query", call: "_subprocess__run",
			wantTools: []string{"search__query"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"t": {
				callReply("c1", tt.call, map[string]string{}),
				{Content: "gave up"},
			}}}
			orch := setupSubprocessOrchestrator(llm)
			ctx := profile.WithProfile(context.Background(), &profile.Profile{Plugins: tt.plugins})

			res := (&subprocessExecutor{orch: orch}).Execute(ctx,
				ToolCall{ID: "o1", Plugin: "_subprocess", Action: "run", Args: map[string]string{"task": "t", "tools": tt.tools}})
			if res.Error != "" || res.Content != "gave up" {
				t.Fatalf("result = %+v, want the sub-agent's answer", res)
			}
			llm.assertTools(t, 2, tt.wantTools...)
			want := provider.Message{Role: provider.RoleTool, ToolCallID: "c1",
				Content: "error: tool " + tt.call + " not allowed in this subprocess"}
			if got := llm.requests[1].Messages[3]; !reflect.DeepEqual(got, want) {
				t.Errorf("result of the refused call = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSubprocessTimeout(t *testing.T) {
	// Use a very short timeout and a slow LLM.
	llm := &slowLLM{delay: 2 * time.Second}

	orch := setupSubprocessOrchestrator(llm)
	orch.subprocessConfig.DefaultTimeout = 50 * time.Millisecond

	req := subprocessRequest{Task: "slow task"}
	_, err := orch.runSubprocess(context.Background(), req, 1)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") && !strings.Contains(err.Error(), "subprocess LLM") {
		t.Errorf("expected context deadline error, got: %v", err)
	}
}

type slowLLM struct {
	delay time.Duration
}

func (s *slowLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return &provider.CompletionResponse{Content: "done"}, nil
	}
}

func TestSubprocessDisabled(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")

	orch := NewWithRules(&fakeLLM{}, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{Enabled: false},
	})

	if _, ok := orch.registry.GetExecutor("_subprocess"); ok {
		t.Error("expected _subprocess to not be registered when disabled")
	}
}

func TestSubprocessEnabled(t *testing.T) {
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")

	orch := NewWithRules(&fakeLLM{}, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{Enabled: true},
	})

	if _, ok := orch.registry.GetExecutor("_subprocess"); !ok {
		t.Error("expected _subprocess to be registered when enabled")
	}
}

// Without native tool calling a sub-agent call fails at once and no request
// reaches the model, for run and parallel alike.
func TestSubprocessNeedsNativeToolCalling(t *testing.T) {
	for _, call := range []ToolCall{
		{ID: "o1", Plugin: "_subprocess", Action: "run", Args: map[string]string{"task": "t"}},
		{ID: "o2", Plugin: "_subprocess", Action: "parallel", Args: map[string]string{"tasks": `[{"task":"a"},{"task":"b"}]`}},
	} {
		t.Run(call.Action, func(t *testing.T) {
			llm := &modelLLM{}
			orch := NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
				OrchestratorOpts{Subprocess: SubprocessConfig{Enabled: true}})

			res := (&subprocessExecutor{orch: orch}).Execute(context.Background(), call)
			if !strings.Contains(res.Error, "sub-agents need native tool calling") {
				t.Errorf("result = %+v, want an error saying sub-agents need native tool calling", res)
			}
			if len(llm.models) != 0 {
				t.Errorf("the model received %d request(s), want none", len(llm.models))
			}
		})
	}
}

func TestParseSubprocessRequest(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]string
		wantErr bool
		check   func(t *testing.T, req subprocessRequest)
	}{
		{
			name:    "missing task",
			args:    map[string]string{},
			wantErr: true,
		},
		{
			name: "task only",
			args: map[string]string{"task": "do something"},
			check: func(t *testing.T, req subprocessRequest) {
				if req.Task != "do something" {
					t.Errorf("wrong task: %s", req.Task)
				}
				if len(req.AllowedTools) != 0 {
					t.Errorf("expected no allowed tools, got: %v", req.AllowedTools)
				}
			},
		},
		{
			name: "with tools",
			args: map[string]string{"task": "search", "tools": "search__query, math__calculate"},
			check: func(t *testing.T, req subprocessRequest) {
				if len(req.AllowedTools) != 2 {
					t.Fatalf("expected 2 tools, got: %d", len(req.AllowedTools))
				}
				if req.AllowedTools[0] != "search__query" || req.AllowedTools[1] != "math__calculate" {
					t.Errorf("wrong tools: %v", req.AllowedTools)
				}
			},
		},
		{
			name: "no tools",
			args: map[string]string{"task": "judge this", "tools": "none"},
			check: func(t *testing.T, req subprocessRequest) {
				if !req.NoTools {
					t.Error("tools=none should set NoTools")
				}
				if len(req.AllowedTools) != 0 {
					t.Errorf("tools=none should leave AllowedTools empty, got: %v", req.AllowedTools)
				}
			},
		},
		{
			name: "no tools is case-insensitive",
			args: map[string]string{"task": "judge this", "tools": "NONE"},
			check: func(t *testing.T, req subprocessRequest) {
				if !req.NoTools {
					t.Error(`tools="NONE" should set NoTools (case-insensitive)`)
				}
			},
		},
		{
			name: "with max_iterations",
			args: map[string]string{"task": "search", "max_iterations": "3"},
			check: func(t *testing.T, req subprocessRequest) {
				if req.MaxIterations != 3 {
					t.Errorf("expected max_iterations=3, got: %d", req.MaxIterations)
				}
			},
		},
		{
			name:    "invalid max_iterations",
			args:    map[string]string{"task": "search", "max_iterations": "abc"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := parseSubprocessRequest(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, req)
			}
		})
	}
}

func TestSubprocessMaxIterationsCapped(t *testing.T) {
	// Request max_iterations=20, should be capped at 10
	llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"keep going": searchCalls(15)}}

	orch := setupSubprocessOrchestrator(llm)

	req := subprocessRequest{Task: "keep going", MaxIterations: 20}
	result, err := orch.runSubprocess(context.Background(), req, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be capped at 10
	if result.Iterations != 10 {
		t.Errorf("expected 10 iterations (capped), got: %d", result.Iterations)
	}
}

// A no-tools sub-agent is offered no tools and is pinned to a single
// iteration, so an adversarial task that keeps emitting (refused) tool calls
// can't burn several model calls — the determinism/budget guarantee a caller
// like llm_review relies on.
func TestSubprocessNoToolsPinsSingleIteration(t *testing.T) {
	llm := &subagentLLM{children: map[string][]provider.CompletionResponse{"judge": searchCalls(5)}}

	orch := setupSubprocessOrchestrator(llm)

	req := subprocessRequest{Task: "judge", NoTools: true, MaxIterations: 20}
	result, err := orch.runSubprocess(context.Background(), req, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Iterations != 1 {
		t.Errorf("NoTools must pin to a single iteration regardless of max_iterations, got: %d", result.Iterations)
	}
	if len(llm.requests) != 1 || len(llm.requests[0].Tools) != 0 {
		t.Fatalf("want one request without tools, got %d request(s): %+v", len(llm.requests), llm.requests)
	}
	if prompt := llm.requests[0].Messages[0].Content; !strings.Contains(prompt, "no tools") {
		t.Errorf("NoTools prompt should tell the sub-agent it has no tools, got:\n%s", prompt)
	}
}

// modelLLM answers every call and records which model each request asked for.
type modelLLM struct {
	mu     sync.Mutex
	models []string
}

func (m *modelLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	m.mu.Lock()
	m.models = append(m.models, req.Model)
	m.mu.Unlock()
	return &provider.CompletionResponse{Content: "ok"}, nil
}

// A host-side caller (e.g. talooner's llm_review via RunAction) picks the
// sub-agent's chat model per call; it must be a configured one, and no model
// means the default.
func TestSubprocessModel(t *testing.T) {
	configured := []string{"claude-haiku-4-5-20251001", "claude-opus-5-5"}
	for _, tt := range []struct {
		name       string
		action     string
		args       map[string]string
		models     []string // SubprocessConfig.Models; nil = configured
		wantModels []string // sorted
		wantErr    string
	}{
		{name: "configured model is used", args: map[string]string{"task": "t", "tools": "none", "model": "claude-haiku-4-5-20251001"},
			wantModels: []string{"claude-haiku-4-5-20251001"}},
		{name: "whitespace is trimmed", args: map[string]string{"task": "t", "tools": "none", "model": "  claude-opus-5-5 "},
			wantModels: []string{"claude-opus-5-5"}},
		{name: "no model means the default", args: map[string]string{"task": "t", "tools": "none"}, wantModels: []string{""}},
		{name: "unknown model is refused", args: map[string]string{"task": "t", "model": "gpt-9"}, wantErr: `unknown model "gpt-9"`},
		{name: "no configured list means no check", models: []string{}, args: map[string]string{"task": "t", "tools": "none", "model": "anything"},
			wantModels: []string{"anything"}},
		{name: "parallel gives each task its own model", action: "parallel",
			args:       map[string]string{"tasks": `[{"task":"a","tools":"none","model":"claude-opus-5-5"},{"task":"b","tools":"none","model":"claude-haiku-4-5-20251001"}]`},
			wantModels: []string{"claude-haiku-4-5-20251001", "claude-opus-5-5"}},
		{name: "parallel mixes explicit and default", action: "parallel",
			args:       map[string]string{"tasks": `[{"task":"a","tools":"none","model":"claude-opus-5-5"},{"task":"b","tools":"none"}]`},
			wantModels: []string{"", "claude-opus-5-5"}},
		{name: "parallel checks every task", action: "parallel",
			args: map[string]string{"tasks": `[{"task":"a","model":"claude-opus-5-5"},{"task":"b","model":"nope"}]`}, wantErr: `unknown model "nope"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			llm := &modelLLM{}
			orch := setupSubprocessOrchestrator(llm)
			orch.subprocessConfig.Models = configured
			if tt.models != nil {
				orch.subprocessConfig.Models = tt.models
			}
			action := tt.action
			if action == "" {
				action = "run"
			}
			_, err := orch.RunAction(context.Background(), "_subprocess", action, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if len(llm.models) != 0 {
					t.Errorf("model was called despite the refusal: %v", llm.models)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunAction: %v", err)
			}
			got := append([]string(nil), llm.models...)
			sort.Strings(got)
			if strings.Join(got, ",") != strings.Join(tt.wantModels, ",") {
				t.Errorf("requested models = %v, want %v", got, tt.wantModels)
			}
		})
	}
}

// The LLM is never offered `model`: an LLM-sourced call naming one is refused,
// so a prompt-injected task cannot fan out onto the most expensive model.
func TestSubprocessModelNotSelectableByLLM(t *testing.T) {
	orch := setupSubprocessOrchestrator(&modelLLM{})
	orch.subprocessConfig.Models = []string{"claude-opus-5-5"}
	if _, err := orch.resolveSubprocessModel(context.Background(), ToolCall{FromLLM: true}, "claude-opus-5-5"); err == nil {
		t.Error("an LLM-sourced model choice must be refused")
	}
	for _, p := range orch.registry.plugins["_subprocess"].Actions {
		for _, param := range p.Parameters {
			if param.Name == "model" {
				t.Errorf("_subprocess.%s advertises a model parameter to the LLM", p.Name)
			}
		}
	}
}

// A nested sub-agent with no explicit model inherits its parent's.
func TestSubprocessModelInherited(t *testing.T) {
	orch := setupSubprocessOrchestrator(&modelLLM{})
	ctx := context.WithValue(context.Background(), subprocessModelKey{}, "claude-haiku-4-5-20251001")
	got, err := orch.resolveSubprocessModel(ctx, ToolCall{FromLLM: true}, "")
	if err != nil || got != "claude-haiku-4-5-20251001" {
		t.Errorf("inherited model = %q, %v; want the parent's", got, err)
	}
}
