package main

import (
	"testing"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/metrics"
)

// models.deciders.<name>.cost reaches the registry the orchestrator prices with.
func TestBuildDecidersCarriesCost(t *testing.T) {
	cfg, err := config.Parse([]byte(`models:
  deciders:
    snap:
      backend: subq
      base_url: "https://api.subq.ai/v1/systemone"
      cost: { input: 0.05, output: 1.5 }
    jev:
      backend: jev
      base_url: "https://api.typesafe.ai/v1/systemone"
`))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := buildDeciders(cfg.Models.Deciders)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Cost("snap"); got != (decideprovider.Cost{Input: 0.05, Output: 1.5}) {
		t.Errorf("snap cost = %+v", got)
	}
	if got := reg.Cost("jev"); got != (decideprovider.Cost{}) {
		t.Errorf("jev cost = %+v, want unpriced", got)
	}

	cfg.Models.Deciders["snap"] = config.DeciderConfig{Backend: "subq", BaseURL: "https://x", Cost: config.CostConfig{Input: -1}}
	if _, err := buildDeciders(cfg.Models.Deciders); err == nil {
		t.Error("a negative decider cost should fail startup")
	}
}

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
