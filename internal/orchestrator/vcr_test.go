package orchestrator

import (
	"context"
	"os"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/vcr"
)

const (
	// Cheapest Anthropic model for VCR recording.
	recordModelAnthropic = "claude-haiku-4-5-20251001"

	// OpenRouter model via OpenAI-compatible API.
	// Profile format "openrouter/<model>" causes the orchestrator to strip the
	// "openrouter/" prefix, leaving the full "mistralai/ministral-8b-2512" that
	// OpenRouter's API expects.
	recordModelOpenRouter = "mistralai/ministral-8b-2512"
	openrouterBaseURL     = "https://openrouter.ai/api/v1"
)

// isRecording returns true when VCR_RECORD=1 is set.
func isRecording() bool { return os.Getenv("VCR_RECORD") == "1" }

// mustPlayer loads a cassette and fails the test immediately if missing or stale.
// Skips if the cassette file doesn't exist (not yet recorded).
// A stale error means prompts changed since recording; re-record with:
//
//	make vcr-record-all
func mustPlayer(t *testing.T, path string) *vcr.Player {
	t.Helper()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skipf("vcr: cassette %s not found; record with: make vcr-record-all", path)
	}
	p, err := vcr.NewPlayer(path)
	if err != nil {
		t.Fatalf("vcr: %v", err)
	}
	return p
}

// vcrLLM returns a Player (replay) or a Recorder wrapping the real Anthropic
// API (when VCR_RECORD=1). save() writes the cassette; no-op on replay.
func vcrLLM(t *testing.T, cassettePath string) (LLMClient, func()) {
	t.Helper()
	if isRecording() {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			t.Skip("VCR_RECORD=1 requires ANTHROPIC_API_KEY")
		}
		real := provider.NewAnthropicProvider("anthropic", "", apiKey, nil)
		rec := vcr.NewRecorder(real, cassettePath, recordModelAnthropic)
		return rec, func() {
			if err := rec.Save(); err != nil {
				t.Errorf("vcr save: %v", err)
			}
		}
	}
	return mustPlayer(t, cassettePath), func() {}
}

// vcrLLMOpenRouter returns a Player (replay) or a Recorder wrapping OpenRouter
// via the OpenAI-compatible API (when VCR_RECORD=1).
func vcrLLMOpenRouter(t *testing.T, cassettePath string) (LLMClient, func()) {
	t.Helper()
	if isRecording() {
		apiKey := os.Getenv("OPENROUTER_API_KEY")
		if apiKey == "" {
			t.Skip("VCR_RECORD=1 requires OPENROUTER_API_KEY for OpenRouter tests")
		}
		real := provider.NewOpenAIProvider("openrouter", openrouterBaseURL, apiKey, nil)
		rec := vcr.NewRecorder(real, cassettePath, recordModelOpenRouter)
		return rec, func() {
			if err := rec.Save(); err != nil {
				t.Errorf("vcr save: %v", err)
			}
		}
	}
	return mustPlayer(t, cassettePath), func() {}
}

// vcrCtx injects the Anthropic record model into context so the provider
// receives a valid model field. Player ignores it on replay.
func vcrCtx() context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{
		// "anthropic/" prefix is stripped by the orchestrator, leaving the bare model ID.
		Model: "anthropic/" + recordModelAnthropic,
	})
}

// vcrCtxOpenRouter injects the OpenRouter model. "openrouter/" prefix is
// stripped by the orchestrator, leaving "mistralai/ministral-8b-2512" which
// OpenRouter's API expects as the full model name.
func vcrCtxOpenRouter() context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{
		Model: "openrouter/" + recordModelOpenRouter,
	})
}

// ── Anthropic / Haiku scenarios ───────────────────────────────────────────

func TestVCRDirectResponse(t *testing.T) {
	llm, save := vcrLLM(t, "testdata/vcr/direct_response.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
	}
}

func TestVCRSingleToolCall(t *testing.T) {
	llm, save := vcrLLM(t, "testdata/vcr/single_tool_call.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "analyze code in myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() {
		if len(result.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
		}
		tc := result.ToolCalls[0]
		if tc.Plugin != "gitlab" || tc.Action != "analyze_code" {
			t.Errorf("tool call = %s.%s, want gitlab.analyze_code", tc.Plugin, tc.Action)
		}
		if tc.Args["repo"] != "myrepo" {
			t.Errorf("args[repo] = %q, want %q", tc.Args["repo"], "myrepo")
		}
	}
}

func TestVCRMultiToolCall(t *testing.T) {
	llm, save := vcrLLM(t, "testdata/vcr/multi_tool_call.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtx(), sessID, "analyze code in myrepo and create a jira issue titled 'Code review'")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() && len(result.ToolCalls) < 2 {
		t.Fatalf("expected at least 2 tool calls, got %d", len(result.ToolCalls))
	}
}

// ── OpenRouter / Ministral 8B scenarios ──────────────────────────────────

func TestVCROpenRouterDirectResponse(t *testing.T) {
	llm, save := vcrLLMOpenRouter(t, "testdata/vcr/openrouter_direct_response.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtxOpenRouter(), sessID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if len(result.ToolCalls) != 0 {
		t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
	}
}

func TestVCROpenRouterSingleToolCall(t *testing.T) {
	llm, save := vcrLLMOpenRouter(t, "testdata/vcr/openrouter_single_tool_call.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtxOpenRouter(), sessID, "analyze code in myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() {
		if len(result.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
		}
		tc := result.ToolCalls[0]
		if tc.Plugin != "gitlab" || tc.Action != "analyze_code" {
			t.Errorf("tool call = %s.%s, want gitlab.analyze_code", tc.Plugin, tc.Action)
		}
		if tc.Args["repo"] != "myrepo" {
			t.Errorf("args[repo] = %q, want %q", tc.Args["repo"], "myrepo")
		}
	}
}

func TestVCROpenRouterMultiToolCall(t *testing.T) {
	llm, save := vcrLLMOpenRouter(t, "testdata/vcr/openrouter_multi_tool_call.json")
	defer save()
	orch, sessID := setupOrchestrator(llm, DefaultParser)
	result, err := orch.Run(vcrCtxOpenRouter(), sessID, "analyze code in myrepo and create a jira issue titled 'Code review'")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}
	if !isRecording() && len(result.ToolCalls) < 2 {
		t.Fatalf("expected at least 2 tool calls, got %d", len(result.ToolCalls))
	}
}
