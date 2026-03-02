package plugin

import (
	"fmt"
	"strings"
)

const (
	// HandshakeVersion is the protocol version in the handshake line.
	HandshakeVersion = 1
)

// Request is the logical request from the host to the plugin.
type Request struct {
	Method string            `json:"method"`
	ID     string            `json:"id,omitempty"`
	Plugin string            `json:"plugin,omitempty"`
	Action string            `json:"action,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
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
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Actions     []ActionMsg `json:"actions"`
}

// ActionMsg describes one action a plugin supports.
type ActionMsg struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  []ParameterMsg `json:"parameters,omitempty"`
}

// ParameterMsg describes one parameter of an action.
type ParameterMsg struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
}

// Handshake is the first line a plugin binary writes to stdout.
// Format: "<version>|<network>|<address>\n"
// Example: "1|unix|/tmp/plugin-gitlab.sock"
type Handshake struct {
	Version int
	Network string // "unix" or "tcp"
	Address string // socket path or host:port
}

func (h Handshake) String() string {
	return fmt.Sprintf("%d|%s|%s", h.Version, h.Network, h.Address)
}

// ParseHandshake parses a handshake line from a plugin.
func ParseHandshake(line string) (Handshake, error) {
	parts := strings.SplitN(strings.TrimSpace(line), "|", 3)
	if len(parts) != 3 {
		return Handshake{}, fmt.Errorf("invalid handshake %q: expected version|network|address", line)
	}

	var h Handshake
	if _, err := fmt.Sscan(parts[0], &h.Version); err != nil {
		return Handshake{}, fmt.Errorf("invalid handshake version %q: %w", parts[0], err)
	}
	h.Network = parts[1]
	h.Address = parts[2]

	if h.Version != HandshakeVersion {
		return Handshake{}, fmt.Errorf("unsupported handshake version %d (want %d)", h.Version, HandshakeVersion)
	}
	if h.Network != "unix" && h.Network != "tcp" {
		return Handshake{}, fmt.Errorf("unsupported network %q (want unix or tcp)", h.Network)
	}
	return h, nil
}
