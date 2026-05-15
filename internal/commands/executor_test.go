package commands

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func TestSafeSkillName(t *testing.T) {
	ok := []string{"a", "repo", "my-skill", "org_repo"}
	for _, s := range ok {
		if !safeSkillName(s) {
			t.Errorf("safeSkillName(%q) = false; want true", s)
		}
	}
	bad := []string{"", "a/b", "../evil", "..", "a\\b", "a/b/c"}
	for _, s := range bad {
		if safeSkillName(s) {
			t.Errorf("safeSkillName(%q) = true; want false", s)
		}
	}
}

func TestParseInstallURL(t *testing.T) {
	tests := []struct {
		url        string
		wantGitHub string
		wantName   string
	}{
		{"https://github.com/org/repo", "org/repo", "repo"},
		{"https://github.com/org/repo.git", "org/repo", "repo"},
		{"org/repo", "org/repo", "repo"},
		{"git@github.com:org/repo.git", "org/repo", "repo"},
	}
	for _, tt := range tests {
		github, name := parseInstallURL(tt.url)
		if github != tt.wantGitHub || name != tt.wantName {
			t.Errorf("parseInstallURL(%q) = %q, %q; want %q, %q", tt.url, github, name, tt.wantGitHub, tt.wantName)
		}
	}
	// Path traversal and invalid names must be rejected
	reject := []string{
		"https://github.com/org/../evil",
		"https://github.com/../etc/repo",
		"org/../evil",
		"../../etc/passwd",
		"org/repo/with/slash",
	}
	for _, url := range reject {
		github, name := parseInstallURL(url)
		if github != "" || name != "" {
			t.Errorf("parseInstallURL(%q) = %q, %q; want \"\", \"\" (reject path traversal)", url, github, name)
		}
	}
}

func TestExecutor_ShowConfig_ListCommands_SetCallID(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sessions := state.NewSessionStore("")
	e := NewExecutor(reg, sessions, "", nil, "")

	// showConfig returns CallID
	res := e.showConfig(orchestrator.ToolCall{ID: "call-1"})
	if res.CallID != "call-1" {
		t.Errorf("showConfig CallID = %q; want call-1", res.CallID)
	}

	// listCommands returns CallID
	res = e.listCommands(orchestrator.ToolCall{ID: "call-2"})
	if res.CallID != "call-2" {
		t.Errorf("listCommands CallID = %q; want call-2", res.CallID)
	}
}

func TestExecutor_SetPrompt_LengthLimit(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sessions := state.NewSessionStore("")
	dir := t.TempDir()
	path := dir + "/prompt.txt"
	e := NewExecutor(reg, sessions, "", nil, path)

	// Over limit
	big := make([]byte, maxRuntimePromptBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	res := e.setPrompt(orchestrator.ToolCall{ID: "1", Args: map[string]string{"text": string(big)}})
	if res.Error == "" {
		t.Error("setPrompt with oversized text: expected Error")
	}
}

// TestExecutor_ClearSession_PreservesIdentity is the executor-level regression
// guard for the entity-stripping bug. The pre-fix handler did
// sessions.Delete(id) + sessions.Create(id, "", "") which dropped the row's
// identity fields and reset CreatedAt. The fix routes through ClearMessages,
// which leaves the row intact. CreatedAt is the most accessible canary at
// this layer (the in-memory Session struct does not project entity_id /
// group_id — those live only on the DB-backed store; see
// internal/state/store/session_test.go for the SQL-level assertion).
func TestExecutor_ClearSession_PreservesIdentity(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sessions := state.NewSessionStore("")
	e := NewExecutor(reg, sessions, "", nil, "")

	const sid = "sess-clear-id"
	sess := sessions.Create(sid, "ent-X", "grp-Y")
	sess.Metadata["debug"] = "true"
	_ = sessions.SetModel(sid, "anthropic/claude-sonnet-4")
	_ = sessions.AddMessage(sid, provider.Message{Role: provider.RoleUser, Content: "Hello"})
	_ = sessions.AddMessage(sid, provider.Message{Role: provider.RoleAssistant, Content: "Hi"})
	createdAt := sess.CreatedAt

	res := e.clearSession(context.Background(), orchestrator.ToolCall{
		ID:   "call-clr",
		Args: map[string]string{"session_id": sid},
	})
	if res.Error != "" {
		t.Fatalf("clearSession returned error: %q", res.Error)
	}
	if res.CallID != "call-clr" {
		t.Errorf("CallID = %q, want call-clr", res.CallID)
	}

	got, err := sessions.Get(sid)
	if err != nil {
		t.Fatalf("session vanished after clear: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(got.Messages))
	}
	if got.Summary != "" {
		t.Errorf("Summary = %q, want empty", got.Summary)
	}
	if got.ActiveModel != "anthropic/claude-sonnet-4" {
		t.Errorf("ActiveModel = %q, want preserved", got.ActiveModel)
	}
	if got.Metadata["debug"] != "true" {
		t.Errorf("Metadata[debug] = %q, want preserved", got.Metadata["debug"])
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt changed: was %v, now %v (the Delete+Create bug would reset this)", createdAt, got.CreatedAt)
	}
}

func TestExecutor_ClearSession_MissingSessionID(t *testing.T) {
	reg := orchestrator.NewToolRegistry()
	sessions := state.NewSessionStore("")
	e := NewExecutor(reg, sessions, "", nil, "")

	res := e.clearSession(context.Background(), orchestrator.ToolCall{
		ID:   "call-1",
		Args: map[string]string{}, // no session_id
	})
	if res.Error == "" {
		t.Errorf("clearSession with empty session_id: expected Error, got Content=%q", res.Content)
	}
}

func TestRedactConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Models.Providers = map[string]config.ProviderConfig{
		"p": {APIKey: "secret", BaseURL: "https://api.example.com"},
	}
	cfg.Plugins = map[string]config.PluginConfig{
		"plug": {Enabled: true, Config: map[string]interface{}{"token": "x"}},
	}
	out := redactConfig(cfg)
	if out.Models.Providers["p"].APIKey != "[redacted]" {
		t.Error("expected APIKey redacted")
	}
	if out.Plugins["plug"].Config != nil {
		t.Error("expected plugin Config omitted (redacted)")
	}
}
