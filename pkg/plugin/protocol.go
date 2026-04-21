package plugin

import (
	"fmt"
	"net"
	"strings"
)

const (
	// HandshakeVersion is the protocol version in the handshake line.
	HandshakeVersion = 1
)

// CredentialHeader is a per-MCP-server credential specifying an HTTP header
// name and value to inject into requests to that server. The plugin merges
// these with its static configured headers; credential headers take priority.
type CredentialHeader struct {
	Header string `json:"header"` // HTTP header name (e.g. "X-App-User", "Authorization")
	Value  string `json:"value"`  // header value (e.g. "user-123", "Bearer tok")
}

// Request is the logical request from the host to the plugin.
type Request struct {
	Method            string                      `json:"method"`
	ID                string                      `json:"id,omitempty"`
	Plugin            string                      `json:"plugin,omitempty"`
	Action            string                      `json:"action,omitempty"`
	Args              map[string]string           `json:"args,omitempty"`
	CredentialHeaders map[string]CredentialHeader `json:"credential_headers,omitempty"` // per-MCP-server credential headers from WhoAmI, keyed by server name
}

// Response is the logical response from the plugin back to the host.
type Response struct {
	CallID  string           `json:"call_id,omitempty"`
	Content string           `json:"content,omitempty"`
	Error   string           `json:"error,omitempty"`
	Caps    *CapabilitiesMsg `json:"caps,omitempty"`
}

// CapabilitiesMsg carries the plugin's self-description.
type CapabilitiesMsg struct {
	Name                 string      `json:"name"`
	Description          string      `json:"description"`
	Actions              []ActionMsg `json:"actions"`
	SystemPromptAddition string      `json:"system_prompt_addition,omitempty"` // optional text appended to the LLM system prompt
}

// ActionMsg describes one action a plugin supports.
type ActionMsg struct {
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Parameters        []ParameterMsg `json:"parameters,omitempty"`
	InjectContextArgs []string       `json:"inject_context_args,omitempty"` // context arg names (e.g. "actor_id") the host injects before calling Execute
	UserOnly          bool           `json:"user_only,omitempty"`           // if true, hidden from LLM and only invocable directly by the user
}

// ParameterMsg describes one parameter of an action.
type ParameterMsg struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
}

// Handshake is the first line a plugin binary writes to stdout.
// Format: "<version>|<network>|<address>[|<http_addr>]\n"
// Example: "1|unix|/tmp/plugin-gitlab.sock"
// Example with HTTP: "1|unix|/tmp/plugin-workflows.sock|127.0.0.1:9091"
//
// http_addr is optional. When present, the host registers a reverse proxy at
// /{plugin-name}/* that forwards requests to http://<http_addr>/*.
// Plugins set OPENTALON_HTTP_PORT to opt in; the Serve helper handles the rest.
type Handshake struct {
	Version  int
	Network  string // "unix" or "tcp"
	Address  string // socket path or host:port
	HTTPAddr string // optional: host:port for the plugin's own HTTP server
}

func (h Handshake) String() string {
	if h.HTTPAddr != "" {
		return fmt.Sprintf("%d|%s|%s|%s", h.Version, h.Network, h.Address, h.HTTPAddr)
	}
	return fmt.Sprintf("%d|%s|%s", h.Version, h.Network, h.Address)
}

// ParseHandshake parses a handshake line from a plugin.
func ParseHandshake(line string) (Handshake, error) {
	parts := strings.SplitN(strings.TrimSpace(line), "|", 4)
	if len(parts) < 3 {
		return Handshake{}, fmt.Errorf("invalid handshake %q: expected version|network|address[|http_addr]", line)
	}

	var h Handshake
	if _, err := fmt.Sscan(parts[0], &h.Version); err != nil {
		return Handshake{}, fmt.Errorf("invalid handshake version %q: %w", parts[0], err)
	}
	h.Network = parts[1]
	h.Address = parts[2]
	if len(parts) == 4 {
		h.HTTPAddr = parts[3]
		host, _, err := net.SplitHostPort(h.HTTPAddr)
		if err != nil {
			return Handshake{}, fmt.Errorf("invalid handshake http_addr %q: %w", h.HTTPAddr, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return Handshake{}, fmt.Errorf("handshake http_addr %q must be a loopback address (127.x.x.x or ::1)", h.HTTPAddr)
		}
	}

	if h.Version != HandshakeVersion {
		return Handshake{}, fmt.Errorf("unsupported handshake version %d (want %d)", h.Version, HandshakeVersion)
	}
	if h.Network != "unix" && h.Network != "tcp" {
		return Handshake{}, fmt.Errorf("unsupported network %q (want unix or tcp)", h.Network)
	}
	return h, nil
}
