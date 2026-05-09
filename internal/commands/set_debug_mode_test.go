package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/state"
)

// stubCounter satisfies DebugEventCounter for status-reply tests.
type stubCounter struct{ n int64 }

func (s *stubCounter) CountForSession(ctx context.Context, _ string) (int64, error) {
	return s.n, nil
}

func newDebugTestExecutor(t *testing.T) (*Executor, *state.SessionStore) {
	t.Helper()
	sessions := state.NewSessionStore("")
	sessions.Create("sess-1", "", "")
	registry := orchestrator.NewToolRegistry()
	exec := NewExecutor(registry, sessions, t.TempDir(), nil, "")
	if err := registry.Register(Capability(), exec); err != nil {
		t.Fatalf("register capability: %v", err)
	}
	return exec, sessions
}

func TestSetDebugMode_DefaultToggle(t *testing.T) {
	exec, sessions := newDebugTestExecutor(t)

	// First call (default mode) flips OFF -> ON.
	res := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "c1",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"session_id": "sess-1"},
	})
	if res.Error != "" {
		t.Fatalf("first toggle returned error: %v", res.Error)
	}
	if !strings.Contains(strings.ToUpper(res.Content), "ON") {
		t.Errorf("first toggle Content = %q, expected ON state", res.Content)
	}
	sess, _ := sessions.Get("sess-1")
	if sess.Metadata["debug"] != "true" {
		t.Errorf("metadata[debug] = %q, want \"true\"", sess.Metadata["debug"])
	}

	// Second toggle flips back OFF.
	res = exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "c2",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"session_id": "sess-1"},
	})
	if res.Error != "" {
		t.Fatalf("second toggle returned error: %v", res.Error)
	}
	sess, _ = sessions.Get("sess-1")
	if v, present := sess.Metadata["debug"]; present {
		t.Errorf("after second toggle: metadata[debug] = %q, want absent", v)
	}
}

func TestSetDebugMode_ExplicitOnOff(t *testing.T) {
	exec, sessions := newDebugTestExecutor(t)

	for _, mode := range []string{"on", "ON", "enable", "true"} {
		res := exec.Execute(context.Background(), orchestrator.ToolCall{
			ID:     "x",
			Plugin: PluginName,
			Action: ActionSetDebugMode,
			Args:   map[string]string{"session_id": "sess-1", "mode": mode},
		})
		if res.Error != "" {
			t.Fatalf("mode=%q error: %v", mode, res.Error)
		}
		sess, _ := sessions.Get("sess-1")
		if sess.Metadata["debug"] != "true" {
			t.Errorf("after mode=%q, metadata[debug] = %q, want \"true\"", mode, sess.Metadata["debug"])
		}
	}

	for _, mode := range []string{"off", "disable", "false"} {
		_ = exec.Execute(context.Background(), orchestrator.ToolCall{
			ID:     "x",
			Plugin: PluginName,
			Action: ActionSetDebugMode,
			Args:   map[string]string{"session_id": "sess-1", "mode": mode},
		})
		sess, _ := sessions.Get("sess-1")
		if _, present := sess.Metadata["debug"]; present {
			t.Errorf("after mode=%q, metadata[debug] still present", mode)
		}
	}
}

func TestSetDebugMode_StatusWithCounter(t *testing.T) {
	exec, sessions := newDebugTestExecutor(t)
	exec.WithDebugEventCounter(&stubCounter{n: 7})

	// Turn debug on first.
	_ = exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "x",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"session_id": "sess-1", "mode": "on"},
	})

	res := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "s",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"session_id": "sess-1", "mode": "status"},
	})
	if res.Error != "" {
		t.Fatalf("status error: %v", res.Error)
	}
	if !strings.Contains(res.Content, "ON") || !strings.Contains(res.Content, "7 events") {
		t.Errorf("status content missing ON/7 events: %q", res.Content)
	}

	// Status must not flip the flag.
	sess, _ := sessions.Get("sess-1")
	if sess.Metadata["debug"] != "true" {
		t.Errorf("status mutated debug flag: metadata[debug] = %q", sess.Metadata["debug"])
	}
}

func TestSetDebugMode_RejectsUnknownMode(t *testing.T) {
	exec, _ := newDebugTestExecutor(t)
	res := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "x",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"session_id": "sess-1", "mode": "wat"},
	})
	if res.Error == "" {
		t.Error("expected error for unknown mode, got nil")
	}
}

func TestSetDebugMode_MissingSessionID(t *testing.T) {
	exec, _ := newDebugTestExecutor(t)
	res := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "x",
		Plugin: PluginName,
		Action: ActionSetDebugMode,
		Args:   map[string]string{"mode": "on"},
	})
	if res.Error == "" {
		t.Error("expected error for missing session_id, got nil")
	}
}

// TestSetDebugMode_RepliesHonestlyWithoutCounter exercises the no-DB path:
// the action must not promise persistence when the executor was wired
// without a DebugEventCounter (i.e. no state store configured).
func TestSetDebugMode_RepliesHonestlyWithoutCounter(t *testing.T) {
	exec, _ := newDebugTestExecutor(t)
	// No WithDebugEventCounter — counter remains nil.

	on := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID: "x", Plugin: PluginName, Action: ActionSetDebugMode,
		Args: map[string]string{"session_id": "sess-1", "mode": "on"},
	})
	if strings.Contains(on.Content, "ai_debug_events") || strings.Contains(on.Content, "persisted to") {
		t.Errorf("ON reply must not promise persistence when no counter wired: %q", on.Content)
	}
	if !strings.Contains(on.Content, "no state store configured") &&
		!strings.Contains(on.Content, "not persisted") {
		t.Errorf("ON reply should disclose that persistence is disabled: %q", on.Content)
	}

	off := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID: "x", Plugin: PluginName, Action: ActionSetDebugMode,
		Args: map[string]string{"session_id": "sess-1", "mode": "off"},
	})
	if strings.Contains(off.Content, "Already-captured events") {
		t.Errorf("OFF reply must not reference retention when no counter wired: %q", off.Content)
	}

	status := exec.Execute(context.Background(), orchestrator.ToolCall{
		ID: "x", Plugin: PluginName, Action: ActionSetDebugMode,
		Args: map[string]string{"session_id": "sess-1", "mode": "status"},
	})
	if !strings.Contains(status.Content, "Persistence disabled") {
		t.Errorf("status reply should say Persistence disabled when no counter: %q", status.Content)
	}
}

func TestSetDebugMode_IdempotentEnable(t *testing.T) {
	exec, _ := newDebugTestExecutor(t)
	// Enable twice — second call should not error and reply with status.
	for i := 0; i < 2; i++ {
		res := exec.Execute(context.Background(), orchestrator.ToolCall{
			ID:     "x",
			Plugin: PluginName,
			Action: ActionSetDebugMode,
			Args:   map[string]string{"session_id": "sess-1", "mode": "on"},
		})
		if res.Error != "" {
			t.Fatalf("call %d error: %v", i, res.Error)
		}
	}
}
