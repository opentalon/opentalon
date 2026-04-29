package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/scenarios"
	"github.com/opentalon/opentalon/internal/vcr"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

const (
	recordModelAnthropic  = "claude-haiku-4-5-20251001"
	recordModelOpenRouter = "mistralai/ministral-8b-2512"
	openrouterBaseURL     = "https://openrouter.ai/api/v1"
)

// isRecording returns true when VCR_RECORD=1 is set.
func isRecording() bool { return os.Getenv("VCR_RECORD") == "1" }

// mustPlayer loads a cassette. Skips the test if the file doesn't exist (not yet
// recorded). Fails immediately if the cassette is stale (prompt_hash mismatch).
// Re-record with: make vcr-record-all
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

// vcrLLM returns a Player (replay) or a Recorder wrapping the real Anthropic API
// (when VCR_RECORD=1).
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
// (when VCR_RECORD=1).
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

// vcrCtx injects the Anthropic model. The "anthropic/" prefix is stripped by the
// orchestrator, leaving the bare model ID for the provider.
func vcrCtx() context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{
		Model: "anthropic/" + recordModelAnthropic,
	})
}

// vcrCtxOpenRouter injects the OpenRouter model. The "openrouter/" prefix is
// stripped, leaving "mistralai/ministral-8b-2512" which OpenRouter expects.
func vcrCtxOpenRouter() context.Context {
	return profile.WithProfile(context.Background(), &profile.Profile{
		Model: "openrouter/" + recordModelOpenRouter,
	})
}

// providerFixture groups per-provider helpers so scenarios can be driven by a loop.
type providerFixture struct {
	name   string
	model  string // bare model ID sent to provider (after profile prefix is stripped)
	llmFn  func(*testing.T, string) (LLMClient, func())
	ctxFn  func() context.Context
	prefix string // cassette filename prefix, e.g. "openrouter_"
}

func vcrProviders() []providerFixture {
	return []providerFixture{
		{"Anthropic", recordModelAnthropic, vcrLLM, vcrCtx, ""},
		{"OpenRouter", recordModelOpenRouter, vcrLLMOpenRouter, vcrCtxOpenRouter, "openrouter_"},
	}
}

// withModel wraps an LLMClient so that any Complete call with an empty model
// field is filled in. The planner makes its own Complete calls without inheriting
// the profile model, so this wrapper ensures it reaches the provider correctly.
type withModelLLM struct {
	inner LLMClient
	model string
}

func withModel(inner LLMClient, model string) LLMClient {
	return &withModelLLM{inner: inner, model: model}
}

func (w *withModelLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if req.Model == "" {
		req.Model = w.model
	}
	return w.inner.Complete(ctx, req)
}

// ── Core scenarios (both providers, driven from internal/scenarios/testdata) ──

// TestVCRScenarios replays all shared scenarios for both providers.
// Cassette paths are derived from scenario names: "direct response" → direct_response.json.
// Adding a new scenario to testdata/*.yaml automatically queues it for VCR recording;
// the test skips gracefully until the cassette is recorded with make vcr-record-all.
func TestVCRScenarios(t *testing.T) {
	all, err := scenarios.EmbeddedScenarios()
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	for _, s := range all {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			for _, prov := range vcrProviders() {
				prov := prov
				cassette := "testdata/vcr/" + prov.prefix + scenarios.CassetteName(s.Name)
				t.Run(prov.name, func(t *testing.T) {
					llm, save := prov.llmFn(t, cassette)
					defer save()
					orch, sessID := setupOrchestrator(withModel(llm, prov.model), DefaultParser)
					result, err := orch.Run(prov.ctxFn(), sessID, s.Input)
					if err != nil {
						t.Fatal(err)
					}
					if !isRecording() {
						if reason := scenarios.CheckAssertions(s, vcrRunResult(result)); reason != "" {
							t.Error(reason)
						}
					}
				})
			}
		})
	}
}

// vcrRunResult converts a RunResult to scenarios.RunResult for assertion checking.
func vcrRunResult(r *RunResult) scenarios.RunResult {
	out := scenarios.RunResult{Response: r.Response}
	for _, tc := range r.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, scenarios.ToolCallResult{
			Plugin: tc.Plugin,
			Action: tc.Action,
			Args:   tc.Args,
		})
	}
	return out
}

