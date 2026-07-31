package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

const (
	// notifyPluginName is the built-in reserved plugin namespace for the
	// background-trigger push entrypoint, mirroring the _escalate / _subprocess
	// / _meta precedent.
	notifyPluginName = "_notify"
	// notifySendAction is the single action on the notify plugin. The
	// fully-qualified name background callers use is "_notify__send".
	notifySendAction = "send"
	// notificationMessageType tags the pushed OutboundMessage so channels and
	// clients can distinguish an agent-initiated notification from a reply to an
	// inbound user message (and from an escalation, which carries a reasoned
	// answer rather than a canned alert).
	notificationMessageType = "agent.notification"
)

// NotifyConfig gates the background-trigger push entrypoint (_notify). Enabled
// is the master switch (default false → ship dark): when false the plugin is
// not registered, so a background caller gets tool_call_not_found. It is gated
// separately from escalation because the two have different costs: escalation
// spends tokens, notification spends the user's attention.
type NotifyConfig struct {
	Enabled bool
}

// ConversationSender pushes an OutboundMessage to an explicit
// (channel, conversation) pair. It is the addressing complement of
// ChannelSender, which can only reach a conversation via a packed session key.
// A background caller that captured a channel/conversation at create time — but
// whose originating session is long gone — needs this path. Optional: nil means
// only the session-keyed path works.
type ConversationSender func(ctx context.Context, channelID, conversationID string, msg pkgchannel.OutboundMessage) error

// notifyRequest is parsed from the _notify.send call args.
type notifyRequest struct {
	// Text is the message to deliver, verbatim. The orchestrator neither
	// generates nor rewrites it — no model is involved on this path.
	Text string
	// SessionID is the preferred target: the packed session key, which the
	// ChannelSender adapter splits back into channel + conversation.
	SessionID string
	// ChannelID/ConversationID address the conversation directly, for a caller
	// that stored them at create time. Used when SessionID is empty.
	ChannelID      string
	ConversationID string
	// EntityID/GroupID name the identity the notification is sent on behalf of.
	// A background trigger runs profile-less, so these are how it declares who
	// it is; EntityID is also cross-checked against an entity-prefixed session
	// key (see checkTargetOwnership).
	EntityID string
	GroupID  string
	// Source/AgentID/Trigger are optional provenance stamped onto the pushed
	// message's Metadata, same contract as escalation.
	Source  string
	AgentID string
	Trigger string
}

// notifyResult is the small JSON status returned to the background caller.
// delivered=false with a reason is a POLICY refusal (disabled, no target, no
// sender wired) — a transport failure comes back as a ToolResult error instead,
// so a caller can tell "we chose not to" from "we tried and it broke".
type notifyResult struct {
	Delivered bool   `json:"delivered"`
	Reason    string `json:"reason,omitempty"`
}

// notifyExecutor implements PluginExecutor for the built-in _notify plugin.
type notifyExecutor struct {
	orch *Orchestrator
}

func (e *notifyExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	// Defense in depth: the action is UserOnly, which already hides it from the
	// LLM tool catalog and blocks LLM-sourced calls. Reject FromLLM explicitly
	// too so a future mis-registration can never let the model message users
	// out of band.
	if call.FromLLM {
		return ToolResult{CallID: call.ID, Error: "notify is not callable by the model"}
	}
	return e.orch.sendNotification(ctx, call)
}

// sendNotification validates the request and pushes the message.
//
// Unlike escalation this runs SYNCHRONOUSLY and starts no turn: it is a single
// channel send, so the caller can learn the outcome instead of firing into the
// dark, and there is no session turn-lock to deadlock on.
func (o *Orchestrator) sendNotification(ctx context.Context, call ToolCall) ToolResult {
	if !o.notifyConfig.Enabled {
		return notifyStatus(call, false, "disabled")
	}

	req, err := parseNotifyRequest(ctx, call.Args)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}
	if reason := o.checkTargetOwnership(req); reason != "" {
		slog.Warn("notification refused: target not owned by the calling entity",
			"entity", req.EntityID, "session_id", req.SessionID, "agent_id", req.AgentID)
		return notifyStatus(call, false, reason)
	}

	msg := pkgchannel.OutboundMessage{Content: req.Text, Metadata: req.messageMetadata()}

	switch {
	case req.SessionID != "":
		if o.channelSender == nil {
			return notifyStatus(call, false, "no_channel_sender")
		}
		// ConversationID left empty: the ChannelSender adapter is the only layer
		// that can split the packed sessionID back into (entity, channel,
		// conversation) — see the title-push note in maybeGenerateTitle.
		if err := o.channelSender(ctx, req.SessionID, msg); err != nil {
			return ToolResult{CallID: call.ID, Error: fmt.Sprintf("notify: push to session %s failed: %v", req.SessionID, err)}
		}
	default:
		if o.conversationSender == nil {
			return notifyStatus(call, false, "no_conversation_sender")
		}
		msg.ConversationID = req.ConversationID
		if err := o.conversationSender(ctx, req.ChannelID, req.ConversationID, msg); err != nil {
			return ToolResult{CallID: call.ID, Error: fmt.Sprintf("notify: push to %s/%s failed: %v", req.ChannelID, req.ConversationID, err)}
		}
	}

	o.recordNotificationInTranscript(req)
	slog.Info("notification pushed", "session_id", req.SessionID, "channel", req.ChannelID,
		"entity", req.EntityID, "agent_id", req.AgentID, "trigger", req.Trigger)
	return notifyStatus(call, true, "")
}

