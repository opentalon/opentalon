package channel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

const maxMessageSize = 4 * 1024 * 1024 // 4 MB

// ChannelRequest is the wire format sent from the host to a channel plugin.
type ChannelRequest struct {
	Method string           `json:"method"`
	Msg    *OutboundMessage `json:"msg,omitempty"`
}

// ChannelResponse is the wire format sent from the channel plugin to the host.
type ChannelResponse struct {
	Error string          `json:"error,omitempty"`
	Msg   *InboundMessage `json:"msg,omitempty"`
	Caps  *Capabilities   `json:"caps,omitempty"`
}

func writeMsg(conn net.Conn, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(data) > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", len(data), maxMessageSize)
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

func readMsg(conn net.Conn, v interface{}) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	size := binary.BigEndian.Uint32(header)
	if size > maxMessageSize {
		return fmt.Errorf("message too large: %d bytes (max %d)", size, maxMessageSize)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	return json.Unmarshal(body, v)
}
