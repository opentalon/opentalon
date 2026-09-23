package decideprovider

import (
	"fmt"
	"sort"
)

// DeciderConfig is one named decision-model backend, mirroring
// provider.ProviderConfig on the decision side. It is populated from the
// `deciders:` block of config.yaml (see config.DeciderConfig).
type DeciderConfig struct {
	// Name is the decider's identifier — the `using model "<name>"` string in
	// .tln and the server name a `decide` callback arrives under.
	Name string
	// Backend selects the implementation: "laya" | "jev" | "local-logits".
	Backend string
	// BaseURL is the backend endpoint. Required for every backend.
	BaseURL string
	// APIKey is an optional bearer token (Jev) / auth (laya, local-logits).
	APIKey string
	// Model is the served model id — used by local-logits; ignored by
	// laya/Jev, which serve a fixed model per endpoint.
	Model string
}

// FromConfig builds a single Provider from cfg, dispatching on Backend.
func FromConfig(cfg DeciderConfig) (Provider, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("decideprovider: decider requires a name")
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("decideprovider: decider %q requires a base_url", cfg.Name)
	}
	switch cfg.Backend {
	case BackendLaya:
		return NewLayaProvider(cfg.Name, cfg.BaseURL, cfg.APIKey), nil
	case BackendJev:
		return NewJevProvider(cfg.Name, cfg.BaseURL, cfg.APIKey), nil
	case BackendLocalLogits:
		return NewLocalLogitsProvider(cfg.Name, cfg.BaseURL, cfg.APIKey, cfg.Model), nil
	case "":
		return nil, fmt.Errorf("decideprovider: decider %q requires a backend (%s, %s, %s)",
			cfg.Name, BackendLaya, BackendJev, BackendLocalLogits)
	default:
		return nil, fmt.Errorf("decideprovider: decider %q has unknown backend %q (supported: %s, %s, %s)",
			cfg.Name, cfg.Backend, BackendLaya, BackendJev, BackendLocalLogits)
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
	}
	return NewRegistry(providers...), nil
}
