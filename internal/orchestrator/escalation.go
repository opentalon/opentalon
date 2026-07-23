package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

const (
	// escalatePluginName is the built-in reserved plugin namespace for the
	// background-trigger turn entrypoint. The underscore prefix mirrors the
	// _subprocess / _meta precedent — distinguishes a built-in from a
	// user-configured plugin.
	escalatePluginName = "_escalate"
	// escalateTurnAction is the single action on the escalate plugin. The
	// fully-qualified name background callers use is "_escalate__turn".
	escalateTurnAction = "turn"
	// escalationMessageType tags the pushed OutboundMessage so channels and
	// clients can distinguish an agent-initiated escalation from a reply to an
	// inbound user message.
	escalationMessageType = "agent.escalation"
)

// EscalationConfig gates the background-trigger turn entrypoint (_escalate).
// Enabled is the master switch (default false → ship dark): when false the
// plugin is not registered, so a background caller gets tool_call_not_found.
type EscalationConfig struct {
	Enabled bool
}

// UsageLimitChecker reports an entity's summed chat-token spend since a cutoff.
// It is the read side of the same rolling-window query the channel handler uses
// to enforce Profile.Limit; the escalation path reuses it to pre-check a
// background turn against the entity's budget. *store.UsageStore satisfies it
// structurally, so no import of internal/state or internal/channel is needed.
type UsageLimitChecker interface {
	TotalTokensSince(ctx context.Context, entityID string, since time.Time) (int, error)
}

// escalationRequest is parsed from the _escalate.turn call args.
type escalationRequest struct {
	SessionID string
	Prompt    string
}

// escalationResult is the small JSON status the executor returns synchronously
// to the background caller. The turn itself runs asynchronously; escalated=true
// means "accepted and spawned", not "the turn finished".
type escalationResult struct {
	Escalated bool   `json:"escalated"`
	Reason    string `json:"reason,omitempty"`
}

// escalationExecutor implements PluginExecutor for the built-in _escalate plugin.
// The closure-over-Orchestrator pattern matches subprocessExecutor.
type escalationExecutor struct {
	orch *Orchestrator
}

func (e *escalationExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	// Defense in depth: the action is UserOnly, which already hides it from the
	// LLM tool catalog and blocks LLM-sourced calls. Reject FromLLM explicitly
	// too so a future mis-registration can never let the model fork background
	// turns.
	if call.FromLLM {
		return ToolResult{CallID: call.ID, Error: "escalation is not callable by the model"}
	}
	return e.orch.startEscalation(ctx, call)
}

// startEscalation validates the request, applies the opt-in / rate-limit / in-flight
// gates, and spawns the turn. It returns immediately — the turn runs in the
// background (see runEscalation) because running it inline would block the
// caller's ExecuteBidi stream on the target session's turn lock.
func (o *Orchestrator) startEscalation(ctx context.Context, call ToolCall) ToolResult {
	if !o.escalationConfig.Enabled {
		return escalationStatus(call, false, "disabled")
	}

	req, err := parseEscalationRequest(ctx, call.Args)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}

	// A background turn needs a verified identity to attribute usage, scope the
	// session, and bound spend. The caller (a deterministic tick / job) must run
	// under the target entity's profile — without one we can't safely run or
	// bill a turn.
	p := profile.FromContext(ctx)
	if p == nil || p.EntityID == "" {
		return escalationStatus(call, false, "no_profile")
	}

	// Rate limit: reuse the entity's chat token budget (Profile.Limit over
	// LimitWindow) — the same check the channel handler applies to inbound
	// messages. Escalation spend is recorded as kind=chat (see runEscalation),
	// so a background turn competes with, and is bounded by, the user's own
	// budget. A checker error is logged and does not block (fail-open, matching
	// the handler).
	if o.escalationLimit != nil && p.Limit > 0 && p.LimitWindow > 0 {
		since := time.Now().Add(-p.LimitWindow)
		used, lerr := o.escalationLimit.TotalTokensSince(ctx, p.EntityID, since)
		if lerr != nil {
			slog.Warn("escalation limit check failed", "entity", p.EntityID, "error", lerr)
		} else if used >= p.Limit {
			slog.Info("escalation skipped: token limit reached",
				"entity", p.EntityID, "used", used, "limit", p.Limit, "window", p.LimitWindow)
			return escalationStatus(call, false, "limit")
		}
	}

	// In-flight guard: at most one escalation per session at a time. A second
	// trip while one runs is dropped rather than queued — otherwise a flapping
	// trigger stacks goroutines, each a full (token-spending) LLM turn.
	entry, ok := o.escalationMuxes.tryLock(req.SessionID)
	if !ok {
		return escalationStatus(call, false, "in_flight")
	}

	// Snapshot the identity for the detached goroutine. Force Kind=chat so usage
	// records against the shared chat budget and title generation still runs;
	// clear SystemSource for the same reason.
	turnProfile := *p
	turnProfile.Kind = profile.KindChat
	turnProfile.SystemSource = ""

	go o.runEscalation(req, turnProfile, entry)

	return escalationStatus(call, true, "")
}

