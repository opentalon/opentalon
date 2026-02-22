package plugin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Process manages the lifecycle of a plugin subprocess.
type Process struct {
	mu     sync.Mutex
	path   string
	args   []string
	cmd    *exec.Cmd
	hs     Handshake
	exited chan struct{}
}

// NewProcess creates a process handle without starting it.
func NewProcess(binaryPath string, args ...string) *Process {
	return &Process{
		path: binaryPath,
		args: args,
	}
}

// Start launches the plugin binary and reads its handshake line from
// stdout. The plugin must print "version|network|address\n" within
// the given timeout.
func (p *Process) Start(ctx context.Context, timeout time.Duration) (Handshake, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	cmd := exec.CommandContext(ctx, p.path, p.args...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Handshake{}, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return Handshake{}, fmt.Errorf("start %s: %w", p.path, err)
	}
	p.cmd = cmd
	p.exited = make(chan struct{})

	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()

	hsLine := make(chan string, 1)
	hsErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			hsLine <- strings.TrimSpace(scanner.Text())
		} else {
			if err := scanner.Err(); err != nil {
				hsErr <- fmt.Errorf("reading handshake: %w", err)
			} else {
				hsErr <- fmt.Errorf("plugin closed stdout before handshake")
			}
		}
		// Drain remaining stdout so the pipe doesn't block.
		_, _ = io.Copy(io.Discard, stdout)
	}()

	select {
	case line := <-hsLine:
		hs, err := ParseHandshake(line)
		if err != nil {
			_ = cmd.Process.Kill()
			return Handshake{}, err
		}
		p.hs = hs
		return hs, nil
	case err := <-hsErr:
		_ = cmd.Process.Kill()
		return Handshake{}, err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return Handshake{}, fmt.Errorf("handshake timeout after %s for %s", timeout, p.path)
	case <-p.exited:
		return Handshake{}, fmt.Errorf("plugin exited before handshake: %s", p.path)
	}
}

// Stop sends SIGINT and waits for the process to exit. If it doesn't
// exit within the grace period, it is killed.
func (p *Process) Stop(grace time.Duration) error {
	p.mu.Lock()
	cmd := p.cmd
	exited := p.exited
	p.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		log.Printf("plugin: interrupt %s: %v, killing", p.path, err)
		return cmd.Process.Kill()
	}

	select {
	case <-exited:
		return nil
	case <-time.After(grace):
		log.Printf("plugin: %s did not exit after %s, killing", p.path, grace)
		return cmd.Process.Kill()
	}
}

// Exited returns a channel that is closed when the process exits.
func (p *Process) Exited() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// Running reports whether the process is still alive.
func (p *Process) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited == nil {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}
