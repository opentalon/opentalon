package orchestrator

import (
	"context"
	"encoding/json"
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

// fixedResultExecutor returns fixed content (for preparers that return JSON).
type fixedResultExecutor struct {
	content string
}

func (e *fixedResultExecutor) Execute(call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, Content: e.content}
}

// previousResultExecutor returns args["previous_result"] so tests can assert it was passed.
type previousResultExecutor struct{}

func (e *previousResultExecutor) Execute(call ToolCall) ToolResult {
	prev := call.Args["previous_result"]
	if prev == "" {
		prev = "(none)"
	}
	return ToolResult{CallID: call.ID, Content: "got: " + prev}
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

// --- Invoke steps (preparer-driven plugin runs without LLM) ---

func TestRunInvokeStepsSingleStep(t *testing.T) {
	llm := &fakeLLM{responses: []string{"ignored"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	orch, _ := setupOrchestrator(llm, parser)
	steps := []InvokeStep{
		{Plugin: "gitlab", Action: "analyze_code", Args: map[string]string{"repo": "r1"}},
	}
	result, err := orch.runInvokeSteps(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "executed gitlab.analyze_code" {
		t.Errorf("Response = %q", result.Response)
	}
	if len(result.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	// LLM should not have been called
	if llm.callCount != 0 {
		t.Errorf("LLM should not be called for invoke steps, callCount = %d", llm.callCount)
	}
}

func TestRunInvokeStepsMultiStepPreviousResult(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "first", Description: "First",
		Actions: []Action{{Name: "run", Description: "Run"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "second", Description: "Second",
		Actions: []Action{{Name: "run", Description: "Run"}},
	}, &previousResultExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")
	orch := New(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions)

	steps := []InvokeStep{
		{Plugin: "first", Action: "run", Args: nil},
		{Plugin: "second", Action: "run", Args: nil},
	}
	result, err := orch.runInvokeSteps(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	// Second step receives previous_result = "executed first.run"
	if result.Response != "got: executed first.run" {
		t.Errorf("Response = %q (expected previous_result from first step)", result.Response)
	}
	if len(result.ToolCalls) != 2 {
		t.Errorf("expected 2 tool calls, got %d", len(result.ToolCalls))
	}
}

func TestRunInvokeStepsSkipsInvalidStep(t *testing.T) {
	llm := &fakeLLM{responses: []string{"ignored"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	orch, _ := setupOrchestrator(llm, parser)
	steps := []InvokeStep{
		{Plugin: "gitlab", Action: "analyze_code", Args: nil},
		{Plugin: "", Action: "missing", Args: nil},                 // skipped: no plugin
		{Plugin: "unknown", Action: "do_thing", Args: nil},         // skipped: unknown plugin
		{Plugin: "gitlab", Action: "delete_everything", Args: nil}, // skipped: unknown action
		{Plugin: "jira", Action: "create_issue", Args: nil},
	}
	result, err := orch.runInvokeSteps(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	// Only gitlab.analyze_code and jira.create_issue run
	if len(result.ToolCalls) != 2 {
		t.Errorf("expected 2 tool calls (invalid steps skipped), got %d", len(result.ToolCalls))
	}
	if result.Response != "executed jira.create_issue" {
		t.Errorf("Response = %q", result.Response)
	}
}

func TestRunInvokeStepsStopsOnError(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "fail", Description: "Fail",
		Actions: []Action{{Name: "run", Description: "Run"}},
	}, &errorReturningExecutor{err: "step failed"})
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")
	orch := New(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions)

	steps := []InvokeStep{
		{Plugin: "fail", Action: "run", Args: nil},
		{Plugin: "gitlab", Action: "analyze_code", Args: nil},
	}
	result, err := orch.runInvokeSteps(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Response, "Invoke step failed") || !strings.Contains(result.Response, "step failed") {
		t.Errorf("Response should mention invoke step failure: %q", result.Response)
	}
	if len(result.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call (stop on first error), got %d", len(result.ToolCalls))
	}
}

type errorReturningExecutor struct{ err string }

func (e *errorReturningExecutor) Execute(call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, Error: e.err}
}

func TestPreparerReturnsInvokeSingle(t *testing.T) {
	// Preparer plugin returns JSON: send_to_llm false + invoke single step -> runInvokeSteps runs once
	invokeJSON := `{"send_to_llm": false, "invoke": {"plugin": "gitlab", "action": "analyze_code", "args": {"repo": "r1"}}}`
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "invoker", Description: "Invoker",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: invokeJSON})
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")
	preparers := []ContentPreparerEntry{{Plugin: "invoker", Action: "prepare", Insecure: false}} // trusted: can invoke
	orch := NewWithRules(&fakeLLM{responses: []string{"LLM reply"}}, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions, nil, preparers, nil)

	result, err := orch.Run(context.Background(), "s1", "deploy branch one")
	if err != nil {
		t.Fatal(err)
	}
	// Should run invoke step, not LLM
	if result.Response != "executed gitlab.analyze_code" {
		t.Errorf("Response = %q (expected invoke step result)", result.Response)
	}
	if len(result.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call from invoke, got %d", len(result.ToolCalls))
	}
}

func TestPreparerReturnsInvokeArray(t *testing.T) {
	// Preparer returns invoke as array of steps -> runInvokeSteps runs both
	invokeJSON := `{"send_to_llm": false, "invoke": [
		{"plugin": "gitlab", "action": "analyze_code"},
		{"plugin": "jira", "action": "create_issue", "args": {}}
	]}`
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "invoker", Description: "Invoker",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: invokeJSON})
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "jira", Description: "Jira",
		Actions: []Action{{Name: "create_issue", Description: "Create"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")
	preparers := []ContentPreparerEntry{{Plugin: "invoker", Action: "prepare", Insecure: false}} // trusted: can invoke
	orch := NewWithRules(&fakeLLM{responses: []string{"LLM reply"}}, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions, nil, preparers, nil)

	result, err := orch.Run(context.Background(), "s1", "analyze and create issue")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "executed jira.create_issue" {
		t.Errorf("Response = %q (expected last invoke step)", result.Response)
	}
	if len(result.ToolCalls) != 2 {
		t.Errorf("expected 2 tool calls, got %d", len(result.ToolCalls))
	}
}

func TestInvokeStepsUnmarshalSingleAndArray(t *testing.T) {
	// invokeStepsUnmarshal accepts both single object and array (used by preparer JSON)
	var pr preparerResponse
	sendFalse := false
	pr.SendToLLM = &sendFalse

	// Single object
	if err := json.Unmarshal([]byte(`{"send_to_llm": false, "invoke": {"plugin": "p", "action": "a"}}`), &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Invoke) != 1 {
		t.Fatalf("single invoke: expected 1 step, got %d", len(pr.Invoke))
	}
	if pr.Invoke[0].Plugin != "p" || pr.Invoke[0].Action != "a" {
		t.Errorf("single invoke step = %+v", pr.Invoke[0])
	}

	// Array
	if err := json.Unmarshal([]byte(`{"send_to_llm": false, "invoke": [{"plugin": "p1", "action": "a1"}, {"plugin": "p2", "action": "a2"}]}`), &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Invoke) != 2 {
		t.Fatalf("array invoke: expected 2 steps, got %d", len(pr.Invoke))
	}
	if pr.Invoke[1].Plugin != "p2" {
		t.Errorf("second step plugin = %q", pr.Invoke[1].Plugin)
	}
}

func TestInsecurePreparerCannotInvoke(t *testing.T) {
	// Insecure preparer returns send_to_llm: false + invoke; we must not run invoke, request continues to LLM.
	invokeJSON := `{"send_to_llm": false, "invoke": {"plugin": "gitlab", "action": "analyze_code"}}`
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "insecure-preparer", Description: "Insecure",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: invokeJSON})
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")
	llm := &fakeLLM{responses: []string{"LLM reply"}}
	preparers := []ContentPreparerEntry{
		{Plugin: "insecure-preparer", Action: "prepare", Insecure: true},
	}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry, memory, sessions, nil, preparers, nil)

	result, err := orch.Run(context.Background(), "s1", "deploy branch one")
	if err != nil {
		t.Fatal(err)
	}
	// Invoke must not run; we get LLM response instead of invoke output.
	if result.Response != "LLM reply" {
		t.Errorf("Response = %q (expected LLM reply; insecure preparer must not run invoke)", result.Response)
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected 0 tool calls (invoke ignored), got %d", len(result.ToolCalls))
	}
}
