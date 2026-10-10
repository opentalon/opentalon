package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
)

const (
	// decideAction is the single action every configured decider answers. A
	// tln `decide "..." using model "<name>"` block reaches it as the host
	// callback RunAction(ctx, "<name>", "decide", {state, choices}).
	decideAction = "decide"
	// decideModelPrefix namespaces decider spend in profile_usage.model_id so it
	// is distinguishable from chat-provider spend.
	decideModelPrefix = "decide/"
	// unattributedEntity is the profile_usage entity a decision is recorded
	// under when the callback carries no identity (the plugin omitted the
	// __ot_cb_entity_id / __ot_cb_group_id args, or the external gateway
	// path), so its spend is still counted rather than free.
	unattributedEntity = "_unattributed"
)

// CheckDeciderNames fails when a configured decider's name is already a plugin
// in tools, or is one of reserved (plugins configured but not yet loaded, e.g.
// retried later by the plugin manager, and tools registered after the
// orchestrator is built). Decider names share the plugin namespace, so a clash
// would route a tln `decide` callback to the wrong plugin, or make that plugin
// fail to register later. Call it before NewWithRules and treat an error as
// fatal configuration.
func CheckDeciderNames(reg *decideprovider.Registry, tools *ToolRegistry, reserved ...string) error {
	taken := make(map[string]bool, len(reserved))
	for _, r := range reserved {
		taken[r] = true
	}
	for _, name := range reg.Names() {
		if _, ok := tools.GetCapability(name); ok || taken[name] {
			return fmt.Errorf("decider %q clashes with a plugin or built-in tool of the same name; rename the decider", name)
		}
	}
	return nil
}

// registerDeciders registers each configured decider as a built-in plugin
// named after it, with one UserOnly `decide` action: hidden from the LLM tool
// catalog and blocked from LLM-sourced calls, reachable only through a plugin's
// HostCaller.RunAction callback (the _notify / _escalate precedent).
func (o *Orchestrator) registerDeciders(reg *decideprovider.Registry) {
	for _, name := range reg.Names() {
		p, _ := reg.Get(name)
		cap := PluginCapability{
			Name:        name,
			Description: "Typed-decision model: pick one of a fixed set of choices for a state",
			Actions: []Action{{
				Name:        decideAction,
				Description: "Classify state into exactly one of choices; returns {chosen, confidence, probabilities}.",
				UserOnly:    true,
				Parameters: []Parameter{
					{Name: "state", Description: "The text to classify", Required: true},
					{Name: "choices", Description: `JSON array of choice labels, e.g. ["Spam","Legitimate"]`, Required: true},
				},
			}},
		}
		// CheckDeciderNames and decideprovider.ValidateName reject both failure
		// causes at startup; reaching this means the caller skipped them.
		if err := o.registry.Register(cap, &decideExecutor{orch: o, provider: p}); err != nil {
			slog.Error("decider not registered", "decider", name, "error", err)
		}
	}
}

type decideExecutor struct {
	orch     *Orchestrator
	provider decideprovider.Provider
}

func (e *decideExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	if call.FromLLM {
		return ToolResult{CallID: call.ID, Error: "decide is not callable by the model"}
	}
	if call.Action != decideAction {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("decider %q has no action %q", e.provider.ID(), call.Action)}
	}
	req, err := parseDecideRequest(call.Args)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}
	if reason := e.orch.decideOverLimit(ctx); reason != "" {
		return ToolResult{CallID: call.ID, Error: reason}
	}

	// A failed decision is not metered: the backends report usage only on a
	// successful response, and a transport error or malformed reply carries
	// none to record. Deliberate — guessing tokens for a failure would make
	// the budget wrong in the other direction.
	dec, err := e.provider.Decide(ctx, req)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}
	if dec == nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("decider %q returned no decision", e.provider.ID())}
	}
	e.orch.recordDecideUsage(ctx, e.provider.ID(), dec.Usage)

	body, err := json.Marshal(dec)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("encode decision: %v", err)}
	}
	return ToolResult{CallID: call.ID, Content: string(body), StructuredContent: string(body)}
}

// parseDecideRequest validates the callback args. Required-argument checks only
// run for LLM-sourced calls, and correctness must not depend on every backend
// rejecting an empty state or choice set itself.
func parseDecideRequest(args map[string]string) (*decideprovider.Request, error) {
	state := args["state"]
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("decide requires a non-empty state")
	}
	var choices []string
	if err := json.Unmarshal([]byte(args["choices"]), &choices); err != nil {
		return nil, fmt.Errorf("choices must be a JSON array of strings: %v", err)
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("decide requires at least one choice")
	}
	seen := make(map[string]bool, len(choices))
	for _, c := range choices {
		if strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("decide choices must not be empty strings")
		}
		if seen[c] {
			return nil, fmt.Errorf("decide choice %q is listed twice", c)
		}
		seen[c] = true
	}
	return &decideprovider.Request{State: state, Choices: choices}, nil
}

// decideOverLimit gates a decision on the caller's token budget — the same
// Profile.Limit over LimitWindow check the channel handler and _escalate apply
// (UsageLimitChecker; wired as EscalationLimitChecker) — so a tln loop of
// decides cannot run a profile past its limit. Returns the refusal reason, or
// "" to proceed. A checker error fails open, matching the other two gates. A
// call with no profile has no budget to check (it is metered as unattributed).
func (o *Orchestrator) decideOverLimit(ctx context.Context) string {
	p := profile.FromContext(ctx)
	if o.escalationLimit == nil || p == nil || p.EntityID == "" || p.Limit <= 0 || p.LimitWindow <= 0 {
		return ""
	}
	used, err := o.escalationLimit.TotalTokensSince(ctx, p.EntityID, time.Now().Add(-p.LimitWindow))
	if err != nil {
		slog.Warn("decide limit check failed", "entity", p.EntityID, "error", err)
		return ""
	}
	if used >= p.Limit {
		return fmt.Sprintf("token limit reached for entity %q (%d of %d in %s)", p.EntityID, used, p.Limit, p.LimitWindow)
	}
	return ""
}

// recordDecideUsage meters a decision into profile_usage like a chat-LLM run,
// so it counts against the same per-profile limits (TotalTokensSince). A call
// with no profile in context is recorded under unattributedEntity, so its
// spend is visible rather than free, and warned about once per decider.
func (o *Orchestrator) recordDecideUsage(ctx context.Context, decider string, u decideprovider.Usage) {
	o.observePluginUsage(ctx, decideModelPrefix+decider, provider.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
	if o.usageRecorder == nil {
		return
	}
	p := profile.FromContext(ctx)
	if p == nil || p.EntityID == "" {
		if _, warned := o.decideUnattributed.LoadOrStore(decider, true); !warned {
			slog.Warn("decide callback carries no identity; metering under the unattributed entity",
				"decider", decider, "entity", unattributedEntity)
		}
		o.usageRecorder.RecordUsage(ctx, unattributedEntity, "", "", actor.SessionID(ctx),
			decideModelPrefix+decider, profile.KindSystem, "decide", u.InputTokens, u.OutputTokens, 0)
		return
	}
	o.usageRecorder.RecordUsage(ctx, p.EntityID, p.Group, p.ChannelID, actor.SessionID(ctx),
		decideModelPrefix+decider, p.Kind, p.SystemSource, u.InputTokens, u.OutputTokens, 0)
}
