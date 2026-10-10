package metrics

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Per-plugin LLM spend — opt-in per plugin (plugins.<name>.metrics.cost), so
// installs that don't ask for it keep exactly the series they had.
//
//	opentalon_plugin_llm_tokens_total{plugin, action, model, direction}
//	opentalon_plugin_cost_usd_total{plugin, action, model}
//
// With a prefix (plugins.<name>.metrics.prefix) the same numbers are also
// exported under the plugin's own name, without the plugin label:
//
//	<prefix>_llm_tokens_total{action, model, direction}
//	<prefix>_llm_cost_usd_total{action, model}
type pluginCost struct {
	mu      sync.RWMutex
	enabled map[string]*pluginAlias // plugin → alias (nil when it has no prefix)

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
			Help: "LLM tokens used on behalf of a plugin (its _subprocess and decide calls), by action, model and direction. Only for plugins with metrics.cost enabled.",
		}, []string{"plugin", "action", "model", "direction"}),
		cost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "opentalon_plugin_cost_usd_total",
			Help: "Cost of LLM calls made on behalf of a plugin, by action and model, priced with the model's configured cost. Only for plugins with metrics.cost enabled.",
		}, []string{"plugin", "action", "model"}),
	}
	reg.MustRegister(p.tokens, p.cost)
	return p
}

// EnablePluginCost turns on per-plugin LLM spend metrics for plugin, and with
// a non-empty prefix also the <prefix>_llm_* alias. Call it at startup, once
// per plugin.
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
				Help: "LLM tokens used by the " + plugin + " plugin, by action, model and direction (alias of opentalon_plugin_llm_tokens_total).",
			}, []string{"action", "model", "direction"}),
			cost: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: prefix + "_llm_cost_usd_total",
				Help: "Cost of LLM calls made by the " + plugin + " plugin, by action and model (alias of opentalon_plugin_cost_usd_total).",
			}, []string{"action", "model"}),
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

// PluginCostEnabled reports whether plugin has per-plugin spend metrics on.
func (c *Collector) PluginCostEnabled(plugin string) bool {
	c.pluginCost.mu.RLock()
	defer c.pluginCost.mu.RUnlock()
	_, ok := c.pluginCost.enabled[plugin]
	return ok
}

// ObservePluginUsage records one priced LLM call made for plugin's action. It
// is a no-op for plugins without metrics.cost.
func (c *Collector) ObservePluginUsage(_ context.Context, plugin, action, model string, inputTokens, outputTokens int, costUSD float64) {
	p := c.pluginCost
	p.mu.RLock()
	alias, ok := p.enabled[plugin]
	p.mu.RUnlock()
	if !ok {
		return
	}
	p.tokens.WithLabelValues(plugin, action, model, "input").Add(float64(inputTokens))
	p.tokens.WithLabelValues(plugin, action, model, "output").Add(float64(outputTokens))
	p.cost.WithLabelValues(plugin, action, model).Add(costUSD)
	if alias != nil {
		alias.tokens.WithLabelValues(action, model, "input").Add(float64(inputTokens))
		alias.tokens.WithLabelValues(action, model, "output").Add(float64(outputTokens))
		alias.cost.WithLabelValues(action, model).Add(costUSD)
	}
}
