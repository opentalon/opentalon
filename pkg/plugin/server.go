package plugin

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
)

// SocketFileName is the Unix socket filename the server listens on.
const SocketFileName = "plugin.sock"

// Handler is the interface that plugin authors implement on the
// server side. The host calls Execute for each tool invocation.
type Handler interface {
	Capabilities() CapabilitiesMsg
	Execute(req Request) Response
}

// ServeListener starts a gRPC server on an existing listener. The caller is
// responsible for printing the handshake line to stdout before calling this,
// and for closing the listener after ServeListener returns.
// Useful for TCP mode (MCP_GRPC_PORT).
func ServeListener(ln net.Listener, handler Handler) error {
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &grpcServer{handler: handler})
	return srv.Serve(ln)
}

// Serve starts a gRPC server on a Unix socket and serves requests from the
// host using the given handler. It prints the handshake line to stdout so the
// host can discover the socket. This function blocks until the server is stopped.
func Serve(handler Handler) error {
	sockDir, err := os.MkdirTemp("", "opentalon-plugin-*")
	if err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	sockPath := filepath.Join(sockDir, SocketFileName)

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

	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &grpcServer{handler: handler})

	return srv.Serve(ln)
}
