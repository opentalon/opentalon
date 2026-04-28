package pipeline

import (
	"context"
	"strings"
	"testing"
)

type fakePlannerLLM struct {
	response string
}

func (f *fakePlannerLLM) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	return &CompletionResponse{Content: f.response}, nil
}

type capturingPlannerLLM struct {
	response string
	captured *CompletionRequest
}

func (c *capturingPlannerLLM) Complete(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	c.captured = req
	return &CompletionResponse{Content: c.response}, nil
}

func TestPlannerReturnsDirect(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"type": "direct"}`}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "direct" {
		t.Errorf("Type = %q, want direct", result.Type)
	}
	if len(result.Steps) != 0 {
		t.Errorf("expected no steps for direct, got %d", len(result.Steps))
	}
}

func TestPlannerReturnsPipeline(t *testing.T) {
	llm := &fakePlannerLLM{response: `{
		"type": "pipeline",
		"steps": [
			{"id": "1", "name": "Get error details", "plugin": "appsignal", "action": "get_error", "args": {"error_id": "123"}, "depends_on": []},
			{"id": "2", "name": "Create Jira issue", "plugin": "jira", "action": "create_issue", "args": {"title": "Fix error"}, "depends_on": ["1"]}
		]
	}`}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "investigate error 123 and create a ticket", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "pipeline" {
		t.Errorf("Type = %q, want pipeline", result.Type)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Steps))
	}
	if result.Steps[0].Command.Plugin != "appsignal" {
		t.Errorf("step 0 plugin = %q", result.Steps[0].Command.Plugin)
	}
	if result.Steps[1].Command.Plugin != "jira" {
		t.Errorf("step 1 plugin = %q", result.Steps[1].Command.Plugin)
	}
	if len(result.Steps[1].DependsOn) != 1 || result.Steps[1].DependsOn[0] != "1" {
		t.Errorf("step 1 depends_on = %v", result.Steps[1].DependsOn)
	}
	if result.Steps[0].State != StepPending {
		t.Errorf("step state = %q, want pending", result.Steps[0].State)
	}
	if result.Steps[0].MaxRetries != -1 {
		t.Errorf("step max_retries = %d, want -1", result.Steps[0].MaxRetries)
	}
}

func TestPlannerHandlesMarkdownCodeFence(t *testing.T) {
	llm := &fakePlannerLLM{response: "```json\n{\"type\": \"pipeline\", \"steps\": [{\"id\": \"1\", \"name\": \"Step one\", \"plugin\": \"p\", \"action\": \"a\"}]}\n```"}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "do things", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "pipeline" {
		t.Errorf("Type = %q, want pipeline", result.Type)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
}

func TestPlannerFallsBackOnInvalidJSON(t *testing.T) {
	llm := &fakePlannerLLM{response: "I'm not sure what you mean, here's some text"}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "something", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "direct" {
		t.Errorf("Type = %q, want direct (fallback)", result.Type)
	}
}