// runEscalation runs the turn on a detached context and pushes the reply to the
// session's channel. Mirrors maybeGenerateTitle's background-goroutine pattern:
// the caller's ctx is cancelled when its ExecuteBidi stream closes, so a fresh
// context.Background() carries the re-attached identity instead.
func (o *Orchestrator) runEscalation(req escalationRequest, p profile.Profile, entry *keyedMutexEntry) {
	defer o.escalationMuxes.unlock(req.SessionID, entry)

	ctx := context.Background()
	ctx = profile.WithProfile(ctx, &p)
	ctx = actor.WithActor(ctx, p.EntityID)
	ctx = actor.WithGroupID(ctx, p.Group)
	ctx = actor.WithSessionID(ctx, req.SessionID)
	// The seed message is fed to the model but dropped from the user-facing
	// transcript; the assistant's reply is always visible.
	ctx = actor.WithVisibility(ctx, provider.VisibilityHidden)

	result, err := o.Run(ctx, req.SessionID, req.Prompt)
	if err != nil {
		slog.Warn("escalation turn failed", "session_id", req.SessionID, "entity", p.EntityID, "error", err)
		return
	}
	if result == nil || result.Response == "" {
		return
	}

	if o.channelSender == nil {
		// No live push wired: the assistant reply already persisted to the
		// session and surfaces on the next transcript load.
		slog.Info("escalation reply not pushed: no channel sender", "session_id", req.SessionID)
		return
	}

	// ConversationID left empty: the ChannelSender adapter is the only layer
	// that can split the packed sessionID back into (entity, channel,
	// conversation) — see the title-push note in maybeGenerateTitle.
	if pushErr := o.channelSender(ctx, req.SessionID, pkgchannel.OutboundMessage{
		Content:  result.Response,
		Metadata: map[string]string{"type": escalationMessageType},
	}); pushErr != nil {
		slog.Warn("escalation reply push failed", "session_id", req.SessionID, "error", pushErr)
	}
}

// parseEscalationRequest reads the turn args. prompt is required; session_id
// defaults to the caller's session when omitted (a tick already scoped to one
// session need not repeat it).
func parseEscalationRequest(ctx context.Context, args map[string]string) (escalationRequest, error) {
	prompt := strings.TrimSpace(args["prompt"])
	if prompt == "" {
		return escalationRequest{}, fmt.Errorf("escalation requires a 'prompt' argument")
	}
	sessionID := strings.TrimSpace(args["session_id"])
	if sessionID == "" {
		sessionID = actor.SessionID(ctx)
	}
	if sessionID == "" {
		return escalationRequest{}, fmt.Errorf("escalation requires a 'session_id' argument (no session in context)")
	}
	return escalationRequest{SessionID: sessionID, Prompt: prompt}, nil
}

func escalationStatus(call ToolCall, escalated bool, reason string) ToolResult {
	payload, _ := json.Marshal(escalationResult{Escalated: escalated, Reason: reason})
	return ToolResult{CallID: call.ID, Content: string(payload)}
}
