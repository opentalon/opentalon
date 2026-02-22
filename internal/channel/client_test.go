package channel

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeChannelHandler simulates a channel plugin on the server side.
type fakeChannelHandler struct {
	caps     Capabilities
	received []OutboundMessage
	sendDone chan struct{} // closed when "send" was handled (for test sync)
}

func (h *fakeChannelHandler) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		var req ChannelRequest
		if err := readMsg(conn, &req); err != nil {
			return
		}

		var resp ChannelResponse
		switch req.Method {
		case "capabilities":
			resp.Caps = &h.caps
		case "start":
			// After ack, send a test inbound message.
			go func() {
				time.Sleep(20 * time.Millisecond)
				msg := ChannelResponse{
					Msg: &InboundMessage{
						ChannelID:      h.caps.ID,
						ConversationID: "conv-1",
						SenderID:       "user-1",
						SenderName:     "Diana",
						Content:        "hello from plugin",
						Timestamp:      time.Now(),
					},
				}
				_ = writeMsg(conn, &msg)
			}()
		case "send":
			if req.Msg != nil {
				h.received = append(h.received, *req.Msg)
			}
			if h.sendDone != nil {
				ch := h.sendDone
				h.sendDone = nil
				close(ch)
			}
		default:
			resp.Error = fmt.Sprintf("unknown method %q", req.Method)
		}

		if err := writeMsg(conn, &resp); err != nil {
			return
		}
	}
}

func fakeChannelServer(t *testing.T, handler *fakeChannelHandler) (network, address string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ot-ch-*")
	if err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "ch.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handler.serve(conn)
		}
	}()

	return "unix", sockPath, func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}
}

func TestChannelClientCapabilities(t *testing.T) {
	handler := &fakeChannelHandler{
		caps: Capabilities{
			ID:               "test-slack",
			Name:             "Slack",
			Threads:          true,
			Files:            true,
			MaxMessageLength: 40000,
		},
	}

	network, addr, cleanup := fakeChannelServer(t, handler)
	defer cleanup()

	client, err := DialChannel(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop() }()

	if client.ID() != "test-slack" {
		t.Errorf("id = %q, want test-slack", client.ID())
	}

	caps := client.Capabilities()
	if caps.Name != "Slack" {
		t.Errorf("name = %q", caps.Name)
	}
	if !caps.Threads {
		t.Error("threads should be true")
	}
	if !caps.Files {
		t.Error("files should be true")
	}
	if caps.MaxMessageLength != 40000 {
		t.Errorf("max_message_length = %d", caps.MaxMessageLength)
	}
}

func TestChannelClientSend(t *testing.T) {
	// net.Pipe() + channel sync: standard Go pattern so test waits for server to process.
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close(); _ = clientConn.Close() }()

	handler := &fakeChannelHandler{
		caps:     Capabilities{ID: "test-ch", Name: "Test"},
		sendDone: make(chan struct{}),
	}
	go handler.serve(serverConn)

	client := NewClientWithConn(clientConn, handler.caps)
	defer func() { _ = client.Stop() }()

	inbox := make(chan InboundMessage, 1)
	if err := client.Start(context.Background(), inbox); err != nil {
		t.Fatal(err)
	}

	msg := OutboundMessage{
		ConversationID: "conv-1",
		Content:        "hello from core",
	}

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	// Wait for server to handle "send" before asserting (channel sync pattern).
	select {
	case <-handler.sendDone:
	case <-time.After(time.Second):
		t.Fatal("timeout: server did not handle send")
	}

	if len(handler.received) != 1 {
		t.Fatalf("expected 1 received message, got %d", len(handler.received))
	}
	if handler.received[0].Content != "hello from core" {
		t.Errorf("content = %q", handler.received[0].Content)
	}
}

func TestChannelClientReceive(t *testing.T) {
	handler := &fakeChannelHandler{
		caps: Capabilities{ID: "recv-ch", Name: "Recv"},
	}

	network, addr, cleanup := fakeChannelServer(t, handler)
	defer cleanup()

	client, err := DialChannel(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Stop() }()

	inbox := make(chan InboundMessage, 10)
	if err := client.Start(context.Background(), inbox); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-inbox:
		if msg.Content != "hello from plugin" {
			t.Errorf("content = %q", msg.Content)
		}
		if msg.SenderName != "Diana" {
			t.Errorf("sender = %q", msg.SenderName)
		}
		if msg.ChannelID != "recv-ch" {
			t.Errorf("channel_id = %q", msg.ChannelID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestChannelClientDialFailure(t *testing.T) {
	_, err := DialChannel("unix", "/nonexistent/channel.sock", defaultDialTimeout)
	if err == nil {
		t.Error("expected error for nonexistent socket")
	}
}

func TestChannelProtocolRoundTrip(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	sent := ChannelRequest{
		Method: "send",
		Msg: &OutboundMessage{
			ConversationID: "c1",
			Content:        "test message",
		},
	}

	go func() { _ = writeMsg(client, &sent) }()

	var received ChannelRequest
	if err := readMsg(server, &received); err != nil {
		t.Fatal(err)
	}
	if received.Method != "send" {
		t.Errorf("method = %q", received.Method)
	}
	if received.Msg == nil {
		t.Fatal("msg is nil")
	}
	if received.Msg.Content != "test message" {
		t.Errorf("content = %q", received.Msg.Content)
	}
}
