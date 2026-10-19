package main

import (
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/metrics"
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
