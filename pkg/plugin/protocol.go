package plugin

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
)

const (
	// HandshakeVersion is the protocol version in the handshake line.
	HandshakeVersion = 1
	// MaxMessageSize is the maximum length of a single protocol message (4 MB).
	MaxMessageSize = 4 * 1024 * 1024
)

// Request is the wire format sent from the host to the plugin.
type Request struct {
	Method string            `json:"method"`
	ID     string            `json:"id,omitempty"`
	Plugin string            `json:"plugin,omitempty"`
	Action string            `json:"action,omitempty"`
	Args   map[string]string `json:"args,omitempty"`
}

// Response is the wire format sent from the plugin back to the host.
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

// WriteMessage sends a length-prefixed JSON message over a connection.
func WriteMessage(conn net.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(data) > MaxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", len(data), MaxMessageSize)
	}
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(data)))
	if _, err := conn.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// ReadMessage reads a length-prefixed JSON message from a connection.
func ReadMessage(conn net.Conn, v interface{}) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	size := binary.BigEndian.Uint32(header)
	if size > MaxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", size, MaxMessageSize)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return json.Unmarshal(body, v)
}
