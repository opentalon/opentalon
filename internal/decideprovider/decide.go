// Package decideprovider is opentalon's client layer for external "System 1"
// typed-decision models — the host side of tln's `decide "..." using model "m"`
// block (tln-language PR #205, opentalon issue #361).
//
// A tln program's `decide` step is forwarded by the tln-plugin runtime as a
// host callback: RunAction(ctx, "<model>", "decide", {state, choices}). Core
// resolves that name to a Provider here, runs the decision through the chosen
// backend (laya / Jev / local-logits), and records the call's token usage in
// profile_usage just like every provider.Provider LLM call — so decision spend
// is metered and gated by the same per-profile limits.
//
// Every backend returns the same shape the executor expects:
//
//	{ chosen: string, confidence: float64, probabilities: map[string]float64 }
//
// so backends are swappable via config with no change to the .tln source.
package decideprovider

import "context"

// Request is one typed-decision query: pick one of Choices for State.
type Request struct {
	// State is the rendered `ask` expression — the text the model classifies.
	State string
	// Choices is the fixed, declared option set. The returned distribution is
	// over exactly these labels (see normalizeProbabilities).
	Choices []string
}

// Usage is the token accounting for a decision, mirroring provider.Usage so a
// Decision can feed store.UsageRecord unchanged. Pure-classifier backends
// (laya) report OutputTokens == 0; they still report InputTokens so input cost
// is attributable.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Decision is a calibrated distribution over the declared choices plus the
// argmax and its confidence. Invariants (enforced by normalizeProbabilities /
// softmaxLogits):
//
//   - Probabilities has exactly one entry per declared choice and sums to 1.
//   - Chosen is the argmax, tie-broken by declared choice order (determinism,
//     ADR-0001).
//   - Confidence == Probabilities[Chosen], so the executor's `confidence >=`
//     gate is a threshold on the chosen option's mass.
type Decision struct {
	Chosen        string             `json:"chosen"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Usage         Usage              `json:"-"`
}

// Provider is one decision-model backend bound to a name. It parallels
// provider.Provider (the chat-LLM interface) on the decision side.
type Provider interface {
	// ID is the configured decider name (the `using model "..."` string).
	ID() string
	// Decide returns a calibrated Decision for req. Transport failures, HTTP
	// errors, and empty/invalid choice sets are returned as errors so the
	// executor can apply the block's on_error policy.
	Decide(ctx context.Context, req *Request) (*Decision, error)
}

// Registry maps configured decider names to their Provider. It is read-only
// after construction and safe for concurrent use.
type Registry struct {
	byName map[string]Provider
}

// NewRegistry builds a Registry from the given providers, keyed by ID().
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p != nil {
			r.byName[p.ID()] = p
		}
	}
	return r
}

// Get returns the provider bound to name and whether it exists.
func (r *Registry) Get(name string) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byName[name]
	return p, ok
}

// Has reports whether name is a configured decider — the routing predicate the
// orchestrator uses to send a `decide` call here instead of the plugin gateway.
func (r *Registry) Has(name string) bool {
	_, ok := r.Get(name)
	return ok
}
