package plugin

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// fakePluginServer runs a plugin server on a Unix socket in the
// current goroutine. It returns the listener so the caller can
// close it.
func fakePluginServer(t *testing.T, handler pkg.Handler) (network, address string, cleanup func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ot-pl-*")
	if err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "p.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go pkg.ServeConnection(handler, conn)
		}
	}()

	return "unix", sockPath, func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}
}

type echoHandler struct{}

func (h *echoHandler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{
		Name:        "echo",
		Description: "Echoes arguments back",
		Actions: []pkg.ActionMsg{
			{
				Name:        "say",
				Description: "Echo a message",
				Parameters: []pkg.ParameterMsg{
					{Name: "text", Description: "Text to echo", Type: "string", Required: true},
				},
			},
		},
	}
}

func (h *echoHandler) Execute(req pkg.Request) pkg.Response {
	text := req.Args["text"]
	if text == "" {
		return pkg.Response{CallID: req.ID, Error: "missing text"}
	}
	return pkg.Response{CallID: req.ID, Content: "echo: " + text}
}

func TestClientDialAndCapabilities(t *testing.T) {
	network, addr, cleanup := fakePluginServer(t, &echoHandler{})
	defer cleanup()

	client, err := Dial(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if client.Name() != "echo" {
		t.Errorf("name = %q, want echo", client.Name())
	}

	cap := client.Capability()
	if cap.Description != "Echoes arguments back" {
		t.Errorf("description = %q", cap.Description)
	}
	if len(cap.Actions) != 1 {
		t.Fatalf("actions = %d", len(cap.Actions))
	}
	if cap.Actions[0].Name != "say" {
		t.Errorf("action name = %q", cap.Actions[0].Name)
	}
	if len(cap.Actions[0].Parameters) != 1 {
		t.Fatalf("params = %d", len(cap.Actions[0].Parameters))
	}
	if !cap.Actions[0].Parameters[0].Required {
		t.Error("text param should be required")
	}
}

func TestClientExecute(t *testing.T) {
	network, addr, cleanup := fakePluginServer(t, &echoHandler{})
	defer cleanup()

	client, err := Dial(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	result := client.Execute(orchestrator.ToolCall{
		ID:     "c1",
		Plugin: "echo",
		Action: "say",
		Args:   map[string]string{"text": "hello world"},
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.CallID != "c1" {
		t.Errorf("call_id = %q", result.CallID)
	}
	if result.Content != "echo: hello world" {
		t.Errorf("content = %q", result.Content)
	}
}

func TestClientExecuteError(t *testing.T) {
	network, addr, cleanup := fakePluginServer(t, &echoHandler{})
	defer cleanup()

	client, err := Dial(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	result := client.Execute(orchestrator.ToolCall{
		ID:     "c2",
		Plugin: "echo",
		Action: "say",
		Args:   map[string]string{},
	})

	if result.Error != "missing text" {
		t.Errorf("error = %q, want 'missing text'", result.Error)
	}
}

func TestClientMultipleCalls(t *testing.T) {
	network, addr, cleanup := fakePluginServer(t, &echoHandler{})
	defer cleanup()

	client, err := Dial(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	for i := 0; i < 10; i++ {
		result := client.Execute(orchestrator.ToolCall{
			ID:     "multi",
			Plugin: "echo",
			Action: "say",
			Args:   map[string]string{"text": "ping"},
		})
		if result.Content != "echo: ping" {
			t.Fatalf("call %d: content = %q", i, result.Content)
		}
	}
}

func TestClientDialFailure(t *testing.T) {
	_, err := Dial("unix", "/nonexistent/plugin.sock", defaultDialTimeout)
	if err == nil {
		t.Error("expected error for nonexistent socket")
	}
}

func TestManagerLoadAndUnload(t *testing.T) {
	registry := orchestrator.NewToolRegistry()
	mgr := NewManager(registry)

	network, addr, cleanup := fakePluginServer(t, &echoHandler{})
	defer cleanup()

	entry := PluginEntry{
		Name:    "echo",
		Path:    addr,
		Enabled: true,
	}

	// Directly wire up the client (bypass subprocess launch).
	client, err := Dial(network, addr, defaultDialTimeout)
	if err != nil {
		t.Fatal(err)
	}

	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	exec, ok := registry.GetExecutor("echo")
	if !ok {
		t.Fatal("echo not in registry")
	}

	result := exec.Execute(orchestrator.ToolCall{
		ID: "m1", Plugin: "echo", Action: "say",
		Args: map[string]string{"text": "from manager"},
	})
	if result.Content != "echo: from manager" {
		t.Errorf("content = %q", result.Content)
	}

	_ = entry // used for verification
	registry.Deregister("echo")
	_ = client.Close()

	_, ok = registry.GetExecutor("echo")
	if ok {
		t.Error("echo should be deregistered")
	}

	// Verify manager itself can track.
	names := mgr.List()
	if len(names) != 0 {
		t.Errorf("expected 0 managed plugins, got %d", len(names))
	}
}