// checkTargetOwnership rejects an entity trying to push into another entity's
// conversation. It can only catch the case it can actually see: a
// profile-resolved session key is "<entity>:<channel>:<conversation>", so when
// the caller names an entity AND the key carries one, they must agree.
//
// A 2-part (anonymous) key or a raw channel/conversation pair carries no owner,
// so nothing can be verified there — those paths rest on _notify being
// operator-enabled and callable only by background plugin code, never the
// model. Returns "" when the target is acceptable.
func (o *Orchestrator) checkTargetOwnership(req notifyRequest) string {
	if req.EntityID == "" || req.SessionID == "" {
		return ""
	}
	parts := strings.Split(req.SessionID, ":")
	if len(parts) < 3 {
		return "" // no owner segment to compare against
	}
	if parts[0] != req.EntityID {
		return "entity_mismatch"
	}
	return ""
}

// recordNotificationInTranscript appends the pushed message to the target
// session so the conversation stays coherent: the user sees the alert in chat,
// and when they reply "why?" the next assistant turn can see what was sent.
// Best-effort — a notification addressed to a bare channel/conversation, or to
// a session that no longer exists, is delivered without a transcript entry
// rather than conjuring a session row.
func (o *Orchestrator) recordNotificationInTranscript(req notifyRequest) {
	if req.SessionID == "" || o.sessions == nil {
		return
	}
	if _, err := o.sessions.Get(req.SessionID); err != nil {
		return
	}
	msg := provider.Message{Role: "assistant", Content: req.Text}
	if err := o.sessions.AddMessageWithMetadata(req.SessionID, msg, req.messageMetadata()); err != nil {
		slog.Warn("notification transcript append failed", "session_id", req.SessionID, "error", err)
	}
}

// messageMetadata builds the Metadata for the pushed notification. `type` is
// always set; source / agent_id / trigger are added only when the caller
// supplied them.
func (r notifyRequest) messageMetadata() map[string]string {
	md := map[string]string{"type": notificationMessageType}
	if r.Source != "" {
		md["source"] = r.Source
	}
	if r.AgentID != "" {
		md["agent_id"] = r.AgentID
	}
	if r.Trigger != "" {
		md["trigger"] = r.Trigger
	}
	return md
}

// parseNotifyRequest reads the send args. text is required. The target is
// either session_id (falling back to the caller's session when omitted) or an
// explicit channel_id + conversation_id pair; a half-specified pair is an error
// rather than a silent drop, since it usually means the caller stored only one
// half at create time.
func parseNotifyRequest(ctx context.Context, args map[string]string) (notifyRequest, error) {
	text := strings.TrimSpace(args["text"])
	if text == "" {
		return notifyRequest{}, fmt.Errorf("notify requires a 'text' argument")
	}
	req := notifyRequest{
		Text:           text,
		SessionID:      strings.TrimSpace(args["session_id"]),
		ChannelID:      strings.TrimSpace(args["channel_id"]),
		ConversationID: strings.TrimSpace(args["conversation_id"]),
		EntityID:       strings.TrimSpace(args["entity_id"]),
		GroupID:        strings.TrimSpace(args["group_id"]),
		Source:         strings.TrimSpace(args["source"]),
		AgentID:        strings.TrimSpace(args["agent_id"]),
		Trigger:        strings.TrimSpace(args["trigger"]),
	}
	if req.SessionID == "" && req.ChannelID == "" && req.ConversationID == "" {
		req.SessionID = actor.SessionID(ctx)
	}
	if req.SessionID != "" {
		return req, nil
	}
	if req.ChannelID == "" || req.ConversationID == "" {
		return notifyRequest{}, fmt.Errorf("notify requires a 'session_id', or both 'channel_id' and 'conversation_id' (got channel_id=%q conversation_id=%q)", req.ChannelID, req.ConversationID)
	}
	return req, nil
}

// notifyStatus returns the status on BOTH Content and StructuredContent. The
// callback path hands a plugin (Content, StructuredContent) as-is, and a plugin
// decoding a JSON status naturally reads StructuredContent — leaving it empty
// makes a refusal indistinguishable from a delivery on the caller's side.
func notifyStatus(call ToolCall, delivered bool, reason string) ToolResult {
	payload, _ := json.Marshal(notifyResult{Delivered: delivered, Reason: reason})
	return ToolResult{CallID: call.ID, Content: string(payload), StructuredContent: string(payload)}
}
