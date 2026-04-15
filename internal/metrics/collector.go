package metrics

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector holds all OpenTalon Prometheus metrics and implements
// orchestrator.UsageRecorder and orchestrator.PluginCallObserver.
type Collector struct {
	reg *prometheus.Registry

	llmInputTokens    *prometheus.CounterVec
	llmOutputTokens   *prometheus.CounterVec
	llmInputCostUSD   *prometheus.CounterVec
	llmOutputCostUSD  *prometheus.CounterVec
	orchestratorRuns  *prometheus.CounterVec
	pluginCalls       *prometheus.CounterVec
}

// New creates and registers all metrics.
func New() *Collector {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	c := &Collector{
		reg: reg,

		llmInputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_llm_input_tokens_total",
			Help: "Total LLM input tokens consumed.",
		}, []string{"model", "channel", "group"}),

		llmOutputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_llm_output_tokens_total",
			Help: "Total LLM output tokens produced.",
		}, []string{"model", "channel", "group"}),

		llmInputCostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_llm_input_cost_usd_total",
			Help: "Total LLM input spend in USD.",
		}, []string{"model", "channel", "group"}),

		llmOutputCostUSD: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_llm_output_cost_usd_total",
			Help: "Total LLM output spend in USD.",
		}, []string{"model", "channel", "group"}),

		orchestratorRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_orchestrator_runs_total",
			Help: "Total completed orchestrator runs.",
		}, []string{"model", "channel", "group"}),

		pluginCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_plugin_calls_total",
			Help: "Total plugin/tool calls executed by the orchestrator.",
		}, []string{"plugin", "action", "status"}),
	}

	reg.MustRegister(
		c.llmInputTokens,
		c.llmOutputTokens,
		c.llmInputCostUSD,
		c.llmOutputCostUSD,
		c.orchestratorRuns,
		c.pluginCalls,
	)

	return c
}

// RecordUsage implements orchestrator.UsageRecorder.
func (c *Collector) RecordUsage(_ context.Context, _, groupID, channelID, _, modelID string, inputTokens, outputTokens, _ int, inputCostUSD, outputCostUSD float64) {
	labels := prometheus.Labels{
		"model":   modelID,
		"channel": channelID,
		"group":   groupID,
	}
	c.llmInputTokens.With(labels).Add(float64(inputTokens))
	c.llmOutputTokens.With(labels).Add(float64(outputTokens))
	c.orchestratorRuns.With(labels).Inc()

	c.llmInputCostUSD.With(labels).Add(inputCostUSD)
	c.llmOutputCostUSD.With(labels).Add(outputCostUSD)
}

// ObservePluginCall implements orchestrator.PluginCallObserver.
func (c *Collector) ObservePluginCall(plugin, action string, failed bool) {
	status := "success"
	if failed {
		status = "error"
	}
	c.pluginCalls.With(prometheus.Labels{
		"plugin": plugin,
		"action": action,
		"status": status,
	}).Inc()
}

// Handler returns an http.Handler that serves the /metrics endpoint.
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.reg, promhttp.HandlerOpts{})
}
