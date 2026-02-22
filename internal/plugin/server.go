package plugin

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
)

// Handler is the interface that plugin authors implement on the
// server side. The host calls Execute for each tool invocation.
type Handler interface {
	Capabilities() CapabilitiesMsg
	Execute(req Request) Response
}

// Serve starts a Unix socket listener and serves requests from the
// host using the given handler. It prints the handshake line to
// stdout so the host can discover the socket. This function blocks
// until the listener is closed.
func Serve(handler Handler) error {
	sockDir, err := os.MkdirTemp("", "opentalon-plugin-*")
	if err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	sockPath := filepath.Join(sockDir, "plugin.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer func() { _ = ln.Close() }()
	defer func() { _ = os.RemoveAll(sockDir) }()

	hs := Handshake{Version: HandshakeVersion, Network: "unix", Address: sockPath}
	if _, err := fmt.Fprintln(os.Stdout, hs.String()); err != nil {
		return fmt.Errorf("write handshake: %w", err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		go serveConn(handler, conn)
	}
}

func serveConn(handler Handler, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	for {
		var req Request
		if err := ReadMessage(conn, &req); err != nil {
			return // connection closed or broken
		}

		var resp Response
		switch req.Method {
		case "capabilities":
			caps := handler.Capabilities()
			resp.Caps = &caps
		case "execute":
			resp = handler.Execute(req)
		default:
			resp.Error = fmt.Sprintf("unknown method %q", req.Method)
		}

		if err := WriteMessage(conn, &resp); err != nil {
			log.Printf("plugin server: write response: %v", err)
			return
		}
	}
}
