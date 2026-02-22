package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

func TestDefaultRulesNotEmpty(t *testing.T) {
	rc := DefaultRulesConfig()
	if len(rc.Rules()) == 0 {
		t.Error("default rules should not be empty")
	}
}

func TestDefaultRulesContainEnglish(t *testing.T) {
	rc := DefaultRulesConfig()
	found := false
	for _, r := range rc.Rules() {
		if strings.Contains(r, "CRITICAL SAFETY RULE") {
			found = true
			break
		}
	}
	if !found {
		t.Error("default rules should contain English safety rule")
	}
}

func TestDefaultRulesContainEnglishSafetyRule(t *testing.T) {
	rc := DefaultRulesConfig()
	prompt := rc.BuildPromptSection()
	if !strings.Contains(prompt, "CRITICAL SAFETY RULE") {
		t.Error("rules should contain English safety rule (CRITICAL SAFETY RULE)")
	}
}

func TestDefaultRulesContainSchedulingRules(t *testing.T) {
	rc := DefaultRulesConfig()
	prompt := rc.BuildPromptSection()
	if !strings.Contains(prompt, "SCHEDULING RULES") {
		t.Error("default rules should contain scheduling rules")
	}
	if !strings.Contains(prompt, "NEVER create a scheduled job without explicit user approval") {
		t.Error("default rules should contain approval requirement")
	}
}

func TestCustomRulesAppended(t *testing.T) {
	custom := []string{
		"Never send PII to external plugins",
		"All financial data must stay internal",
	}
	rc := NewRulesConfig(custom)

	rules := rc.Rules()
	expected := builtinRuleCount() + 2
	if len(rules) != expected {
		t.Errorf("expected %d rules, got %d", expected, len(rules))
	}

	found := 0
	for _, r := range rules {
		if r == "Never send PII to external plugins" || r == "All financial data must stay internal" {
			found++
		}
	}
	if found != 2 {
		t.Errorf("expected 2 custom rules found, got %d", found)
	}
}

func TestCustomRulesEmptyStringsIgnored(t *testing.T) {
	custom := []string{"valid rule", "", "  ", "another rule"}
	rc := NewRulesConfig(custom)
	rules := rc.Rules()
	expected := builtinRuleCount() + 2
	if len(rules) != expected {
		t.Errorf("empty strings should be ignored, expected %d got %d rules", expected, len(rules))
	}
}

func TestCustomRulesNilPreservesDefaults(t *testing.T) {
	rc := NewRulesConfig(nil)
	if len(rc.Rules()) != builtinRuleCount() {
		t.Errorf("nil custom rules should preserve defaults, got %d", len(rc.Rules()))
	}
}

func TestBuildPromptSectionFormat(t *testing.T) {
	rc := DefaultRulesConfig()
	prompt := rc.BuildPromptSection()

	if !strings.HasPrefix(prompt, "## MANDATORY SAFETY RULES") {
		t.Error("prompt should start with MANDATORY SAFETY RULES header")
	}
	if !strings.Contains(prompt, "You MUST follow ALL") {
		t.Error("prompt should contain enforcement language")
	}
	for _, r := range defaultRules {
		if !strings.Contains(prompt, r) {
			t.Errorf("prompt missing rule: %s", r[:40])
		}
	}
}

func TestBuildPromptSectionCustomMarked(t *testing.T) {
	rc := NewRulesConfig([]string{"my org rule"})
	prompt := rc.BuildPromptSection()

	if !strings.Contains(prompt, "[custom] my org rule") {
		t.Error("custom rules should be marked with [custom] prefix")
	}
}

func TestRulesInjectedIntoSystemPrompt(t *testing.T) {
	llm := &fakeLLM{responses: []string{"Hello!"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1")

	var capturedMessages []provider.Message
	capturingLLM := &captureLLM{
		inner:    llm,
		captured: &capturedMessages,
	}

	orch := NewWithRules(capturingLLM, parser, registry, memory, sessions, []string{"org-specific rule"}, nil)
	_, err := orch.Run(context.Background(), "s1", "hello")
	if err != nil {
		t.Fatal(err)
	}

	if len(capturedMessages) == 0 {
		t.Fatal("expected captured messages")
	}

	systemPrompt := capturedMessages[0].Content
	if !strings.Contains(systemPrompt, "MANDATORY SAFETY RULES") {
		t.Error("system prompt should contain safety rules")
	}
	if !strings.Contains(systemPrompt, "CRITICAL SAFETY RULE") {
		t.Error("system prompt should contain default rules")
	}
	if !strings.Contains(systemPrompt, "org-specific rule") {
		t.Error("system prompt should contain custom rule")
	}
}

type captureLLM struct {
	inner    LLMClient
	captured *[]provider.Message
}

func (c *captureLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	*c.captured = append(*c.captured, req.Messages...)
	return c.inner.Complete(ctx, req)
}
