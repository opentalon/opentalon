package channel

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

// SocketFileName is the Unix socket filename used when OPENTALON_CHANNEL_SOCK_DIR is set.
// The host (connector) expects this name when connecting to a channel subprocess.
const SocketFileName = "channel.sock"

// Serve runs the channel as a subprocess server. It creates a Unix listener,
// then accepts one connection and serves the channel protocol: capabilities, start, send.
// If OPENTALON_CHANNEL_SOCK_DIR is set (when launched by the host), the socket is
// created there and no handshake is written to stdout, so the process can use
// stdin/stdout for the terminal. Otherwise it creates a temp dir and prints
// id|unix|path to stdout for the host to connect.
// Serve blocks until the connection is closed or the context is cancelled.
func Serve(ctx context.Context, ch Channel) error {
	var sockDir string
	if envDir := os.Getenv("OPENTALON_CHANNEL_SOCK_DIR"); envDir != "" {
		sockDir = envDir
	} else {
		var err error
		sockDir, err = os.MkdirTemp("", "opentalon-channel-*")
		if err != nil {
			return fmt.Errorf("create socket dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(sockDir) }()
	}
	sockPath := filepath.Join(sockDir, SocketFileName)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()

	if os.Getenv("OPENTALON_CHANNEL_SOCK_DIR") == "" {
		// Handshake: id|network|address (connector expects 3 parts)
		if _, err := fmt.Fprintf(os.Stdout, "%s|unix|%s\n", ch.ID(), sockPath); err != nil {
			return fmt.Errorf("write handshake: %w", err)
		}
	}

	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer func() { _ = conn.Close() }()

	reqCh := make(chan *ChannelRequest, 8)
	go func() {
		for {
			var req ChannelRequest
			if err := ReadMessage(conn, &req); err != nil {
				return
			}
			reqCh <- &req
		}
	}()

	var inbox chan InboundMessage
	for {
		if inbox == nil {
			req, ok := <-reqCh
			if !ok {
				return nil
			}
			resp, startInbox := handleRequest(ctx, ch, req, nil)
			if resp != nil {
				if err := WriteMessage(conn, resp); err != nil {
					log.Printf("channel server: write: %v", err)
					return err
				}
			}
			if startInbox != nil {
				inbox = startInbox
			}
		} else {
			select {
			case req, ok := <-reqCh:
				if !ok {
					return nil
				}
				resp, _ := handleRequest(ctx, ch, req, nil)
				if resp != nil {
					if err := WriteMessage(conn, resp); err != nil {
						log.Printf("channel server: write: %v", err)
						return err
					}
				}
			case msg, ok := <-inbox:
				if !ok {
					inbox = nil
					continue
				}
				if err := WriteMessage(conn, &ChannelResponse{Msg: &msg}); err != nil {
					log.Printf("channel server: write inbound: %v", err)
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// handleRequest processes one ChannelRequest. Returns response to send and,
// for "start", the inbox channel to read from.
func handleRequest(ctx context.Context, ch Channel, req *ChannelRequest, _ chan InboundMessage) (*ChannelResponse, chan InboundMessage) {
	switch req.Method {
	case "capabilities":
		caps := ch.Capabilities()
		return &ChannelResponse{Caps: &caps}, nil
	case "start":
		inbox := make(chan InboundMessage, 32)
		if err := ch.Start(ctx, inbox); err != nil {
			return &ChannelResponse{Error: err.Error()}, nil
		}
		return &ChannelResponse{}, inbox
	case "send":
		if req.Msg == nil {
			return &ChannelResponse{Error: "send: missing msg"}, nil
		}
		if err := ch.Send(ctx, *req.Msg); err != nil {
			return &ChannelResponse{Error: err.Error()}, nil
		}
		return &ChannelResponse{}, nil
	default:
		return &ChannelResponse{Error: "unknown method " + req.Method}, nil
	}
}
