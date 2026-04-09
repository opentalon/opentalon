package main

import (
	"encoding/json"
	"log/slog"
	"path/filepath"

	"github.com/opentalon/opentalon/internal/plugin"
	"github.com/opentalon/opentalon/pkg/requestpkg"
)

// injectMCPServers merges inline MCP server configs into the "mcp" plugin entry so
// the plugin receives them via its Init RPC config JSON (Config["servers"]).
// Inline packages store the server name as Server; the plugin schema uses "name".
// Any servers already present in Config["servers"] (static config) are preserved.
// The env var OPENTALON_MCP_SERVERS is also set for plugins that support it.
// Returns true if the mcp entry was found and updated.
func injectMCPServers(entries []plugin.PluginEntry, servers []requestpkg.MCPServerConfig, dataDir string) bool {
	if len(servers) == 0 {
		return false
	}
	for i, e := range entries {
		if e.Name != "mcp" {
			continue
		}
		// Build server list in the format the plugin expects: {name, url, headers?}
		inlineServers := make([]map[string]interface{}, len(servers))
		for j, s := range servers {
			srv := map[string]interface{}{"name": s.Server, "url": s.URL}
			if len(s.Headers) > 0 {
				srv["headers"] = s.Headers
			}
			inlineServers[j] = srv
		}
		if entries[i].Config == nil {
			entries[i].Config = make(map[string]interface{})
		}
		// Merge: append inline servers after any statically configured servers.
		existing, _ := entries[i].Config["servers"].([]interface{})
		merged := make([]interface{}, len(existing), len(existing)+len(inlineServers))
		copy(merged, existing)
		for _, s := range inlineServers {
			merged = append(merged, s)
		}
		entries[i].Config["servers"] = merged
		// Also inject via env for plugins that support OPENTALON_MCP_SERVERS.
		if mcpJSON, err := json.Marshal(servers); err == nil {
			entries[i].WithEnvOverride("OPENTALON_MCP_SERVERS", string(mcpJSON))
		}
		if dataDir != "" {
			entries[i].WithEnvOverride("OPENTALON_MCP_CACHE_DIR", filepath.Join(dataDir, "mcp-cache"))
		}
		return true
	}
	slog.Warn("MCP skill configs found but no 'mcp' plugin entry in config", "component", "mcp")
	return false
}
