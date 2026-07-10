package requestpkg

// MCPServerConfig holds the configuration for one MCP server, as specified
// in the mcp: section of a skill YAML file.
type MCPServerConfig struct {
	Server  string            `json:"server" yaml:"server"`
	URL     string            `json:"url" yaml:"url"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// ContextHeaders maps an injected context-arg name (e.g. "session_id") to
	// the outbound HTTP header the MCP plugin forwards it as. Passed straight
	// through to the plugin config so it survives every delivery path (static
	// host config, inline skill mcp: section, and the OPENTALON_MCP_SERVERS env).
	ContextHeaders map[string]string `json:"context_headers,omitempty" yaml:"context_headers,omitempty"`
}
