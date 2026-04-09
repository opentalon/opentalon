package plugin

import (
	"context"
	"errors"
	"os/exec"
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
