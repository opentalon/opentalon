package plugin

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func TestProcess_ExitErrBeforeExit(t *testing.T) {
	proc := &Process{exited: make(chan struct{})}
	if err := proc.ExitErr(); err != nil {
		t.Errorf("ExitErr() before exit = %v, want nil", err)
	}
}

func TestProcess_ExitErrAfterCleanExit(t *testing.T) {
	proc := &Process{exited: make(chan struct{})}
	proc.exitErr = nil
	close(proc.exited)

	if err := proc.ExitErr(); err != nil {
		t.Errorf("ExitErr() after clean exit = %v, want nil", err)
	}
}

func TestProcess_ExitErrAfterFailedExit(t *testing.T) {
	proc := &Process{exited: make(chan struct{})}
	proc.exitErr = errors.New("exit status 1")
	close(proc.exited)

	if err := proc.ExitErr(); err == nil {
		t.Error("ExitErr() = nil, want non-nil error after failed exit")
	}
}

func TestProcess_ExitErrCapturedFromRealProcess(t *testing.T) {
	// Run a real subprocess that exits with code 1 and verify ExitErr captures it.
	cmd := exec.Command("sh", "-c", "exit 1")
	proc := &Process{
		path:   "sh",
		cmd:    cmd,
		exited: make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	go func() {
		err := cmd.Wait()
		proc.mu.Lock()
		proc.exitErr = err
		proc.mu.Unlock()
		close(proc.exited)
	}()

	select {
	case <-proc.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit in time")
	}

	if err := proc.ExitErr(); err == nil {
		t.Error("ExitErr() = nil, want exit error from process that exited with code 1")
	}
}

// TestProcessSurvivesContextCancelAfterHandshake is the regression test for the
// reload_mcp bug: when exec.CommandContext was used, cancelling the caller's
// context (e.g. when a reload tool-call request completed) would kill the
// freshly-started plugin process, causing subsequent tool calls to fail with
// "connection refused". The process must outlive the context that launched it.
func TestProcessSurvivesContextCancelAfterHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subprocess that prints a valid handshake then loops forever, simulating a
	// long-running plugin. The address is a fake path — Start only parses the
	// handshake line and does not attempt to connect.
	// Using a shell loop instead of "sleep 60" so that SIGINT (from Stop) exits
	// the process cleanly without leaving an orphaned sleep child.
	proc := NewProcess("sh", "-c", `echo "1|unix|/tmp/fake-plugin-test.sock"; while true; do sleep 1; done`)

	hs, err := proc.Start(ctx, 5*time.Second)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if hs.Network != "unix" {
		t.Errorf("network = %q, want unix", hs.Network)
	}

	// Cancel the context AFTER the handshake has been received.
	cancel()

	// Allow time for exec.CommandContext to kill the process if it were still
	// wired to ctx. 150 ms is ample; the old (buggy) code killed immediately.
	time.Sleep(150 * time.Millisecond)

	if !proc.Running() {
		t.Error("process was killed after context cancel — plugin must outlive the caller's context")
	}

	_ = proc.Stop(500 * time.Millisecond)
}

// TestProcessKilledOnContextCancelDuringHandshake verifies that if the context
// is cancelled while Start is still waiting for the handshake line, Start kills
// the subprocess and returns an error. This ensures the cancel-during-startup
// path is still handled correctly after the exec.CommandContext → exec.Command
// change.
func TestProcessKilledOnContextCancelDuringHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Subprocess that never prints a handshake.
	proc := NewProcess("sh", "-c", "sleep 60")

	errCh := make(chan error, 1)
	go func() {
		_, err := proc.Start(ctx, 10*time.Second)
		errCh <- err
	}()

	// Cancel while Start is still waiting.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error when context cancelled before handshake, got nil")
		}
		if !strings.Contains(err.Error(), "context") {
			t.Errorf("error = %q, want message about context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestWatchProcessLogsExitError(t *testing.T) {
	// Verify that watchProcess fires and cleans up when a process with a
	// non-nil exitErr exits. The exit error itself is logged (tested via
	// observing that the plugin is removed from m.plugins and deregistered).
	// A dedicated log-capture test would require hooking slog; this test
	// validates the functional outcome (cleanup) that is triggered by the
	// same code path that now also logs exit_error.
	registry := orchestrator.NewToolRegistry()
	m := NewManager(registry)
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	client := newTestPluginClient(t)
	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	proc := &Process{exited: make(chan struct{})}
	proc.exitErr = errors.New("exit status 2")

	m.mu.Lock()
	m.plugins["echo"] = &managed{
		entry:   PluginEntry{Name: "echo"},
		process: proc,
		client:  client,
	}
	m.mu.Unlock()

	m.watchProcess(ctx, "echo", proc)
	close(proc.exited)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, ok := m.plugins["echo"]
		m.mu.Unlock()
		if !ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	m.mu.Lock()
	_, still := m.plugins["echo"]
	m.mu.Unlock()
	if still {
		t.Error("plugin should be removed from m.plugins after exit with non-zero code")
	}
}
