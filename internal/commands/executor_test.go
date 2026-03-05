package commands

import (
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
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
