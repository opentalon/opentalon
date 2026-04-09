package plugin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// Process manages the lifecycle of a plugin subprocess.
type Process struct {
	mu      sync.Mutex
	path    string
	args    []string
	env     []string // if non-nil, used as cmd.Env (replaces inherited env)
	cmd     *exec.Cmd
	hs      pkg.Handshake
	exited  chan struct{}
	exitErr error // set before exited is closed
}

// NewProcess creates a process handle without starting it.
func NewProcess(binaryPath string, args ...string) *Process {
	return &Process{
		path: binaryPath,
		args: args,
	}
}

// SetEnv sets the environment for the subprocess. If called before Start,
// cmd.Env is set to env instead of inheriting the parent process environment.
func (p *Process) SetEnv(env []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.env = env
}

// Start launches the plugin binary and reads its handshake line from
// stdout. The plugin must print "version|network|address\n" within
// the given timeout.
func (p *Process) Start(ctx context.Context, timeout time.Duration) (pkg.Handshake, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Use exec.Command (not CommandContext) so the subprocess is not killed when
	// the caller's context expires. Plugin processes must outlive individual
	// request contexts (e.g. a reload_mcp tool call). Cancellation of ctx is
	// respected only during the handshake phase below.
	cmd := exec.Command(p.path, p.args...)
	cmd.Stderr = os.Stderr
	if len(p.env) > 0 {
		cmd.Env = p.env
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return pkg.Handshake{}, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return pkg.Handshake{}, fmt.Errorf("start %s: %w", p.path, err)
	}
	p.cmd = cmd
	p.exited = make(chan struct{})

	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exitErr = err
		p.mu.Unlock()
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
		hs, err := pkg.ParseHandshake(line)
		if err != nil {
			_ = cmd.Process.Kill()
			return pkg.Handshake{}, err
		}
		p.hs = hs
		return hs, nil
	case err := <-hsErr:
		_ = cmd.Process.Kill()
		return pkg.Handshake{}, err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return pkg.Handshake{}, fmt.Errorf("handshake timeout after %s for %s", timeout, p.path)
	case <-p.exited:
		return pkg.Handshake{}, fmt.Errorf("plugin exited before handshake: %s", p.path)
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		return pkg.Handshake{}, fmt.Errorf("context cancelled waiting for handshake from %s: %w", p.path, ctx.Err())
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
		slog.Warn("plugin: interrupt failed, killing", "path", p.path, "error", err)
		return cmd.Process.Kill()
	}

	select {
	case <-exited:
		return nil
	case <-time.After(grace):
		slog.Warn("plugin: did not exit in time, killing", "path", p.path, "grace", grace)
		return cmd.Process.Kill()
	}
}

// Exited returns a channel that is closed when the process exits.
func (p *Process) Exited() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

// ExitErr returns the error from cmd.Wait() after the process has exited.
// Returns nil if the process has not yet exited or exited with code 0.
func (p *Process) ExitErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
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
