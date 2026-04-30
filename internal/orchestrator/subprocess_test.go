package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

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

	return NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		Subprocess: SubprocessConfig{
			Enabled:        true,
			MaxDepth:       2,
			MaxIterations:  5,
			DefaultTimeout: 30 * time.Second,
		},
	})
}

func TestSubprocessBasicFork(t *testing.T) {
	// LLM response sequence:
	// 1. Parent calls _subprocess.run
	// 2. Subprocess LLM returns direct answer (no tool calls)
	llm := &fakeLLM{responses: []string{
		`[tool_call]
{"tool": "_subprocess.run", "args": {"task": "What is the capital of France?"}}
[/tool_call]`,
		// Subprocess LLM - direct answer
		"The capital of France is Paris.",
		// Parent LLM - final answer incorporating subprocess result
		"Based on my research: Paris is the capital of France.",
	}}

	orch := setupSubprocessOrchestrator(llm)
	sess := orch.sessions.Create("test-basic", "", "")

	result, err := orch.Run(context.Background(), sess.ID, "What is the capital of France?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Response, "Paris") {
		t.Errorf("expected response to contain 'Paris', got: %s", result.Response)
	}
}

func TestSubprocessWithToolCalls(t *testing.T) {
	// LLM sequence:
	// 1. Parent calls _subprocess.run with tools restriction
	// 2. Subprocess LLM calls search.query
	// 3. Subprocess LLM returns answer
	// 4. Parent LLM returns final answer
	llm := &fakeLLM{responses: []string{
		`[tool_call]
{"tool": "_subprocess.run", "args": {"task": "Search for refund policy", "tools": "search.query"}}
[/tool_call]`,
		// Subprocess LLM calls a tool
		`[tool_call]
{"tool": "search.query", "args": {"q": "refund policy"}}
[/tool_call]`,
		// Subprocess LLM returns answer after getting tool result
		"The refund policy allows returns within 30 days.",
		// Parent LLM final answer
		"Our refund policy allows returns within 30 days.",
	}}

	orch := setupSubprocessOrchestrator(llm)
	sess := orch.sessions.Create("test-tools", "", "")

	result, err := orch.Run(context.Background(), sess.ID, "What is our refund policy?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Response, "30 days") {
		t.Errorf("expected response about refund policy, got: %s", result.Response)
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
	llm := &fakeLLM{responses: make([]string, 20)}
	for i := range llm.responses {
		llm.responses[i] = fmt.Sprintf(`[tool_call]
{"tool": "search.query", "args": {"q": "attempt %d"}}
[/tool_call]`, i)
	}

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

func TestSubprocessToolAllowlist(t *testing.T) {
	// Subprocess tries to call math.calculate but only search.query is allowed.
	llm := &fakeLLM{responses: []string{
		// Subprocess calls disallowed tool
		`[tool_call]
{"tool": "math.calculate", "args": {"expr": "2+2"}}
[/tool_call]`,
		// Then gives up
		"I couldn't calculate that — the math tool is not available.",
	}}

	orch := setupSubprocessOrchestrator(llm)

	req := subprocessRequest{
		Task:         "calculate 2+2",
		AllowedTools: []string{"search.query"},
	}
	result, err := orch.runSubprocess(context.Background(), req, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Response == "" {
		t.Error("expected a response")
	}
}

func TestSubprocessBlocksRecursion(t *testing.T) {
	// Subprocess tries to call _subprocess.run — should be blocked.
	llm := &fakeLLM{responses: []string{
		`[tool_call]
{"tool": "_subprocess.run", "args": {"task": "recursive task"}}
[/tool_call]`,
		"I can't spawn subprocesses from here.",
	}}

	orch := setupSubprocessOrchestrator(llm)

	req := subprocessRequest{Task: "try to recurse"}
	result, err := orch.runSubprocess(context.Background(), req, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The subprocess should have gotten an error for the _subprocess call
	// and then returned a final answer.
	if result.Response == "" {
		t.Fatal("expected a non-empty response")
	}
	if result.Iterations < 1 {
		t.Errorf("expected at least 1 iteration, got %d", result.Iterations)
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
			args: map[string]string{"task": "search", "tools": "search.query, math.calculate"},
			check: func(t *testing.T, req subprocessRequest) {
				if len(req.AllowedTools) != 2 {
					t.Fatalf("expected 2 tools, got: %d", len(req.AllowedTools))
				}
				if req.AllowedTools[0] != "search.query" || req.AllowedTools[1] != "math.calculate" {
					t.Errorf("wrong tools: %v", req.AllowedTools)
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

func TestIsSubprocessToolAllowed(t *testing.T) {
	// _subprocess is always blocked
	var noTools []string
	if isSubprocessToolAllowed(ToolCall{Plugin: "_subprocess", Action: "run"}, noTools) {
		t.Error("_subprocess should always be blocked")
	}

	// No allowlist = everything except _subprocess
	if !isSubprocessToolAllowed(ToolCall{Plugin: "search", Action: "query"}, noTools) {
		t.Error("search.query should be allowed with no allowlist")
	}

	// With allowlist
	allowed := []string{"search.query"}
	if !isSubprocessToolAllowed(ToolCall{Plugin: "search", Action: "query"}, allowed) {
		t.Error("search.query should be in allowlist")
	}
	if isSubprocessToolAllowed(ToolCall{Plugin: "math", Action: "calculate"}, allowed) {
		t.Error("math.calculate should not be in allowlist")
	}
}

func TestSubprocessSystemPromptExcludesSubprocess(t *testing.T) {
	orch := setupSubprocessOrchestrator(&fakeLLM{})

	prompt := orch.buildSubprocessSystemPrompt(context.Background(), subprocessRequest{
		Task: "test",
	})

	if strings.Contains(prompt, "_subprocess") {
		t.Error("subprocess system prompt should not contain _subprocess")
	}
	if !strings.Contains(prompt, "search.query") {
		t.Error("subprocess system prompt should contain search.query")
	}
	if !strings.Contains(prompt, "math.calculate") {
		t.Error("subprocess system prompt should contain math.calculate")
	}
}

func TestSubprocessSystemPromptRespectsAllowlist(t *testing.T) {
	orch := setupSubprocessOrchestrator(&fakeLLM{})

	prompt := orch.buildSubprocessSystemPrompt(context.Background(), subprocessRequest{
		Task:         "test",
		AllowedTools: []string{"search.query"},
	})

	if !strings.Contains(prompt, "search.query") {
		t.Error("subprocess system prompt should contain search.query")
	}
	if strings.Contains(prompt, "math.calculate") {
		t.Error("subprocess system prompt should not contain math.calculate when not in allowlist")
	}
}

func TestSubprocessMaxIterationsCapped(t *testing.T) {
	// Request max_iterations=20, should be capped at 10
	llm := &fakeLLM{responses: make([]string, 15)}
	for i := range llm.responses {
		llm.responses[i] = fmt.Sprintf(`[tool_call]
{"tool": "search.query", "args": {"q": "attempt %d"}}
[/tool_call]`, i)
	}

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
