package decideprovider

import (
	"fmt"
	"math"
	"regexp"
	"sort"
)

// DeciderConfig is one named decision-model backend, mirroring
// provider.ProviderConfig on the decision side. It is populated from the
// `deciders:` block of config.yaml (see config.DeciderConfig).
type DeciderConfig struct {
	// Name is the decider's identifier — the `using model "<name>"` string in
	// .tln and the server name a `decide` callback arrives under.
	Name string
	// Backend selects the implementation: "laya" | "jev" | "subq" | "local-logits".
	Backend string
	// BaseURL is the backend endpoint. Required for every backend.
	BaseURL string
	// APIKey is an optional bearer token (Jev, SubQ) / auth (laya, local-logits).
	APIKey string
	// Model is the served model id — used by local-logits, Jev (default
	// jev-latest) and SubQ (default subq-snap-preview); ignored by laya, which
	// serves a fixed model per endpoint.
	Model string
	// Cost is the price per 1M tokens its usage is metered at; zero = unpriced.
	Cost Cost
}

// nameRe is the decider-name charset. A decider is registered as a plugin
// named after it, so `<name>__decide` must pass the provider tool-name charset
// (^[a-zA-Z0-9_-]{1,64}$); a leading underscore is reserved for built-in
// plugins (_subprocess, _notify, …).
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,55}$`)

// ValidateName reports whether name can be a decider name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("decideprovider: invalid decider name %q: must match %s (letters, digits, '-', '_'; no leading '_'; at most 56 chars)",
			name, nameRe.String())
	}
	return nil
}

// FromConfig builds a single Provider from cfg, dispatching on Backend.
func FromConfig(cfg DeciderConfig) (Provider, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("decideprovider: decider requires a name")
	}
	if err := ValidateName(cfg.Name); err != nil {
		return nil, err
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("decideprovider: decider %q requires a base_url", cfg.Name)
	}
	// A negative price would make the cost counters decrease, which panics in
	// Prometheus on the first decide call; reject it at startup instead.
	for _, c := range []struct {
		name  string
		price float64
	}{{"input", cfg.Cost.Input}, {"output", cfg.Cost.Output}} {
		if c.price < 0 || math.IsNaN(c.price) || math.IsInf(c.price, 0) {
			return nil, fmt.Errorf("decideprovider: decider %q cost.%s = %v: want a price per 1M tokens of 0 or more", cfg.Name, c.name, c.price)
		}
	}
	switch cfg.Backend {
	case BackendLaya:
		return NewLayaProvider(cfg.Name, cfg.BaseURL, cfg.APIKey), nil
	case BackendJev:
		return NewJevProvider(cfg.Name, cfg.BaseURL, cfg.APIKey, cfg.Model), nil
	case BackendSubQ:
		return NewSubQProvider(cfg.Name, cfg.BaseURL, cfg.APIKey, cfg.Model), nil
	case BackendLocalLogits:
		return NewLocalLogitsProvider(cfg.Name, cfg.BaseURL, cfg.APIKey, cfg.Model), nil
	case "":
		return nil, fmt.Errorf("decideprovider: decider %q requires a backend (%s, %s, %s, %s)",
			cfg.Name, BackendLaya, BackendJev, BackendSubQ, BackendLocalLogits)
	default:
		return nil, fmt.Errorf("decideprovider: decider %q has unknown backend %q (supported: %s, %s, %s, %s)",
			cfg.Name, cfg.Backend, BackendLaya, BackendJev, BackendSubQ, BackendLocalLogits)
	}
}

// RegistryFromConfigs builds a Registry from all configured deciders. Names are
// processed in sorted order so an error is deterministic. A duplicate name is an
// error — the routing predicate must be unambiguous.
func RegistryFromConfigs(cfgs map[string]DeciderConfig) (*Registry, error) {
	names := make([]string, 0, len(cfgs))
	for name := range cfgs {
		names = append(names, name)
	}
	sort.Strings(names)

	providers := make([]Provider, 0, len(cfgs))
	costs := make(map[string]Cost, len(cfgs))
	seen := make(map[string]bool, len(cfgs))
	for _, name := range names {
		cfg := cfgs[name]
		if cfg.Name == "" {
			cfg.Name = name // allow the map key to supply the name
		}
		if seen[cfg.Name] {
			return nil, fmt.Errorf("decideprovider: duplicate decider name %q", cfg.Name)
		}
		p, err := FromConfig(cfg)
		if err != nil {
			return nil, err
		}
		seen[cfg.Name] = true
		providers = append(providers, p)
		costs[cfg.Name] = cfg.Cost
	}
	return NewRegistry(providers...).WithCosts(costs), nil
}
