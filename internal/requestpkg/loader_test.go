package requestpkg

import (
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func TestCollectMCPServers(t *testing.T) {
	sets := []Set{
		{PluginName: "jira", Packages: []Package{{Action: "a"}}},
		{PluginName: "mcp1", MCP: &MCPServerConfig{Server: "srv1", URL: "http://s1"}},
		{PluginName: "mcp2", MCP: &MCPServerConfig{Server: "srv2", URL: "http://s2"}},
	}
	got := CollectMCPServers(sets)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Server != "srv1" || got[1].Server != "srv2" {
		t.Errorf("unexpected servers: %+v", got)
	}
}

func TestCollectMCPServers_NonePresent(t *testing.T) {
	got := CollectMCPServers([]Set{
		{PluginName: "jira", Packages: []Package{{Action: "a"}}},
	})
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestCollectMCPServers_AllNil(t *testing.T) {
	if got := CollectMCPServers(nil); len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestRegister_SkipsMCPSets(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sets := []Set{
		{
			PluginName: "jira",
			Packages:   []Package{{Action: "create_issue", Description: "d", Method: "GET", URL: "http://x"}},
		},
		{
			PluginName: "mcp",
			MCP:        &MCPServerConfig{Server: "mysrv", URL: "http://mcp.example.com/sse"},
		},
	}
	if err := Register(reg, sets); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.GetCapability("jira"); !ok {
		t.Error("jira should be registered")
	}
	if _, ok := reg.GetCapability("mcp"); ok {
		t.Error("mcp set should be skipped (not registered via HTTP executor)")
	}
}

func TestRegister_OnlyMCPSets(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sets := []Set{
		{PluginName: "mcp", MCP: &MCPServerConfig{Server: "s", URL: "http://u"}},
	}
	if err := Register(reg, sets); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if caps := reg.ListCapabilities(); len(caps) != 0 {
		t.Errorf("expected no registrations, got %d", len(caps))
	}
}
