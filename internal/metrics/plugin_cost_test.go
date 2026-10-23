package metrics

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPluginCostIsOffByDefault(t *testing.T) {
	c := New()
	c.ObservePluginUsage(context.Background(), "talooner", "evaluate_pr", "", "m", 100, 10, 0.5)
	if n := gatherCount(t, c, "opentalon_plugin_llm_tokens_total", "opentalon_plugin_llm_cost_usd_total"); n != 0 {
		t.Fatalf("a plugin without metrics.cost must add no series, got %d", n)
	}
}

func TestPluginCostForEnabledPlugin(t *testing.T) {
	c := New()
	if err := c.EnablePluginCost("talooner", ""); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.ObservePluginUsage(ctx, "talooner", "evaluate_pr", "", "qwen3-coder-plus", 1000, 200, 0.0042)
	c.ObservePluginUsage(ctx, "talooner", "evaluate_pr", "", "qwen3-coder-plus", 500, 100, 0.0021)
	c.ObservePluginUsage(ctx, "other", "x", "", "m", 1, 1, 1)

	if got := testutil.ToFloat64(c.pluginCost.cost.WithLabelValues("talooner", "evaluate_pr", "", "qwen3-coder-plus")); got < 0.00629 || got > 0.00631 {
		t.Errorf("cost = %v, want 0.0063", got)
	}
	if got := testutil.ToFloat64(c.pluginCost.tokens.WithLabelValues("talooner", "evaluate_pr", "", "qwen3-coder-plus", "input")); got != 1500 {
		t.Errorf("input tokens = %v", got)
	}
	if got := testutil.ToFloat64(c.pluginCost.tokens.WithLabelValues("talooner", "evaluate_pr", "", "qwen3-coder-plus", "output")); got != 300 {
		t.Errorf("output tokens = %v", got)
	}
	if n := gatherCount(t, c, "opentalon_plugin_llm_cost_usd_total"); n != 1 {
		t.Errorf("only the enabled plugin has cost series, got %d", n)
	}
	if !c.PluginCostEnabled("talooner") || c.PluginCostEnabled("other") {
		t.Error("PluginCostEnabled")
	}
	if err := c.EnablePluginCost("talooner", ""); err == nil {
		t.Error("enabling twice should fail")
	}
}

func TestPluginCostPrefixAlias(t *testing.T) {
	c := New()
	if err := c.EnablePluginCost("talooner", "talooner"); err != nil {
		t.Fatal(err)
	}
	c.ObservePluginUsage(context.Background(), "talooner", "evaluate_pr", "", "m", 10, 5, 0.25)
	expected := `
# HELP talooner_llm_cost_usd_total Cost of LLM calls made by the talooner plugin, by action, stage and model (alias of opentalon_plugin_llm_cost_usd_total).
# TYPE talooner_llm_cost_usd_total counter
talooner_llm_cost_usd_total{action="evaluate_pr",model="m",stage=""} 0.25
`
	if err := testutil.GatherAndCompare(c.reg, strings.NewReader(expected), "talooner_llm_cost_usd_total"); err != nil {
		t.Fatal(err)
	}
	if n := gatherCount(t, c, "talooner_llm_tokens_total"); n != 2 {
		t.Errorf("alias tokens series = %d, want 2 (input, output)", n)
	}
}

func TestPluginCostPrefixValidation(t *testing.T) {
	c := New()
	if err := c.EnablePluginCost("a", "bad-name"); err == nil {
		t.Error("invalid prefix accepted")
	}
	if err := c.EnablePluginCost("b", "opentalon_plugin"); err == nil {
		t.Error("prefix colliding with an existing metric accepted")
	}
	if c.PluginCostEnabled("a") || c.PluginCostEnabled("b") {
		t.Error("a failed enable must not leave the plugin enabled")
	}
}

func TestPluginCostCarriesStage(t *testing.T) {
	c := New()
	if err := c.EnablePluginCost("talooner", "talooner"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c.ObservePluginUsage(ctx, "talooner", "evaluate_pr", "verify", "m", 10, 5, 0.25)
	c.ObservePluginUsage(ctx, "talooner", "evaluate_pr", "", "m", 1, 2, 0.5)
	c.ObservePluginUsage(ctx, "quiet", "evaluate_pr", "verify", "m", 1, 2, 0.5)

	expected := `
# HELP opentalon_plugin_llm_cost_usd_total Cost of LLM calls made on behalf of a plugin, by action, stage and model, priced with the model's configured cost. Stage is the plugin's own label for the step, "" when unset. Only for plugins with metrics.cost enabled.
# TYPE opentalon_plugin_llm_cost_usd_total counter
opentalon_plugin_llm_cost_usd_total{action="evaluate_pr",model="m",plugin="talooner",stage=""} 0.5
opentalon_plugin_llm_cost_usd_total{action="evaluate_pr",model="m",plugin="talooner",stage="verify"} 0.25
# HELP opentalon_plugin_llm_tokens_total LLM tokens used on behalf of a plugin (its _subprocess and decide calls), by action, stage, model and direction. Stage is the plugin's own label for the step, "" when unset. Only for plugins with metrics.cost enabled.
# TYPE opentalon_plugin_llm_tokens_total counter
opentalon_plugin_llm_tokens_total{action="evaluate_pr",direction="input",model="m",plugin="talooner",stage=""} 1
opentalon_plugin_llm_tokens_total{action="evaluate_pr",direction="input",model="m",plugin="talooner",stage="verify"} 10
opentalon_plugin_llm_tokens_total{action="evaluate_pr",direction="output",model="m",plugin="talooner",stage=""} 2
opentalon_plugin_llm_tokens_total{action="evaluate_pr",direction="output",model="m",plugin="talooner",stage="verify"} 5
# HELP talooner_llm_cost_usd_total Cost of LLM calls made by the talooner plugin, by action, stage and model (alias of opentalon_plugin_llm_cost_usd_total).
# TYPE talooner_llm_cost_usd_total counter
talooner_llm_cost_usd_total{action="evaluate_pr",model="m",stage=""} 0.5
talooner_llm_cost_usd_total{action="evaluate_pr",model="m",stage="verify"} 0.25
# HELP talooner_llm_tokens_total LLM tokens used by the talooner plugin, by action, stage, model and direction (alias of opentalon_plugin_llm_tokens_total).
# TYPE talooner_llm_tokens_total counter
talooner_llm_tokens_total{action="evaluate_pr",direction="input",model="m",stage=""} 1
talooner_llm_tokens_total{action="evaluate_pr",direction="input",model="m",stage="verify"} 10
talooner_llm_tokens_total{action="evaluate_pr",direction="output",model="m",stage=""} 2
talooner_llm_tokens_total{action="evaluate_pr",direction="output",model="m",stage="verify"} 5
`
	names := []string{"opentalon_plugin_llm_cost_usd_total", "opentalon_plugin_llm_tokens_total", "talooner_llm_cost_usd_total", "talooner_llm_tokens_total"}
	if err := testutil.GatherAndCompare(c.reg, strings.NewReader(expected), names...); err != nil {
		t.Fatal(err)
	}
}
