package orchestrator

import (
	"context"
	"testing"
)

type mockExecutor struct {
	result ToolResult
}

func (m *mockExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
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
	result := got.Execute(context.Background(), ToolCall{ID: "1"})
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

// --- Alias tests ---

func TestRegistryAlias_GetExecutor(t *testing.T) {
	reg := NewToolRegistry()
	exec := &mockExecutor{result: ToolResult{Content: "mcp-result"}}
	_ = reg.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "search_issues", Description: "Search"}},
	}, exec)
	if err := reg.RegisterAlias("jira", "mcp"); err != nil {
		t.Fatal(err)
	}

	got, ok := reg.GetExecutor("jira")
	if !ok {
		t.Fatal("expected to find executor via alias")
	}
	result := got.Execute(context.Background(), ToolCall{ID: "1"})
	if result.Content != "mcp-result" {
		t.Errorf("Content = %q, want mcp-result", result.Content)
	}
}

func TestRegistryAlias_GetCapability(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:        "mcp",
		Description: "MCP bridge",
		Actions:     []Action{{Name: "search_issues", Description: "Search"}},
	}, &mockExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")

	cap, ok := reg.GetCapability("jira")
	if !ok {
		t.Fatal("expected to find capability via alias")
	}
	if cap.Name != "jira" {
		t.Errorf("Name = %q, want jira (alias should rewrite name)", cap.Name)
	}
	if cap.Description != "MCP bridge" {
		t.Errorf("Description = %q, want MCP bridge", cap.Description)
	}
	if len(cap.Actions) != 1 || cap.Actions[0].Name != "search_issues" {
		t.Error("alias capability should inherit parent actions")
	}
}

func TestRegistryAlias_HasAction(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "search_issues", Description: "Search"}},
	}, &mockExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")

	if !reg.HasAction("jira", "search_issues") {
		t.Error("expected HasAction(jira, search_issues) = true via alias")
	}
	if reg.HasAction("jira", "nonexistent") {
		t.Error("expected HasAction(jira, nonexistent) = false")
	}
}

func TestRegistryAlias_ListCapabilities_ReplacesParent(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "search", Description: "s"}},
	}, &mockExecutor{})
	_ = reg.Register(PluginCapability{
		Name:    "other",
		Actions: []Action{{Name: "go", Description: "g"}},
	}, &mockExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")
	_ = reg.RegisterAlias("appsignal", "mcp")

	caps := reg.ListCapabilities()
	names := make(map[string]bool)
	for _, c := range caps {
		names[c.Name] = true
	}

	if names["mcp"] {
		t.Error("ListCapabilities should NOT include parent 'mcp' when it has aliases")
	}
	if !names["jira"] {
		t.Error("ListCapabilities should include alias 'jira'")
	}
	if !names["appsignal"] {
		t.Error("ListCapabilities should include alias 'appsignal'")
	}
	if !names["other"] {
		t.Error("ListCapabilities should still include non-aliased 'other'")
	}
}

func TestRegistryAlias_Deregister_CleansUpAliases(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{Name: "mcp"}, &mockExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")

	reg.Deregister("mcp")

	if _, ok := reg.GetExecutor("jira"); ok {
		t.Error("alias should be removed when target is deregistered")
	}
	if aliases := reg.AliasesFor("mcp"); len(aliases) != 0 {
		t.Errorf("AliasesFor should be empty after deregister, got %v", aliases)
	}
}

func TestRegistryAlias_DuplicateAlias(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{Name: "mcp"}, &mockExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")

	err := reg.RegisterAlias("jira", "mcp")
	if err == nil {
		t.Error("expected error on duplicate alias registration")
	}
}

func TestRegistryAlias_ConflictsWithPlugin(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{Name: "mcp"}, &mockExecutor{})
	_ = reg.Register(PluginCapability{Name: "jira"}, &mockExecutor{})

	err := reg.RegisterAlias("jira", "mcp")
	if err == nil {
		t.Error("expected error when alias conflicts with existing plugin name")
	}
}

func TestRegistryAlias_TargetNotFound(t *testing.T) {
	reg := NewToolRegistry()
	err := reg.RegisterAlias("jira", "mcp")
	if err == nil {
		t.Error("expected error when alias target not registered")
	}
}
