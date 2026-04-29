//go:build integration

package orchestrator

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/scenarios"
	"github.com/opentalon/opentalon/internal/testutil"
)

const integrationModel = "claude-haiku-4-5-20251001"

// integrationFixtures returns provider fixtures backed by real API clients.
// Skips if no API keys are set.
func integrationFixtures(t *testing.T) []providerFixture {
	t.Helper()
	var fixtures []providerFixture

	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		real := &testutil.ZeroTempLLM{Inner: provider.NewAnthropicProvider("anthropic", "", key, nil)}
		fixtures = append(fixtures, providerFixture{
			name:  "Anthropic",
			model: integrationModel,
			llmFn: func(t *testing.T, _ string) (LLMClient, func()) {
				t.Helper()
				return real, func() {}
			},
			ctxFn: func() context.Context {
				return profile.WithProfile(context.Background(), &profile.Profile{
					Model: "anthropic/" + integrationModel,
				})
			},
		})
	}

	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		real := &testutil.ZeroTempLLM{Inner: provider.NewOpenAIProvider("openrouter", openrouterBaseURL, key, nil)}
		fixtures = append(fixtures, providerFixture{
			name:  "OpenRouter",
			model: recordModelOpenRouter,
			llmFn: func(t *testing.T, _ string) (LLMClient, func()) {
				t.Helper()
				return real, func() {}
			},
			ctxFn: func() context.Context {
				return profile.WithProfile(context.Background(), &profile.Profile{
					Model: "openrouter/" + recordModelOpenRouter,
				})
			},
		})
	}

	if len(fixtures) == 0 {
		t.Skip("set ANTHROPIC_API_KEY or OPENROUTER_API_KEY to run integration tests")
	}
	return fixtures
}

// assertStructural applies cross-cutting structural checks to any run:
// no raw internal block markers, no stripped-to-empty response, no max-iter breach.
func assertStructural(t *testing.T, result *RunResult, err error) {
	t.Helper()
	if err != nil {
		if strings.Contains(err.Error(), "agent loop exceeded") {
			t.Errorf("max iteration breach: %v", err)
		} else {
			t.Fatalf("Run: %v", err)
		}
	}
	if result.Response == "(no response)" {
		t.Error("response was '(no response)': all content stripped as internal blocks")
	}
	if strings.Contains(result.Response, "[tool_call]") || strings.Contains(result.Response, "[/tool_call]") {
		t.Errorf("raw internal block markers in response: %q", result.Response)
	}
}

// TestIntegrationScenarios runs all shared scenarios against the real API and
// verifies structural correctness. Mirrors the same inputs used in VCR and eval.
func TestIntegrationScenarios(t *testing.T) {
	all, err := scenarios.EmbeddedScenarios()
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}

	for _, s := range all {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			t.Parallel()
			for _, prov := range integrationFixtures(t) {
				prov := prov
				t.Run(prov.name, func(t *testing.T) {
					t.Parallel()
					llm, cleanup := prov.llmFn(t, "")
					defer cleanup()
					orch, sessID := setupOrchestrator(withModel(llm, prov.model), DefaultParser)
					result, err := orch.Run(prov.ctxFn(), sessID, s.Input)
					assertStructural(t, result, err)
					if reason := scenarios.CheckAssertions(s, vcrRunResult(result)); reason != "" {
						t.Error(reason)
					}
				})
			}
		})
	}
}