// ── Special-context scenarios (both providers via table loop) ─────────────

// TestVCRSelfIntroduction verifies the LLM identifies itself as OpenTalon when
// a custom rule injects the name into the system prompt.
func TestVCRSelfIntroduction(t *testing.T) {
	for _, prov := range vcrProviders() {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			llm, save := prov.llmFn(t, "testdata/vcr/"+prov.prefix+"self_introduction.json")
			defer save()
			orch, sessID := setupOrchestratorWithOpts(llm, DefaultParser, OrchestratorOpts{
				CustomRules: []string{"Your name is OpenTalon. You are an AI orchestration platform built by opentalon.ai."},
			})
			result, err := orch.Run(prov.ctxFn(), sessID, "present yourself")
			if err != nil {
				t.Fatal(err)
			}
			if result.Response == "" {
				t.Error("empty response")
			}
			if !isRecording() && !strings.Contains(strings.ToLower(result.Response), "opentalon") {
				t.Errorf("response should mention opentalon: %q", result.Response)
			}
		})
	}
}

// TestVCRPipelineConfirmation verifies that a multi-step request triggers pipeline
// planning and the orchestrator returns a confirmation narrative (not tool results).
// The planner LLM call and the NarratePlan call each consume one cassette interaction.
func TestVCRPipelineConfirmation(t *testing.T) {
	for _, prov := range vcrProviders() {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			llm, save := prov.llmFn(t, "testdata/vcr/"+prov.prefix+"pipeline_confirmation.json")
			defer save()
			// withModel ensures planner LLM calls (which don't inherit profile model)
			// reach the provider with a valid model field.
			orch, sessID := setupOrchestratorWithOpts(withModel(llm, prov.model), DefaultParser, OrchestratorOpts{
				PipelineEnabled: true,
				PipelineConfig:  pipeline.PipelineConfig{},
			})
			result, err := orch.Run(prov.ctxFn(), sessID, "analyze code in myrepo and then create a jira issue for the findings")
			if err != nil {
				t.Fatal(err)
			}
			if result.Response == "" {
				t.Error("empty response")
			}
			// Pipeline confirmation returns the narrative, no tool calls yet.
			if !isRecording() && len(result.ToolCalls) != 0 {
				t.Errorf("pipeline confirmation should have no tool calls, got %d", len(result.ToolCalls))
			}
		})
	}
}

// TestVCRFormatSlack verifies the orchestrator injects the Slack format hint and
// the LLM honours it.
func TestVCRFormatSlack(t *testing.T) {
	for _, prov := range vcrProviders() {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			llm, save := prov.llmFn(t, "testdata/vcr/"+prov.prefix+"format_slack.json")
			defer save()
			orch, sessID := setupOrchestrator(llm, DefaultParser)
			ctx := pkgchannel.WithCapabilities(prov.ctxFn(), pkgchannel.Capabilities{
				ResponseFormat: pkgchannel.FormatSlack,
			})
			result, err := orch.Run(ctx, sessID, "list your capabilities")
			if err != nil {
				t.Fatal(err)
			}
			if result.Response == "" {
				t.Error("empty response")
			}
			if len(result.ToolCalls) != 0 {
				t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
			}
		})
	}
}

// TestVCRFormatTelegram verifies the orchestrator injects the Telegram format hint
// and the LLM honours it.
func TestVCRFormatTelegram(t *testing.T) {
	for _, prov := range vcrProviders() {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			llm, save := prov.llmFn(t, "testdata/vcr/"+prov.prefix+"format_telegram.json")
			defer save()
			orch, sessID := setupOrchestrator(llm, DefaultParser)
			ctx := pkgchannel.WithCapabilities(prov.ctxFn(), pkgchannel.Capabilities{
				ResponseFormat: pkgchannel.FormatTelegram,
			})
			result, err := orch.Run(ctx, sessID, "list your capabilities")
			if err != nil {
				t.Fatal(err)
			}
			if result.Response == "" {
				t.Error("empty response")
			}
			if len(result.ToolCalls) != 0 {
				t.Errorf("expected no tool calls, got %d", len(result.ToolCalls))
			}
		})
	}
}
