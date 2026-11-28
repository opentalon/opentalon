package orchestrator

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
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

func nativeModel(id string) []provider.ModelInfo {
	return []provider.ModelInfo{{ID: id, Features: []provider.Feature{provider.FeatureTools}}}
}

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
		real := provider.NewAnthropicProvider("anthropic", "", apiKey, nativeModel(recordModelAnthropic))
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
		real := provider.NewOpenAIProvider("openrouter", openrouterBaseURL, apiKey, nativeModel(recordModelOpenRouter))
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
// field is filled in. Sub-agents make their own Complete calls without inheriting
// the profile model, so this wrapper ensures they reach the provider correctly.
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

func (w *withModelLLM) SupportsFeature(f provider.Feature) bool { return supportsFeature(w.inner, f) }

func supportsFeature(llm LLMClient, f provider.Feature) bool {
	rp, ok := llm.(ReasoningProvider)
	return ok && rp.SupportsFeature(f)
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

// TestVCRSubprocessParallel drives the full real-LLM path for the parallel
// sub-agent primitive (#307).
//
// Anthropic-only and recorded with MaxParallel=1 ON PURPOSE. The VCR Player
// replays interactions by position and its pos counter is not mutex-guarded —
// so genuinely concurrent sub-agent LLM calls would both race the counter and
// be nondeterministic on replay. Serializing to one at a time keeps recording +
// replay race-free; the concurrency/cap behaviour itself is covered by the
// barrier-LLM unit test in subprocess_parallel_test.go. Which child runs first
// still varies, so both run the same tool and either child's requests match the recorded ones.
func TestVCRSubprocessParallel(t *testing.T) {
	llm, save := vcrLLM(t, "testdata/vcr/subprocess_parallel.json")
	defer save()
	children := &subagentLog{LLMClient: withModel(llm, recordModelAnthropic)}
	orch, sessID := setupOrchestratorWithOpts(children, DefaultParser, OrchestratorOpts{
		Subprocess: SubprocessConfig{
			Enabled:        true,
			MaxDepth:       2,
			MaxIterations:  5,
			MaxParallel:    1,
			DefaultTimeout: 30 * time.Second,
		},
	})

	prompt := "Analyze the code of two repositories, alpha and beta, independently. Use the _subprocess__parallel tool to run both analyses at once in a single call, one task per repository, each telling its sub-agent to call gitlab__analyze_code for that repository. Then give me both results."
	result, err := orch.Run(vcrCtx(), sessID, prompt)
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == "" {
		t.Error("empty response")
	}

	// Assert in BOTH record and replay: the scenario is only meaningful if the
	// real model actually drove a _subprocess.parallel fan-out. Failing at
	// record time signals the prompt/steering needs adjustment before committing
	// a cassette.
	var tasks []parallelTaskResult
	for _, tc := range result.ToolCalls {
		for _, r := range result.Results {
			if tc.Plugin == "_subprocess" && tc.Action == "parallel" && r.CallID == tc.ID {
				if err := json.Unmarshal([]byte(r.StructuredContent), &tasks); err != nil {
					t.Errorf("parallel call's structured result %q: %v", r.StructuredContent, err)
				}
			}
		}
	}
	if len(tasks) != 2 {
		t.Fatalf("parallel call's tasks = %+v, want two; tool calls %+v", tasks, result.ToolCalls)
	}
	for _, task := range tasks {
		if task.Error != "" {
			t.Errorf("task %q failed: %s", task.Task, task.Error)
		}
	}
	called := map[string]bool{}
	var offered []string
	for i, ex := range children.exchanges {
		var names []string
		for _, td := range ex.Request.Tools {
			names = append(names, td.Name)
		}
		if i == 0 {
			offered = names
		}
		if !slices.Contains(names, "gitlab__analyze_code") || !slices.Equal(names, offered) {
			t.Errorf("sub-agent request %d offered %q, want gitlab__analyze_code among the tools every sub-agent request offers (%q)", i+1, names, offered)
		}
		if slices.ContainsFunc(ex.Response.ToolCalls, func(c provider.ToolCall) bool { return c.Name == "gitlab__analyze_code" }) {
			called[ex.Request.Messages[1].Content] = true
		}
	}
	if len(called) != 2 {
		t.Errorf("sub-agents whose response called gitlab__analyze_code: %d of 2 tasks", len(called))
	}
}

type subagentLog struct {
	LLMClient
	mu        sync.Mutex
	exchanges []vcr.Interaction
}

func (l *subagentLog) SupportsFeature(f provider.Feature) bool {
	return supportsFeature(l.LLMClient, f)
}

func (l *subagentLog) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	resp, err := l.LLMClient.Complete(ctx, req)
	if err == nil && strings.HasPrefix(req.Messages[0].Content, prompts.SubprocessPreamble) {
		l.mu.Lock()
		l.exchanges = append(l.exchanges, vcr.Interaction{Request: req, Response: resp})
		l.mu.Unlock()
	}
	return resp, err
}

func TestVCRReplayRefusesTheTextPath(t *testing.T) {
	if isRecording() {
		t.Skip("replay only")
	}
	// The anonymous struct keeps only Complete, so no native tool calling.
	textOnly := struct{ LLMClient }{mustPlayer(t, "testdata/vcr/single_tool_call.json")}
	orch, sessID := setupOrchestrator(textOnly, DefaultParser)
	_, err := orch.Run(vcrCtx(), sessID, "analyze code in myrepo")
	if err == nil || !strings.Contains(err.Error(), "interaction 1: request differs from the recorded one: tools [], recorded [") ||
		!strings.HasSuffix(err.Error(), "; native preamble false, recorded true") {
		t.Fatalf("Run error = %v, want the first request refused as off the recorded path", err)
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