func TestPlannerFallsBackOnEmptySteps(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"type": "pipeline", "steps": []}`}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "something", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != "direct" {
		t.Errorf("Type = %q, want direct (empty steps fallback)", result.Type)
	}
}

func TestPlannerAssignsDefaultIDs(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"type": "pipeline", "steps": [{"name": "Step A", "plugin": "p", "action": "a"}, {"name": "Step B", "plugin": "p", "action": "b"}]}`}
	planner := NewPlanner(llm)

	result, err := planner.Plan(context.Background(), "do things", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps[0].ID != "1" {
		t.Errorf("step 0 ID = %q, want 1", result.Steps[0].ID)
	}
	if result.Steps[1].ID != "2" {
		t.Errorf("step 1 ID = %q, want 2", result.Steps[1].ID)
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`{"type": "direct"}`, `{"type": "direct"}`},
		{"```json\n{\"type\": \"direct\"}\n```", `{"type": "direct"}`},
		{"```\n{\"type\": \"direct\"}\n```", `{"type": "direct"}`},
		{"  {\"type\": \"direct\"}  ", `{"type": "direct"}`},
	}
	for _, tt := range tests {
		got := extractJSON(tt.input)
		if got != tt.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildPlannerPrompt(t *testing.T) {
	caps := []CapabilityInfo{
		{
			Name:        "jira",
			Description: "Jira integration",
			Actions: []ActionInfo{
				{Name: "create_issue", Description: "Create a Jira issue", Parameters: []ParamInfo{
					{Name: "title", Description: "Issue title", Required: true},
				}},
			},
		},
	}
	prompt := buildPlannerPrompt(caps, "")
	if prompt == "" {
		t.Error("expected non-empty prompt")
	}
	if !containsStr(prompt, "plugin=jira | action=create_issue") {
		t.Error("prompt should list plugin=jira | action=create_issue")
	}
	if !containsStr(prompt, "(required)") {
		t.Error("prompt should mark required params")
	}
}

// TestBuildPlannerPromptMCPDotActions verifies that when a plugin (e.g. "mcp") has
// action names containing dots (e.g. "appsignal.get_applications"), the prompt uses
// an explicit plugin= | action= format so the LLM cannot misparse the boundary.
// Regression test for: planner generating plugin=mcp.appsignal action=get_applications
// instead of plugin=mcp action=appsignal.get_applications.
func TestBuildPlannerPromptMCPDotActions(t *testing.T) {
	caps := []CapabilityInfo{
		{
			Name:        "mcp",
			Description: "MCP gateway",
			Actions: []ActionInfo{
				{Name: "appsignal.get_applications", Description: "List AppSignal apps"},
				{Name: "jira.search_issues", Description: "Search Jira issues"},
			},
		},
	}
	prompt := buildPlannerPrompt(caps, "")

	// The explicit format must appear so the LLM knows plugin="mcp", not "mcp.appsignal".
	if !containsStr(prompt, "plugin=mcp | action=appsignal.get_applications") {
		t.Error("prompt should contain 'plugin=mcp | action=appsignal.get_applications'")
	}
	if !containsStr(prompt, "plugin=mcp | action=jira.search_issues") {
		t.Error("prompt should contain 'plugin=mcp | action=jira.search_issues'")
	}
	// The old ambiguous dot-joined form must not appear.
	if containsStr(prompt, "mcp.appsignal.get_applications") {
		t.Error("prompt must not contain ambiguous 'mcp.appsignal.get_applications'")
	}
}

func TestBuildPlannerPromptWithLanguage(t *testing.T) {
	caps := []CapabilityInfo{
		{Name: "jira", Description: "Jira", Actions: []ActionInfo{
			{Name: "create_issue", Description: "Create issue"},
		}},
	}
	prompt := buildPlannerPrompt(caps, "English")
	if !containsStr(prompt, "English") {
		t.Error("prompt should contain language instruction for English")
	}

	// Without language, no language instruction should appear.
	promptNoLang := buildPlannerPrompt(caps, "")
	if containsStr(promptNoLang, "must be written in") {
		t.Error("prompt without language should not contain language instruction")
	}
}

func TestNarratePlanReturnsLLMResponse(t *testing.T) {
	want := "I'll fetch error details from AppSignal, then create a Jira ticket. Want me to proceed?"
	llm := &fakePlannerLLM{response: want}
	planner := NewPlanner(llm)
	steps := []*Step{
		{ID: "1", Name: "Get error details", Command: &PluginCommand{Plugin: "appsignal", Action: "get_error"}},
		{ID: "2", Name: "Create Jira issue", Command: &PluginCommand{Plugin: "jira", Action: "create_issue"}},
	}
	got, err := planner.NarratePlan(context.Background(), steps)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("NarratePlan = %q, want %q", got, want)
	}
}

func TestNarratePlanIncludesStepNamesInPrompt(t *testing.T) {
	c := &capturingPlannerLLM{response: "ok"}
	planner := NewPlanner(c)
	steps := []*Step{
		{ID: "1", Name: "Fetch metrics", Command: &PluginCommand{Plugin: "p", Action: "a"}},
	}
	if _, err := planner.NarratePlan(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range c.captured.Messages {
		if strings.Contains(msg.Content, "Fetch metrics") {
			found = true
			break
		}
	}
	if !found {
		t.Error("NarratePlan prompt should include step names")
	}
}

func TestClassifyConfirmationApproved(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"approved": true}`}
	planner := NewPlanner(llm)
	d, err := planner.ClassifyConfirmation(context.Background(), "yes please go ahead")
	if err != nil {
		t.Fatal(err)
	}
	if d != Approved {
		t.Errorf("ClassifyConfirmation = %d, want Approved", d)
	}
}

func TestClassifyConfirmationRejected(t *testing.T) {
	llm := &fakePlannerLLM{response: `{"approved": false}`}
	planner := NewPlanner(llm)
	d, err := planner.ClassifyConfirmation(context.Background(), "no thanks cancel it")
	if err != nil {
		t.Fatal(err)
	}
	if d != Rejected {
		t.Errorf("ClassifyConfirmation = %d, want Rejected", d)
	}
}

func TestClassifyConfirmationMarkdownWrapped(t *testing.T) {
	llm := &fakePlannerLLM{response: "```json\n{\"approved\": true}\n```"}
	planner := NewPlanner(llm)
	d, err := planner.ClassifyConfirmation(context.Background(), "sure go for it")
	if err != nil {
		t.Fatal(err)
	}
	if d != Approved {
		t.Errorf("ClassifyConfirmation = %d, want Approved", d)
	}
}

func TestClassifyConfirmationMalformedJSON(t *testing.T) {
	llm := &fakePlannerLLM{response: "I cannot determine that"}
	planner := NewPlanner(llm)
	d, err := planner.ClassifyConfirmation(context.Background(), "sure")
	if err == nil {
		t.Error("expected error on malformed JSON")
	}
	if d != Rejected {
		t.Errorf("ClassifyConfirmation = %d, want Rejected on error", d)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
