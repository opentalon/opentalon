package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func gatherCount(t *testing.T, c *Collector, names ...string) int {
	t.Helper()
	n, err := testutil.GatherAndCount(c.reg, names...)
	if err != nil {
		t.Fatalf("GatherAndCount: %v", err)
	}
	return n
}

func TestCollectorNew(t *testing.T) {
	c := New()
	if c == nil {
		t.Fatal("New() returned nil")
	}
	if gatherCount(t, c) == 0 {
		t.Error("expected at least one metric family from go/process collectors")
	}
}

func TestRecordUsageSingleCall(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "entity1", "g1", "ch1", "sess1", "m1", 100, 50, 3, 0.01, 0.02)

	labels := prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1"}
	if got := testutil.ToFloat64(c.llmInputTokens.With(labels)); got != 100 {
		t.Errorf("inputTokens = %v, want 100", got)
	}
	if got := testutil.ToFloat64(c.llmOutputTokens.With(labels)); got != 50 {
		t.Errorf("outputTokens = %v, want 50", got)
	}
	if got := testutil.ToFloat64(c.orchestratorRuns.With(labels)); got != 1 {
		t.Errorf("orchestratorRuns = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.llmInputCostUSD.With(labels)); got != 0.01 {
		t.Errorf("inputCost = %v, want 0.01", got)
	}
	if got := testutil.ToFloat64(c.llmOutputCostUSD.With(labels)); got != 0.02 {
		t.Errorf("outputCost = %v, want 0.02", got)
	}
}

func TestRecordUsageAccumulates(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 100, 50, 1, 0.0, 0.0)
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 200, 80, 2, 0.0, 0.0)

	labels := prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1"}
	if got := testutil.ToFloat64(c.llmInputTokens.With(labels)); got != 300 {
		t.Errorf("inputTokens = %v, want 300", got)
	}
	if got := testutil.ToFloat64(c.orchestratorRuns.With(labels)); got != 2 {
		t.Errorf("orchestratorRuns = %v, want 2", got)
	}
}

func TestRecordUsageZeroCostEmitsSeries(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 10, 5, 0, 0.0, 0.0)

	// Zero-cost models still initialise the series so sum() queries include them.
	if n := gatherCount(t, c, "opentalon_llm_input_cost_usd_total"); n != 1 {
		t.Errorf("expected 1 input cost series, got %d", n)
	}
	if n := gatherCount(t, c, "opentalon_llm_output_cost_usd_total"); n != 1 {
		t.Errorf("expected 1 output cost series, got %d", n)
	}
}

func TestRecordUsagePartialCost(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 10, 5, 0, 0.05, 0.0)

	labels := prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1"}
	if got := testutil.ToFloat64(c.llmInputCostUSD.With(labels)); got != 0.05 {
		t.Errorf("inputCost = %v, want 0.05", got)
	}
	if got := testutil.ToFloat64(c.llmOutputCostUSD.With(labels)); got != 0 {
		t.Errorf("outputCost = %v, want 0", got)
	}
}

func TestObservePluginCallSuccess(t *testing.T) {
	c := New()
	c.ObservePluginCall("gitlab", "analyze_code", false)

	if got := testutil.ToFloat64(c.pluginCalls.With(prometheus.Labels{"plugin": "gitlab", "action": "analyze_code", "status": "success"})); got != 1 {
		t.Errorf("plugin calls success = %v, want 1", got)
	}
}

func TestObservePluginCallFailed(t *testing.T) {
	c := New()
	c.ObservePluginCall("jira", "create_issue", true)

	if got := testutil.ToFloat64(c.pluginCalls.With(prometheus.Labels{"plugin": "jira", "action": "create_issue", "status": "error"})); got != 1 {
		t.Errorf("plugin calls error = %v, want 1", got)
	}
	// success counter for same pair must be zero (not initialised).
	if n := gatherCount(t, c, "opentalon_plugin_calls_total"); n != 1 {
		t.Errorf("expected 1 series (error only), got %d", n)
	}
}

func TestObservePluginCallAccumulates(t *testing.T) {
	c := New()
	c.ObservePluginCall("gitlab", "analyze_code", false)
	c.ObservePluginCall("gitlab", "analyze_code", false)
	c.ObservePluginCall("gitlab", "analyze_code", true)

	success := testutil.ToFloat64(c.pluginCalls.With(prometheus.Labels{"plugin": "gitlab", "action": "analyze_code", "status": "success"}))
	if success != 2 {
		t.Errorf("success = %v, want 2", success)
	}
	errs := testutil.ToFloat64(c.pluginCalls.With(prometheus.Labels{"plugin": "gitlab", "action": "analyze_code", "status": "error"}))
	if errs != 1 {
		t.Errorf("errors = %v, want 1", errs)
	}
}

func TestHandlerServesMetrics(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 1, 1, 0, 0.0, 0.0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	c.Handler().ServeHTTP(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "opentalon_orchestrator_runs_total") {
		t.Errorf("body does not contain opentalon_orchestrator_runs_total:\n%s", body)
	}
}

func TestCollectAndCompareAfterUsage(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "entity1", "g1", "ch1", "sess1", "m1", 10, 5, 0, 0.0, 0.0)

	expected := `
		# HELP opentalon_orchestrator_runs_total Total completed orchestrator runs.
		# TYPE opentalon_orchestrator_runs_total counter
		opentalon_orchestrator_runs_total{channel="ch1",group="g1",model="m1"} 1
	`
	if err := testutil.GatherAndCompare(c.reg, strings.NewReader(expected), "opentalon_orchestrator_runs_total"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}
