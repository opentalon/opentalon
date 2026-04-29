//go:build eval

package eval_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/eval"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/scenarios"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/testutil"
)

const (
	anthropicModel  = "claude-haiku-4-5-20251001"
	openrouterModel = "mistralai/ministral-8b-2512"
	openrouterURL   = "https://openrouter.ai/api/v1"
	scenariosDir    = "../scenarios/testdata"
	baselineDir     = "../../.eval-baselines"
	passThreshold   = 0.9
)

type evalProvider struct {
	name         string
	profileModel string // "anthropic/<model>" or "openrouter/<model>"
	llm          orchestrator.LLMClient
}

func evalProviders() []evalProvider {
	var out []evalProvider
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		out = append(out, evalProvider{
			name:         "Anthropic",
			profileModel: "anthropic/" + anthropicModel,
			llm:          &testutil.ZeroTempLLM{Inner: provider.NewAnthropicProvider("anthropic", "", key, nil)},
		})
	}
	if key := os.Getenv("OPENROUTER_API_KEY"); key != "" {
		out = append(out, evalProvider{
			name:         "OpenRouter",
			profileModel: "openrouter/" + openrouterModel,
			llm:          &testutil.ZeroTempLLM{Inner: provider.NewOpenAIProvider("openrouter", openrouterURL, key, nil)},
		})
	}
	return out
}

func currentTag() string {
	out, err := exec.Command("git", "describe", "--tags", "--exact-match").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newOrchestrator(llm orchestrator.LLMClient) (*orchestrator.Orchestrator, string) {
	registry := orchestrator.NewToolRegistry()
	_ = registry.Register(orchestrator.PluginCapability{
		Name:        "gitlab",
		Description: "GitLab integration",
		Actions: []orchestrator.Action{
			{Name: "analyze_code", Description: "Analyze code", Parameters: []orchestrator.Parameter{{Name: "repo", Description: "Repository"}}},
			{Name: "create_pr", Description: "Create PR"},
		},
	}, &echoExecutor{})
	_ = registry.Register(orchestrator.PluginCapability{
		Name:        "jira",
		Description: "Jira integration",
		Actions:     []orchestrator.Action{{Name: "create_issue", Description: "Create issue"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessID := "eval-session"
	sessions.Create(sessID)
	return orchestrator.New(llm, orchestrator.DefaultParser, registry, memory, sessions), sessID
}

type echoExecutor struct{}

func (e *echoExecutor) Execute(_ context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("executed %s.%s", call.Plugin, call.Action),
	}
}

func runScenario(prov evalProvider, s scenarios.Scenario) (scenarios.RunResult, error) {
	ctx := profile.WithProfile(context.Background(), &profile.Profile{
		Model: prov.profileModel,
	})
	orch, sessID := newOrchestrator(prov.llm)
	res, err := orch.Run(ctx, sessID, s.Input)
	if err != nil {
		return scenarios.RunResult{}, err
	}
	out := scenarios.RunResult{Response: res.Response}
	for _, tc := range res.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, scenarios.ToolCallResult{
			Plugin: tc.Plugin,
			Action: tc.Action,
			Args:   tc.Args,
		})
	}
	return out, nil
}

func TestEval(t *testing.T) {
	providers := evalProviders()
	if len(providers) == 0 {
		t.Skip("set ANTHROPIC_API_KEY and/or OPENROUTER_API_KEY to run eval")
	}

	all, err := scenarios.LoadScenarios(scenariosDir)
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no scenarios found in " + scenariosDir)
	}

	var results []eval.ScenarioResult
	passed := 0
	total := 0

	for _, prov := range providers {
		prov := prov
		t.Run(prov.name, func(t *testing.T) {
			for _, s := range all {
				s := s
				total++
				t.Run(s.Name, func(t *testing.T) {
					res, err := runScenario(prov, s)
					name := prov.name + "/" + s.Name
					if err != nil {
						results = append(results, eval.ScenarioResult{Name: name, Reason: err.Error()})
						t.Errorf("run error: %v", err)
						return
					}
					reason := scenarios.CheckAssertions(s, res)
					sr := eval.ScenarioResult{Name: name, Passed: reason == "", Reason: reason}
					results = append(results, sr)
					if reason != "" {
						t.Errorf("FAIL: %s", reason)
					} else {
						passed++
					}
				})
			}
		})
	}

	passRate := float64(passed) / float64(total)
	t.Logf("pass rate: %d/%d (%.0f%%)", passed, total, passRate*100)

	tag := currentTag()
	result := eval.EvalResult{
		Tag:      tag,
		PassRate: passRate,
		Passed:   passed,
		Total:    total,
		Results:  results,
	}

	if tag != "" {
		baselinePath := filepath.Join(baselineDir, tag+".json")
		baseline, err := eval.LoadBaseline(baselinePath)
		if err != nil {
			t.Logf("warn: load baseline: %v", err)
		}
		if baseline != nil {
			if passRate < baseline.PassRate {
				t.Errorf("pass rate regressed: %.0f%% < baseline %.0f%% (tag %s)",
					passRate*100, baseline.PassRate*100, baseline.Tag)
			}
		} else if os.Getenv("EVAL_SAVE_BASELINE") == "1" {
			if err := eval.SaveBaseline(baselinePath, result); err != nil {
				t.Logf("warn: save baseline: %v", err)
			} else {
				t.Logf("saved baseline to %s", baselinePath)
			}
		} else {
			t.Errorf("no baseline for tag %s; run with EVAL_SAVE_BASELINE=1 to create one", tag)
		}
	}

	if passRate < passThreshold {
		t.Errorf("pass rate %.0f%% below threshold %.0f%%", passRate*100, passThreshold*100)
	}
}
