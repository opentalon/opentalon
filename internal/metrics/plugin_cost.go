package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

type pluginCost struct {
	mu      sync.RWMutex
	enabled map[string]*pluginAlias

	tokens *prometheus.CounterVec
	cost   *prometheus.CounterVec
}

type pluginAlias struct {
	tokens *prometheus.CounterVec
	cost   *prometheus.CounterVec
}

var metricPrefix = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func newPluginCost(reg prometheus.Registerer) *pluginCost {
	p := &pluginCost{
		enabled: map[string]*pluginAlias{},
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_plugin_llm_tokens_total",
			Help: "LLM tokens used on behalf of a plugin (its _subprocess and decide calls), by action, stage, model and direction. Stage is the plugin's own label for the step, \"\" when unset. Only for plugins with metrics.cost enabled.",
		}, []string{"plugin", "action", "stage", "model", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_plugin_llm_cost_usd_total",
			Help: "Cost of LLM calls made on behalf of a plugin, by action, stage and model, priced with the model's configured cost. Stage is the plugin's own label for the step, \"\" when unset. Only for plugins with metrics.cost enabled.",
		}, []string{"plugin", "action", "stage", "model"}),
	}
	reg.MustRegister(p.tokens, p.cost)
	return p
}

func (c *Collector) EnablePluginCost(plugin, prefix string) error {
	p := c.pluginCost
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.enabled[plugin]; dup {
		return fmt.Errorf("plugin %q: metrics cost already enabled", plugin)
	}
	var alias *pluginAlias
	if prefix != "" {
		if !metricPrefix.MatchString(prefix) {
			return fmt.Errorf("plugin %q: metrics.prefix %q is not a valid Prometheus name prefix", plugin, prefix)
		}
		alias = &pluginAlias{
			tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: prefix + "_llm_tokens_total",
				Help: "LLM tokens used by the " + plugin + " plugin, by action, stage, model and direction (alias of opentalon_plugin_llm_tokens_total).",
			}, []string{"action", "stage", "model", "direction"}),
			cost: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: prefix + "_llm_cost_usd_total",
				Help: "Cost of LLM calls made by the " + plugin + " plugin, by action, stage and model (alias of opentalon_plugin_llm_cost_usd_total).",
			}, []string{"action", "stage", "model"}),
		}
		if err := c.reg.Register(alias.tokens); err != nil {
			return fmt.Errorf("plugin %q: metrics.prefix %q: %w", plugin, prefix, err)
		}
		if err := c.reg.Register(alias.cost); err != nil {
			c.reg.Unregister(alias.tokens)
			return fmt.Errorf("plugin %q: metrics.prefix %q: %w", plugin, prefix, err)
		}
	}
	p.enabled[plugin] = alias
	return nil
}

func (c *Collector) PluginCostEnabled(plugin string) bool {
	c.pluginCost.mu.RLock()
	defer c.pluginCost.mu.RUnlock()
	_, ok := c.pluginCost.enabled[plugin]
	return ok
}

func (c *Collector) ObservePluginUsage(_ context.Context, plugin, action, stage, model string, inputTokens, outputTokens int, costUSD float64) {
	p := c.pluginCost
	p.mu.RLock()
	alias, ok := p.enabled[plugin]
	p.mu.RUnlock()
	if !ok {
		return
	}
	p.tokens.WithLabelValues(plugin, action, stage, model, "input").Add(float64(inputTokens))
	p.tokens.WithLabelValues(plugin, action, stage, model, "output").Add(float64(outputTokens))
	p.cost.WithLabelValues(plugin, action, stage, model).Add(costUSD)
	if alias != nil {
		alias.tokens.WithLabelValues(action, stage, model, "input").Add(float64(inputTokens))
		alias.tokens.WithLabelValues(action, stage, model, "output").Add(float64(outputTokens))
		alias.cost.WithLabelValues(action, stage, model).Add(costUSD)
	}
}
