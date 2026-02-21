package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

type fakeLLM struct {
	responses []string
	callCount int
}

func (f *fakeLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if f.callCount >= len(f.responses) {
		return nil, fmt.Errorf("no more responses")
	}
	resp := f.responses[f.callCount]
	f.callCount++
	return &provider.CompletionResponse{Content: resp}, nil
}

type fakeParser struct {
	parseFn func(response string) []ToolCall
}

func (p *fakeParser) Parse(response string) []ToolCall {
	return p.parseFn(response)
}

type echoExecutor struct{}

func (e *echoExecutor) Execute(call ToolCall) ToolResult {
	return ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("executed %s.%s", call.Plugin, call.Action),
	}
}

func setupOrchestrator(llm LLMClient, parser ToolCallParser) (*Orchestrator, string) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:        "gitlab",
		Description: "GitLab integration",
		Actions: []Action{
			{Name: "analyze_code", Description: "Analyze code"},
			{Name: "create_pr", Description: "Create PR"},
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:        "jira",
		Description: "Jira integration",
		Actions:     []Action{{Name: "create_issue", Description: "Create issue"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("test-session")

	orch := New(llm, parser, registry, memory, sessions)
	return orch, "test-session"
}

func TestOrchestratorDirectAnswer(t *testing.T) {
	llm := &fakeLLM{responses: []string{"Hello! How can I help?"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	orch, sessID := setupOrchestrator(llm, parser)
	result, err := orch.Run(context.Background(), sessID, "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "Hello! How can I help?" {
		t.Errorf("Response = %q", result.Response)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
	}
}

func TestOrchestratorSingleToolCall(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.analyze_code repo=myrepo",
		"The code looks good!",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "call_1", Plugin: "gitlab", Action: "analyze_code", Args: map[string]string{"repo": "myrepo"}}}
		}
		return nil
	}}

	orch, sessID := setupOrchestrator(llm, parser)
	result, err := orch.Run(context.Background(), sessID, "Analyze my code")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "The code looks good!" {
		t.Errorf("Response = %q", result.Response)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Plugin != "gitlab" {
		t.Errorf("Plugin = %q", result.ToolCalls[0].Plugin)
	}
	if result.Results[0].Content != "executed gitlab.analyze_code" {
		t.Errorf("Result = %q", result.Results[0].Content)
	}
}

func TestOrchestratorMultiStepWorkflow(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.analyze_code",
		"[tool] jira.create_issue",
		"[tool] gitlab.create_pr",
		"Done! I analyzed the code, created a Jira issue, and opened a PR.",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		switch callNum {
		case 1:
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "analyze_code"}}
		case 2:
			return []ToolCall{{ID: "c2", Plugin: "jira", Action: "create_issue"}}
		case 3:
			return []ToolCall{{ID: "c3", Plugin: "gitlab", Action: "create_pr"}}
		default:
			return nil
		}
	}}

	orch, sessID := setupOrchestrator(llm, parser)
	result, err := orch.Run(context.Background(), sessID, "analyze code, create issue, create PR")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolCalls) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(result.ToolCalls))
	}
	if !strings.Contains(result.Response, "Done") {
		t.Errorf("unexpected response: %q", result.Response)
	}
}

func TestOrchestratorWorkflowMemory(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.analyze_code",
		"[tool] jira.create_issue",
		"All done.",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		switch callNum {
		case 1:
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "analyze_code"}}
		case 2:
			return []ToolCall{{ID: "c2", Plugin: "jira", Action: "create_issue"}}
		default:
			return nil
		}
	}}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "jira", Description: "Jira",
		Actions: []Action{{Name: "create_issue", Description: "Create issue"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")

	orch := New(llm, parser, registry, memory, sessions)
	_, err := orch.Run(context.Background(), "s1", "analyze and create issue")
	if err != nil {
		t.Fatal(err)
	}

	workflows := memory.SearchByTag("workflow")
	if len(workflows) != 1 {
		t.Fatalf("expected 1 workflow memory, got %d", len(workflows))
	}
	if !strings.Contains(workflows[0].Content, "gitlab") {
		t.Error("workflow should mention gitlab")
	}
	if !strings.Contains(workflows[0].Content, "jira") {
		t.Error("workflow should mention jira")
	}
}

func TestOrchestratorUnknownPlugin(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] unknown.do_thing",
		"Sorry, that plugin is not available.",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "c1", Plugin: "unknown", Action: "do_thing"}}
		}
		return nil
	}}

	orch, sessID := setupOrchestrator(llm, parser)
	result, err := orch.Run(context.Background(), sessID, "do something")
	if err != nil {
		t.Fatal(err)
	}
	if result.Results[0].Error == "" {
		t.Error("expected error for unknown plugin")
	}
}

