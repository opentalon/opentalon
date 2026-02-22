package channel

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"bufio"
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
func (c *Connector) Connect(ctx context.Context, id, pluginRef string) (Channel, error) {
	mode := DetectMode(pluginRef)

	switch mode {
	case ModeBinary:
		return c.connectBinary(ctx, id, pluginRef)
	case ModeGRPC:
		_, addr := ParsePluginAddress(pluginRef)
		return c.connectRemote(id, addr)
	default:
		return nil, fmt.Errorf("channel %q: connection mode %s not yet implemented", id, mode)
	}
}

func (c *Connector) connectBinary(ctx context.Context, id, binaryPath string) (Channel, error) {
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start channel binary %s: %w", binaryPath, err)
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	hsLine := make(chan string, 1)
	hsErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			hsLine <- strings.TrimSpace(scanner.Text())
		} else {
			if err := scanner.Err(); err != nil {
				hsErr <- err
			} else {
				hsErr <- fmt.Errorf("channel binary closed stdout before handshake")
			}
		}
	}()

	var network, address string
	select {
	case line := <-hsLine:
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("invalid handshake from channel %q: %q", id, line)
		}
		network = parts[1]
		address = parts[2]
	case err := <-hsErr:
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("channel %q handshake error: %w", id, err)
	case <-time.After(defaultHandshakeTimeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("channel %q handshake timeout", id)
	case <-exited:
		return nil, fmt.Errorf("channel binary %q exited before handshake", id)
	}

	client, err := DialChannel(network, address, defaultDialTimeout)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("dial channel %q: %w", id, err)
	}

	c.mu.Lock()
	c.processes[id] = &channelProcess{cmd: cmd, exited: exited}
	c.mu.Unlock()

	return client, nil
}

func (c *Connector) connectRemote(id, address string) (Channel, error) {
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
