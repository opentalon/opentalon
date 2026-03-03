package channel

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/opentalon/opentalon/pkg/channel/channelpb"
	"google.golang.org/grpc"
)

// SocketFileName is the Unix socket filename used when OPENTALON_CHANNEL_SOCK_DIR is set.
// The host (connector) expects this name when connecting to a channel subprocess.
const SocketFileName = "channel.sock"

// Serve runs the channel as a subprocess server. It creates a Unix listener,
// registers the gRPC ChannelService, then serves until the context is cancelled.
// If OPENTALON_CHANNEL_SOCK_DIR is set (when launched by the host), the socket is
// created there and no handshake is written to stdout, so the process can use
// stdin/stdout for the terminal. Otherwise it creates a temp dir and prints
// id|unix|path to stdout for the host to connect.
// Serve blocks until the context is cancelled.
func Serve(ctx context.Context, ch Channel) error {
	var sockDir string
	var cleanup bool
	if envDir := os.Getenv("OPENTALON_CHANNEL_SOCK_DIR"); envDir != "" {
		sockDir = envDir
	} else {
		var err error
		sockDir, err = os.MkdirTemp("", "opentalon-channel-*")
		if err != nil {
			return fmt.Errorf("create socket dir: %w", err)
		}
		cleanup = true
		defer func() {
			if cleanup {
				_ = os.RemoveAll(sockDir)
			}
		}()
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

	srv := grpc.NewServer()
	channelpb.RegisterChannelServiceServer(srv, &grpcServer{ch: ch})

	// Shut down gracefully when context is cancelled.
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	if err := srv.Serve(ln); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
