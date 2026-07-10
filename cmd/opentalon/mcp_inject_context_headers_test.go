package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/plugin"
	"github.com/opentalon/opentalon/pkg/requestpkg"
)

// context_headers must survive to the MCP plugin on BOTH delivery paths
// injectMCPServers writes: the inline Config["servers"] map and the
// OPENTALON_MCP_SERVERS env JSON. (The third path, static host config, already
// preserves unknown keys.)
func TestInjectMCPServers_ForwardsContextHeaders(t *testing.T) {
	entries := []plugin.PluginEntry{{Name: "mcp", Enabled: true}}
	servers := []requestpkg.MCPServerConfig{
		{
			Server:         "example",
			URL:            "http://localhost:8001/mcp",
			ContextHeaders: map[string]string{"session_id": "X-Session-Id"},
		},
	}

	if ok := injectMCPServers(entries, servers, ""); !ok {
		t.Fatal("expected injection to succeed")
	}

	// Inline-config path: context_headers reaches the plugin's Config["servers"].
	got, _ := entries[0].Config["servers"].([]interface{})
	if len(got) != 1 {
		t.Fatalf("Config[servers] len = %d, want 1", len(got))
	}
	m := got[0].(map[string]interface{})
	ch, _ := m["context_headers"].(map[string]string)
	if ch["session_id"] != "X-Session-Id" {
		t.Errorf("context_headers not forwarded to plugin config: %v", m["context_headers"])
	}

	// Env path: OPENTALON_MCP_SERVERS JSON carries context_headers via the tag.
	var envVal string
	for _, kv := range entries[0].Env {
		if strings.HasPrefix(kv, "OPENTALON_MCP_SERVERS=") {
			envVal = strings.TrimPrefix(kv, "OPENTALON_MCP_SERVERS=")
		}
	}
	if !strings.Contains(envVal, `"context_headers"`) || !strings.Contains(envVal, "X-Session-Id") {
		t.Errorf("OPENTALON_MCP_SERVERS missing context_headers: %s", envVal)
	}
	var decoded []requestpkg.MCPServerConfig
	if err := json.Unmarshal([]byte(envVal), &decoded); err != nil {
		t.Fatalf("env JSON invalid: %v", err)
	}
	if len(decoded) != 1 || decoded[0].ContextHeaders["session_id"] != "X-Session-Id" {
		t.Errorf("decoded env ContextHeaders = %v", decoded)
	}
}

// Guards the inline MCPServerConfigInl -> requestpkg.MCPServerConfig conversion
// (the second layer the injection test does not exercise): dropping the
// ContextHeaders copy here would re-break the inline-skill delivery path.
func TestMCPConfigFromInline_CarriesContextHeaders(t *testing.T) {
	inl := &config.MCPServerConfigInl{
		Server:         "example",
		URL:            "http://localhost:9000/mcp",
		Headers:        map[string]string{"Authorization": "Bearer z"},
		ContextHeaders: map[string]string{"session_id": "X-Session-Id"},
	}

	got := mcpConfigFromInline(inl)
	if got == nil {
		t.Fatal("nil result for non-nil input")
	}
	if got.ContextHeaders["session_id"] != "X-Session-Id" {
		t.Errorf("ContextHeaders = %v, want session_id -> X-Session-Id", got.ContextHeaders)
	}
	if got.Server != "example" || got.URL != "http://localhost:9000/mcp" || got.Headers["Authorization"] != "Bearer z" {
		t.Errorf("other fields not carried through: %+v", got)
	}
	if mcpConfigFromInline(nil) != nil {
		t.Error("nil input must yield nil")
	}
}
