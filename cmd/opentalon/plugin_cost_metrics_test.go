package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/metrics"
	"github.com/opentalon/opentalon/internal/provider"
)

func TestEnablePluginCostMetrics(t *testing.T) {
	plugins := map[string]config.PluginConfig{
		"talooner": {Enabled: true, Metrics: config.PluginMetricsConfig{Cost: true, Prefix: "talooner"}},
		"quiet":    {Enabled: true},
		"off":      {Enabled: false, Metrics: config.PluginMetricsConfig{Cost: true}},
	}
	c := metrics.New()
	n, err := enablePluginCostMetrics(plugins, c)
	if err != nil || n != 1 || !c.PluginCostEnabled("talooner") || c.PluginCostEnabled("quiet") || c.PluginCostEnabled("off") {
		t.Fatalf("n=%d err=%v", n, err)
	}

	if n, err := enablePluginCostMetrics(plugins, nil); err != nil || n != 0 {
		t.Fatalf("without the metrics endpoint: n=%d err=%v", n, err)
	}

	bad := map[string]config.PluginConfig{"x": {Enabled: true, Metrics: config.PluginMetricsConfig{Prefix: "x"}}}
	if _, err := enablePluginCostMetrics(bad, metrics.New()); err == nil {
		t.Fatal("prefix without cost should be an error")
	}
}

type pricedProvider struct{ provider.Provider }

func (pricedProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{ID: "m", Cost: provider.ModelCost{Input: 2, Output: 10}}}
}

func TestModelCostUSD(t *testing.T) {
	in, out := modelCostUSD(pricedProvider{}, "m", 1_000_000, 500_000)
	if in != 2 || out != 5 {
		t.Errorf("got %v %v", in, out)
	}
	if in, out := modelCostUSD(pricedProvider{}, "unknown", 10, 10); in != 0 || out != 0 {
		t.Error("an unpriced model costs 0")
	}
	if in, out := modelCostUSD(nil, "m", 10, 10); in != 0 || out != 0 {
		t.Error("no provider costs 0")
	}
}

func TestPluginUsageAdapterPricesAndRecords(t *testing.T) {
	c := metrics.New()
	if err := c.EnablePluginCost("talooner", ""); err != nil {
		t.Fatal(err)
	}
	a := &pluginUsageAdapter{provider: pricedProvider{}, collector: c}
	a.ObservePluginUsage(context.Background(), "talooner", "evaluate_pr", "m", 1_000_000, 100_000)
	a.ObservePluginUsage(context.Background(), "talooner", "evaluate_pr", "decide/jev", 500, 5)

	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		`opentalon_plugin_llm_cost_usd_total{action="evaluate_pr",model="m",plugin="talooner"} 3`,
		`opentalon_plugin_llm_cost_usd_total{action="evaluate_pr",model="decide/jev",plugin="talooner"} 0`,
		`opentalon_plugin_llm_tokens_total{action="evaluate_pr",direction="input",model="decide/jev",plugin="talooner"} 500`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
}
