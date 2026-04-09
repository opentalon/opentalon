package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/plugin"
	"github.com/opentalon/opentalon/pkg/requestpkg"
)

func TestInjectMCPServers_InjectsIntoConfig(t *testing.T) {
	entries := []plugin.PluginEntry{
		{Name: "hello-world"},
		{Name: "mcp", Enabled: true},
	}
	servers := []requestpkg.MCPServerConfig{
		{Server: "jira", URL: "http://localhost:8001/mcp"},
		{Server: "gitlab", URL: "http://localhost:8002/sse"},
	}

	ok := injectMCPServers(entries, servers, "")

	if !ok {
		t.Fatal("expected injection to succeed")
	}
	mcpEntry := entries[1]
	got, _ := mcpEntry.Config["servers"].([]interface{})
	if len(got) != 2 {
		t.Fatalf("Config[servers] len = %d, want 2", len(got))
	}
	for i, srv := range got {
		m := srv.(map[string]interface{})
		if m["name"] != servers[i].Server {
			t.Errorf("entry %d: name = %q, want %q", i, m["name"], servers[i].Server)
		}
		if m["url"] != servers[i].URL {
			t.Errorf("entry %d: url = %q, want %q", i, m["url"], servers[i].URL)
		}
	}
}

func TestInjectMCPServers_MapsServerFieldToName(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp"}}
	servers := []requestpkg.MCPServerConfig{{Server: "appsignal", URL: "http://localhost:9000/sse"}}

	injectMCPServers(entries, servers, "")

	got := entries[0].Config["servers"].([]interface{})
	m := got[0].(map[string]interface{})
	if _, hasServer := m["server"]; hasServer {
		t.Error("Config entry should use 'name' key, not 'server'")
	}
	if m["name"] != "appsignal" {
		t.Errorf("name = %q, want %q", m["name"], "appsignal")
	}
}

func TestInjectMCPServers_PreservesStaticServers(t *testing.T) {
	entries := []plugin.PluginEntry{
		{
			Name: "mcp",
			Config: map[string]interface{}{
				"servers": []interface{}{
					map[string]interface{}{"name": "static-server", "url": "http://static.example.com"},
				},
			},
		},
	}
	servers := []requestpkg.MCPServerConfig{{Server: "inline-server", URL: "http://inline.example.com"}}

	injectMCPServers(entries, servers, "")

	got := entries[0].Config["servers"].([]interface{})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (static + inline)", len(got))
	}
	if got[0].(map[string]interface{})["name"] != "static-server" {
		t.Error("static server should come first")
	}
	if got[1].(map[string]interface{})["name"] != "inline-server" {
		t.Error("inline server should be appended")
	}
}

func TestInjectMCPServers_SetsEnvVar(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp"}}
	servers := []requestpkg.MCPServerConfig{
		{Server: "jira", URL: "http://localhost:8001/mcp"},
	}

	injectMCPServers(entries, servers, "")

	var envVal string
	for _, kv := range entries[0].Env {
		if strings.HasPrefix(kv, "OPENTALON_MCP_SERVERS=") {
			envVal = strings.TrimPrefix(kv, "OPENTALON_MCP_SERVERS=")
		}
	}
	if envVal == "" {
		t.Fatal("OPENTALON_MCP_SERVERS not set in env")
	}
	var decoded []requestpkg.MCPServerConfig
	if err := json.Unmarshal([]byte(envVal), &decoded); err != nil {
		t.Fatalf("unmarshal OPENTALON_MCP_SERVERS: %v", err)
	}
	if len(decoded) != 1 || decoded[0].Server != "jira" {
		t.Errorf("unexpected OPENTALON_MCP_SERVERS: %v", decoded)
	}
}

func TestInjectMCPServers_SetsCacheDirWhenDataDirProvided(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp"}}
	servers := []requestpkg.MCPServerConfig{{Server: "s", URL: "http://u"}}

	injectMCPServers(entries, servers, "/data")

	var cacheDir string
	for _, kv := range entries[0].Env {
		if strings.HasPrefix(kv, "OPENTALON_MCP_CACHE_DIR=") {
			cacheDir = strings.TrimPrefix(kv, "OPENTALON_MCP_CACHE_DIR=")
		}
	}
	if cacheDir != "/data/mcp-cache" {
		t.Errorf("OPENTALON_MCP_CACHE_DIR = %q, want %q", cacheDir, "/data/mcp-cache")
	}
}

func TestInjectMCPServers_ReturnsFalseWhenNoMCPEntry(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "hello-world"}}
	servers := []requestpkg.MCPServerConfig{{Server: "s", URL: "http://u"}}

	ok := injectMCPServers(entries, servers, "")

	if ok {
		t.Error("expected false when no mcp entry present")
	}
}

func TestInjectMCPServers_ReturnsFalseWhenNoServers(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp"}}

	ok := injectMCPServers(entries, nil, "")

	if ok {
		t.Error("expected false when server list is empty")
	}
}

func TestInjectMCPServers_IncludesHeaders(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp"}}
	servers := []requestpkg.MCPServerConfig{
		{Server: "secure", URL: "http://secure.example.com", Headers: map[string]string{"Authorization": "Bearer tok"}},
	}

	injectMCPServers(entries, servers, "")

	got := entries[0].Config["servers"].([]interface{})
	m := got[0].(map[string]interface{})
	headers, _ := m["headers"].(map[string]string)
	if headers["Authorization"] != "Bearer tok" {
		t.Errorf("headers = %v, want Authorization: Bearer tok", headers)
	}
}
