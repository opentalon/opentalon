package requestpkg

// MCPServerConfig holds the configuration for one MCP server, as specified
// in the mcp: section of a skill YAML file.
type MCPServerConfig struct {
	Server  string            `json:"server" yaml:"server"`
	URL     string            `json:"url" yaml:"url"`
	Headers map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
}
