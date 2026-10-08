package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/profile"
)

const (
	// decideAction is the single action every configured decider answers. A
	// tln `decide "..." using model "<name>"` block reaches it as the host
	// callback RunAction(ctx, "<name>", "decide", {state, choices}).
	decideAction = "decide"
	// decideModelPrefix namespaces decider spend in profile_usage.model_id so it
	// is distinguishable from chat-provider spend.
	decideModelPrefix = "decide/"
)

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
		if err := o.registry.Register(cap, &decideExecutor{orch: o, provider: p}); err != nil {
			slog.Warn("decider not registered", "decider", name, "error", err)
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
	var choices []string
	if err := json.Unmarshal([]byte(call.Args["choices"]), &choices); err != nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("choices must be a JSON array of strings: %v", err)}
	}

	dec, err := e.provider.Decide(ctx, &decideprovider.Request{State: call.Args["state"], Choices: choices})
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}
	e.orch.recordDecideUsage(ctx, e.provider.ID(), dec.Usage)

	body, err := json.Marshal(dec)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("encode decision: %v", err)}
	}
	return ToolResult{CallID: call.ID, Content: string(body), StructuredContent: string(body)}
}

// recordDecideUsage meters a decision into profile_usage like a chat-LLM run,
// so it counts against the same per-profile limits (TotalTokensSince). A call
// with no profile in context (no billable entity) is not recorded.
func (o *Orchestrator) recordDecideUsage(ctx context.Context, decider string, u decideprovider.Usage) {
	if o.usageRecorder == nil {
		return
	}
	p := profile.FromContext(ctx)
	if p == nil {
		return
	}
	o.usageRecorder.RecordUsage(ctx, p.EntityID, p.Group, p.ChannelID, actor.SessionID(ctx),
		decideModelPrefix+decider, p.Kind, p.SystemSource, u.InputTokens, u.OutputTokens, 0)
}
