package main

import (
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
