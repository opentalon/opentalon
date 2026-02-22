package channel

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultDialTimeout      = 5 * time.Second
	defaultStopGrace        = 5 * time.Second
)

// Connector creates Channel instances from config entries using the
// appropriate connection mode (binary, gRPC, Docker, webhook, WS).
type Connector struct {
	mu        sync.Mutex
	processes map[string]*channelProcess
}

type channelProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

// NewConnector creates a connector that manages channel subprocesses.
func NewConnector() *Connector {
	return &Connector{
		processes: make(map[string]*channelProcess),
	}
}

// Connect creates a Channel from the given plugin reference string.
// For binary mode, it launches the binary and connects over Unix socket.
// For remote gRPC mode, it dials the remote address directly.
func (c *Connector) Connect(ctx context.Context, id, pluginRef string) (pkg.Channel, error) {
	mode := pkg.DetectMode(pluginRef)

	switch mode {
	case pkg.ModeBinary:
		return c.connectBinary(ctx, id, pluginRef)
	case pkg.ModeGRPC:
		_, addr := pkg.ParsePluginAddress(pluginRef)
		return c.connectRemote(id, addr)
	default:
		return nil, fmt.Errorf("channel %q: connection mode %s not yet implemented", id, mode)
	}
}

// sockFileName is the Unix socket filename used when OPENTALON_CHANNEL_SOCK_DIR is set.
const sockFileName = "channel.sock"

func (c *Connector) connectBinary(ctx context.Context, id, binaryPath string) (pkg.Channel, error) {
	absPath, err := filepath.Abs(binaryPath)
	if err != nil {
		return nil, fmt.Errorf("channel %q path: %w", id, err)
	}
	if _, err := os.Stat(absPath); err != nil {
		return nil, fmt.Errorf("channel %q binary %s: %w", id, absPath, err)
	}

	sockDir, err := os.MkdirTemp("", "opentalon-channel-*")
	if err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	sockPath := filepath.Join(sockDir, sockFileName)

	cmd := exec.CommandContext(ctx, absPath)
	cmd.Env = append(os.Environ(), "OPENTALON_CHANNEL_SOCK_DIR="+sockDir)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(sockDir)
		return nil, fmt.Errorf("start channel binary %s: %w", binaryPath, err)
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// Wait for the channel to create the socket (no stdout handshake when env is set).
	for deadline := time.Now().Add(defaultHandshakeTimeout); time.Now().Before(deadline); {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		select {
		case <-exited:
			_ = os.RemoveAll(sockDir)
			return nil, fmt.Errorf("channel binary %q exited before socket ready", id)
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = os.RemoveAll(sockDir)
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}

	client, err := DialChannel("unix", sockPath, defaultDialTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = os.RemoveAll(sockDir)
		return nil, fmt.Errorf("dial channel %q: %w", id, err)
	}

	c.mu.Lock()
	c.processes[id] = &channelProcess{cmd: cmd, exited: exited}
	c.mu.Unlock()

	return client, nil
}

func (c *Connector) connectRemote(id, address string) (pkg.Channel, error) {
	client, err := DialChannel("tcp", address, defaultDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("connect remote channel %q at %s: %w", id, address, err)
	}
	return client, nil
}

// StopProcess stops the subprocess for a channel if one exists.
func (c *Connector) StopProcess(id string) error {
	c.mu.Lock()
	proc, ok := c.processes[id]
	if ok {
		delete(c.processes, id)
	}
	c.mu.Unlock()

	if !ok || proc.cmd.Process == nil {
		return nil
	}

	if err := proc.cmd.Process.Signal(os.Interrupt); err != nil {
		return proc.cmd.Process.Kill()
	}

	select {
	case <-proc.exited:
		return nil
	case <-time.After(defaultStopGrace):
		log.Printf("channel: %s did not exit gracefully, killing", id)
		return proc.cmd.Process.Kill()
	}
}

// StopAll stops all managed channel subprocesses.
func (c *Connector) StopAll() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.processes))
	for id := range c.processes {
		ids = append(ids, id)
	}
	c.mu.Unlock()

	for _, id := range ids {
		if err := c.StopProcess(id); err != nil {
			log.Printf("channel: stop %s: %v", id, err)
		}
	}
}
