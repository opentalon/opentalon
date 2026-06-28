package orchestrator

import (
	"context"
	"strings"
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

// TestUpdateCapabilityReflectsAddedAndRemovedActions locks in the guarantee that
// makes the periodic-refresh UpdateCapability safe: replacing a plugin's
// capability in place must change what the live-resolving reads (ListCapabilities
// / HasAction) return — added actions become routable, removed ones disappear —
// so a future derived index can't silently break it.
func TestUpdateCapabilityReflectsAddedAndRemovedActions(t *testing.T) {
	reg := NewToolRegistry()
	if err := reg.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "a"}, {Name: "b"}},
	}, &mockExecutor{}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A refresh drops b and adds c.
	reg.UpdateCapability("mcp", PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "a"}, {Name: "c"}},
	})

	cap, ok := reg.GetCapability("mcp")
	if !ok {
		t.Fatal("GetCapability(mcp) missing after update")
	}
	names := map[string]bool{}
	for _, a := range cap.Actions {
		names[a.Name] = true
	}
	if len(cap.Actions) != 2 || !names["a"] || !names["c"] || names["b"] {
		t.Errorf("actions after update = %+v, want exactly {a, c}", cap.Actions)
	}
	if !reg.HasAction("mcp", "c") {
		t.Error("HasAction(mcp, c) = false, want true (added action is routable)")
	}
	if reg.HasAction("mcp", "b") {
		t.Error("HasAction(mcp, b) = true, want false (removed action is gone)")
	}
}

// TestRegistryRejectsDottedPluginName verifies the registration-time FQN
// charset guard: a plugin name containing a dot composes a name that an
// LLM provider would reject as an opaque 400, so Register fails up front
// with a clear error instead of letting it reach the API.
func TestRegistryRejectsDottedPluginName(t *testing.T) {
	reg := NewToolRegistry()
	cap := PluginCapability{
		Name: "my.server", // dot is illegal in a provider tool name
		Actions: []Action{
			{Name: "list-items"},
		},
	}
	err := reg.Register(cap, &mockExecutor{})
	if err == nil {
		t.Fatal("Register must reject a dotted plugin name (composed FQN fails the provider charset)")
	}
	if !strings.Contains(err.Error(), "my.server__list-items") {
		t.Errorf("error must name the offending composed FQN, got: %v", err)
	}
	if _, ok := reg.GetCapability("my.server"); ok {
		t.Error("a rejected plugin must not be registered")
	}
}

// TestRegistryAcceptsValidFQN is the positive control: ordinary plugin and
// action names (including the bridged-MCP double-underscore action form)
// compose valid FQNs and register cleanly.
func TestRegistryAcceptsValidFQN(t *testing.T) {
	reg := NewToolRegistry()
	cap := PluginCapability{
		Name: "timly",
		Actions: []Action{
			{Name: "list-items"},
			{Name: "timly__create-item"}, // bridged-MCP action; FQN = timly__timly__create-item
		},
	}
	if err := reg.Register(cap, &mockExecutor{}); err != nil {
		t.Fatalf("valid names must register, got: %v", err)
	}
}