func TestOrchestratorUnknownAction(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.delete_everything",
		"Sorry, that action is not available.",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "delete_everything"}}
		}
		return nil
	}}

	orch, sessID := setupOrchestrator(llm, parser)
	result, err := orch.Run(context.Background(), sessID, "delete everything")
	if err != nil {
		t.Fatal(err)
	}
	if result.Results[0].Error == "" {
		t.Error("expected error for unknown action")
	}
}

func TestOrchestratorSessionNotFound(t *testing.T) {
	llm := &fakeLLM{responses: []string{"hi"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	orch, _ := setupOrchestrator(llm, parser)
	_, err := orch.Run(context.Background(), "nonexistent", "hi")
	if err == nil {
		t.Error("expected error for nonexistent session")
	}
}

func TestOrchestratorNoWorkflowForSingleCall(t *testing.T) {
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.analyze_code",
		"Done.",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(response string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "analyze_code"}}
		}
		return nil
	}}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")

	orch := New(llm, parser, registry, memory, sessions)
	_, err := orch.Run(context.Background(), "s1", "analyze code")
	if err != nil {
		t.Fatal(err)
	}

	workflows := memory.SearchByTag("workflow")
	if len(workflows) != 0 {
		t.Errorf("expected no workflow for single tool call, got %d", len(workflows))
	}
}

func TestOrchestratorMaxIterationsExceeded(t *testing.T) {
	llm := &fakeLLM{responses: make([]string, 25)}
	for i := range llm.responses {
		llm.responses[i] = "[tool] gitlab.analyze_code"
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall {
		return []ToolCall{{ID: "cx", Plugin: "gitlab", Action: "analyze_code"}}
	}}

	orch, sessID := setupOrchestrator(llm, parser)
	_, err := orch.Run(context.Background(), sessID, "infinite loop")
	if err == nil {
		t.Fatal("expected error for max iterations")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error = %q, should mention exceeded", err.Error())
	}
}

func TestOrchestratorLLMFailure(t *testing.T) {
	llm := &fakeLLM{responses: nil}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	orch, sessID := setupOrchestrator(llm, parser)
	_, err := orch.Run(context.Background(), sessID, "hi")
	if err == nil {
		t.Fatal("expected error when LLM has no responses")
	}
}

func TestOrchestratorSessionHistoryGrows(t *testing.T) {
	llm := &fakeLLM{responses: []string{"Hello!"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")

	orch := New(llm, parser, registry, memory, sessions)
	_, _ = orch.Run(context.Background(), "s1", "Hi")

	sess, _ := sessions.Get("s1")
	if len(sess.Messages) != 2 {
		t.Errorf("expected 2 messages (user+assistant), got %d", len(sess.Messages))
	}
}

func TestFormatToolCallMessageNoArgs(t *testing.T) {
	call := ToolCall{ID: "1", Plugin: "gitlab", Action: "list_repos"}
	got := formatToolCallMessage(call)
	want := "[tool_call] gitlab.list_repos"
	if got != want {
		t.Errorf("formatToolCallMessage = %q, want %q", got, want)
	}
}

func TestFormatToolCallMessageWithArgs(t *testing.T) {
	call := ToolCall{ID: "1", Plugin: "gitlab", Action: "analyze", Args: map[string]string{"repo": "myrepo"}}
	got := formatToolCallMessage(call)
	if !strings.Contains(got, "repo=myrepo") {
		t.Errorf("formatToolCallMessage = %q, should contain repo=myrepo", got)
	}
}

func TestFormatToolResultMessageSuccess(t *testing.T) {
	result := ToolResult{CallID: "1", Content: "all good"}
	got := formatToolResultMessage(result)
	if got != "[tool_result] all good" {
		t.Errorf("formatToolResultMessage = %q", got)
	}
}

func TestFormatToolResultMessageError(t *testing.T) {
	result := ToolResult{CallID: "1", Error: "not found"}
	got := formatToolResultMessage(result)
	if got != "[tool_result] error: not found" {
		t.Errorf("formatToolResultMessage = %q", got)
	}
}

func TestFilterByTag(t *testing.T) {
	memories := []*state.Memory{
		{ID: "m1", Content: "wf1", Tags: []string{"workflow"}},
		{ID: "m2", Content: "fact1", Tags: []string{"fact"}},
		{ID: "m3", Content: "wf2", Tags: []string{"workflow", "important"}},
	}
	got := filterByTag(memories, "workflow")
	if len(got) != 2 {
		t.Errorf("expected 2 workflows, got %d", len(got))
	}
}

func TestFilterByTagNone(t *testing.T) {
	memories := []*state.Memory{
		{ID: "m1", Content: "fact", Tags: []string{"fact"}},
	}
	got := filterByTag(memories, "workflow")
	if len(got) != 0 {
		t.Errorf("expected 0, got %d", len(got))
	}
}
