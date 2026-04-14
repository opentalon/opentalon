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
	if got := testutil.ToFloat64(c.llmRequests.With(labels)); got != 1 {
		t.Errorf("requests = %v, want 1", got)
	}
	if got := testutil.ToFloat64(c.llmCostUSD.With(prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1", "type": "input"})); got != 0.01 {
		t.Errorf("cost input = %v, want 0.01", got)
	}
	if got := testutil.ToFloat64(c.llmCostUSD.With(prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1", "type": "output"})); got != 0.02 {
		t.Errorf("cost output = %v, want 0.02", got)
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
	if got := testutil.ToFloat64(c.llmRequests.With(labels)); got != 2 {
		t.Errorf("requests = %v, want 2", got)
	}
}

func TestRecordUsageZeroCostNotRecorded(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 10, 5, 0, 0.0, 0.0)

	// With zero costs, no label combinations should be initialised for cost metric.
	if n := gatherCount(t, c, "opentalon_llm_cost_usd_total"); n != 0 {
		t.Errorf("expected 0 cost series, got %d", n)
	}
}

func TestRecordUsagePartialCost(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "e", "g1", "ch1", "s", "m1", 10, 5, 0, 0.05, 0.0)

	// Only the "input" type should be initialised.
	if n := gatherCount(t, c, "opentalon_llm_cost_usd_total"); n != 1 {
		t.Errorf("expected 1 cost series (input only), got %d", n)
	}
	if got := testutil.ToFloat64(c.llmCostUSD.With(prometheus.Labels{"model": "m1", "channel": "ch1", "group": "g1", "type": "input"})); got != 0.05 {
		t.Errorf("cost input = %v, want 0.05", got)
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
	if !strings.Contains(string(body), "opentalon_llm_requests_total") {
		t.Errorf("body does not contain opentalon_llm_requests_total:\n%s", body)
	}
}

func TestCollectAndCompareAfterUsage(t *testing.T) {
	c := New()
	c.RecordUsage(context.Background(), "entity1", "g1", "ch1", "sess1", "m1", 10, 5, 0, 0.0, 0.0)

	expected := `
		# HELP opentalon_llm_requests_total Total completed LLM orchestrator runs.
		# TYPE opentalon_llm_requests_total counter
		opentalon_llm_requests_total{channel="ch1",group="g1",model="m1"} 1
	`
	if err := testutil.GatherAndCompare(c.reg, strings.NewReader(expected), "opentalon_llm_requests_total"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}
