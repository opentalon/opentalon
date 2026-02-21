package orchestrator

import "testing"

type mockExecutor struct {
	result ToolResult
}

func (m *mockExecutor) Execute(call ToolCall) ToolResult {
	m.result.CallID = call.ID
	return m.result
}

func gitlabCapability() PluginCapability {
	return PluginCapability{
		Name:        "gitlab",
		Description: "Interact with GitLab",
		Actions: []Action{
			{
				Name:        "analyze_code",
				Description: "Analyze code in a repository",
				Parameters: []Parameter{
					{Name: "repo", Description: "Repository URL", Required: true},
				},
			},
			{
				Name:        "create_pr",
				Description: "Create a pull request",
			},
		},
	}
}

func TestRegistryRegister(t *testing.T) {
	reg := NewToolRegistry()
	cap := gitlabCapability()
	exec := &mockExecutor{}

	if err := reg.Register(cap, exec); err != nil {
		t.Fatal(err)
	}

	got, ok := reg.GetCapability("gitlab")
	if !ok {
		t.Fatal("expected to find gitlab")
	}
	if got.Description != "Interact with GitLab" {
		t.Errorf("Description = %q", got.Description)
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	reg := NewToolRegistry()
	cap := gitlabCapability()
	exec := &mockExecutor{}

	_ = reg.Register(cap, exec)
	err := reg.Register(cap, exec)
	if err == nil {
		t.Error("expected error on duplicate registration")
	}
}

func TestRegistryDeregister(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(gitlabCapability(), &mockExecutor{})
	reg.Deregister("gitlab")

	_, ok := reg.GetCapability("gitlab")
	if ok {
		t.Error("expected gitlab to be deregistered")
	}
}

func TestRegistryGetExecutor(t *testing.T) {
	reg := NewToolRegistry()
	exec := &mockExecutor{result: ToolResult{Content: "done"}}
	_ = reg.Register(gitlabCapability(), exec)

	got, ok := reg.GetExecutor("gitlab")
	if !ok {
		t.Fatal("expected to find executor")
	}
	result := got.Execute(ToolCall{ID: "1"})
	if result.Content != "done" {
		t.Errorf("Content = %q, want done", result.Content)
	}
}

func TestRegistryListCapabilities(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(gitlabCapability(), &mockExecutor{})
	_ = reg.Register(PluginCapability{
		Name:        "jira",
		Description: "Interact with Jira",
		Actions:     []Action{{Name: "create_issue", Description: "Create issue"}},
	}, &mockExecutor{})

	caps := reg.ListCapabilities()
	if len(caps) != 2 {
		t.Errorf("expected 2 capabilities, got %d", len(caps))
	}
}

func TestRegistryHasAction(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(gitlabCapability(), &mockExecutor{})

	if !reg.HasAction("gitlab", "analyze_code") {
		t.Error("expected HasAction(gitlab, analyze_code) = true")
	}
	if !reg.HasAction("gitlab", "create_pr") {
		t.Error("expected HasAction(gitlab, create_pr) = true")
	}
	if reg.HasAction("gitlab", "delete_repo") {
		t.Error("expected HasAction(gitlab, delete_repo) = false")
	}
	if reg.HasAction("unknown", "analyze_code") {
		t.Error("expected HasAction(unknown, analyze_code) = false")
	}
}
