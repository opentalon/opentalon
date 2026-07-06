package channel

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
	pkg "github.com/opentalon/opentalon/pkg/channel"
)

// ProfileVerifier is the subset of profile.Verifier used by the handler.
type ProfileVerifier interface {
	Verify(ctx context.Context, token, channelType string, metadata map[string]string) (*profile.Profile, error)
}

// LimitChecker checks how many tokens an entity has consumed within a rolling window.
type LimitChecker interface {
	TotalTokensSince(ctx context.Context, entityID string, since time.Time) (int, error)
}

// HandlerConfig holds the dependencies for NewMessageHandler.
type HandlerConfig struct {
	// ResumeSession is the strict-resume path: error if the session is not
	// in the store. Called when the inbound message carries
	// pkg.ResumeIntentMetadataKey="true" (i.e. the channel got a
	// conversation_id from the client). Required — see NewMessageHandler.
	ResumeSession pkg.ResumeSessionFunc
	// CreateSession is the fresh-mint path: idempotent registration of a
	// new session for the given key/entity/group. Called when the inbound
	// message does NOT carry resume_intent=true (channel-minted id).
	// Required — see NewMessageHandler.
	CreateSession pkg.CreateSessionFunc
	Runner        pkg.Runner
	RunAction     pkg.RunActionFunc
	HasAction     pkg.HasActionFunc
	Verifier      ProfileVerifier // nil disables profile verification
	LimitChecker  LimitChecker    // nil disables token spend enforcement
	// PendingConfirmation, when set, is consulted on a resume_hello control
	// message: if the (scoped) session still has a tool call awaiting the
	// user's approval, it returns the prompt text plus the confirmation-frame
	// metadata to re-emit so the reconnected client can redraw the buttons.
	// ok=false means nothing is pending. Read-only — it MUST NOT consume or
	// mutate pending state. nil disables confirmation re-emit on resume.
	PendingConfirmation func(sessionKey string) (content string, metadata map[string]string, ok bool)
}

// NewMessageHandler returns a MessageHandler that: ensures session, verifies profile token (if
// Verifier is non-nil), runs channel-specific content preparer (if registered), then runs the
// message through the Runner and returns the response.
//
// Panics at construction if ResumeSession, CreateSession, or Runner is nil:
// these are required for the strict-session contract and a missing one is a
// boot-time misconfiguration that must surface immediately rather than as a
// nil-deref under load. Verifier and LimitChecker remain optional.
func NewMessageHandler(cfg HandlerConfig) pkg.MessageHandler {
	if cfg.ResumeSession == nil {
		panic("channel.NewMessageHandler: ResumeSession is required")
	}
	if cfg.CreateSession == nil {
		panic("channel.NewMessageHandler: CreateSession is required")
	}
	if cfg.Runner == nil {
		panic("channel.NewMessageHandler: Runner is required")
	}
	return func(ctx context.Context, sessionKey string, msg pkg.InboundMessage) (pkg.OutboundMessage, error) {
		var entityID, groupID string

		// Inbound enrichment is fail-closed: if the channel adapter
		// couldn't fetch the data the WhoAmI server (or LLM) is going
		// to need, refuse the request rather than serve a half-known
		// identity. The metadata flags are stamped by yaml_ws.go after
		// runEnrich returns an error. Checked first so we don't bother
		// the verifier with a message we already know we're rejecting.
		if reason := msg.Metadata[enrichmentFailedKey]; reason != "" {
			step := msg.Metadata[enrichmentFailedStepKey]
			slog.Warn("inbound enrichment failed; rejecting message",
				"channel", msg.ChannelID, "step", step, "reason", reason)
			return errorFrame(msg, "We couldn't verify your account info right now. Please try again in a moment.", "enrichment_failed"), nil
		}

		// interaction_kind for a session minted on this connection; a verified
		// profile may override it below (a system invocation sets "system").
		interactionKind := "chat"

		// Profile verification: required when verifier is configured.
		if cfg.Verifier != nil {
			token := msg.Metadata["profile_token"]
			if token == "" {
				return errorFrame(msg, "profile token required", "token_required"), nil
			}
			// channelType passes the channel KIND (e.g. "slack"), not the
			// instance id. Different bot instances of the same kind should
			// reach WhoAmI as channel_type=slack; the instance is forwarded
			// separately via metadata_headers (e.g. msg.Metadata["channel_id"]
			// → X-Channel-Id) so WhoAmI can grant different permissions per
			// bot while still keying type-shaped behaviour on the kind.
			p, err := cfg.Verifier.Verify(ctx, token, kindOf(msg), msg.Metadata)
			if err != nil {
				slog.Warn("profile verification failed", "error", err, "channel", msg.ChannelID, "kind", msg.Kind)
				return errorFrame(msg, "authentication failed", "auth_failed"), nil
			}
			p.ChannelID = msg.ChannelID
			ctx = profile.WithProfile(ctx, p)

			// Enforce per-profile token spend limit when configured. Control
			// messages (e.g. a resume handshake) do no LLM work, so they must
			// not be blocked by an exhausted budget — a reconnecting client
			// still needs to see a pending confirmation.
			if cfg.LimitChecker != nil && p.Limit > 0 && p.LimitWindow > 0 && msg.Metadata[pkg.ControlMetadataKey] == "" {
				since := time.Now().Add(-p.LimitWindow)
				used, lerr := cfg.LimitChecker.TotalTokensSince(ctx, p.EntityID, since)
				if lerr != nil {
					slog.Warn("limit check failed", "error", lerr, "entity", p.EntityID)
				} else if used >= p.Limit {
					slog.Info("token limit exceeded", "entity", p.EntityID, "used", used, "limit", p.Limit, "window", p.LimitWindow)
					return errorFrame(msg, "token limit reached, please try again later", "token_limit_exceeded"), nil
				}
			}

			// Scope session to entity so profiles cannot access each other's history.
			entityID = p.EntityID
			groupID = p.Group
			interactionKind = p.Kind
			sessionKey = p.EntityID + ":" + sessionKey
			// Use entity ID as actor for memory scoping and permission checks.
			ctx = actor.WithActor(ctx, p.EntityID)
		} else {
			// No profile system: use the classic channel:sender actor.
			ctx = actor.WithActor(ctx, msg.ChannelID+":"+msg.SenderID)
		}

		// Carry the inbound conversation id so scheduler jobs (and anything
		// else creating deferred work) can deliver results back to this chat.
		ctx = actor.WithConversationID(ctx, msg.ConversationID)

		// Carry the resolved group (account) id so emitted session events can be
		// scoped per-account by an out-of-process consumer. No-op without a
		// profile system (groupID stays empty).
		ctx = actor.WithGroupID(ctx, groupID)

		// Pass explicit confirmation decision from frontend metadata so
		// the orchestrator can bypass LLM-based classification.
		if cd := msg.Metadata["confirmation"]; cd != "" {
			ctx = actor.WithConfirmationDecision(ctx, cd)
		}

		// Route by resume intent: client-supplied conv-id → strict Resume
		// (error frame if the session is gone); channel-minted conv-id →
		// idempotent Create. This replaces the legacy EnsureSession
		// closure that silently auto-created on any cache miss and let
		// the UI drift against a brand-new server-side session.
		if msg.Metadata[pkg.ResumeIntentMetadataKey] == "true" {
			if err := cfg.ResumeSession(sessionKey); err != nil {
				// Only "row genuinely absent" maps to session_expired —
				// the client-side recovery contract assumes the session
				// is gone and clears its stored conversation_id. A DB
				// hiccup must not look the same: surface as a retryable
				// internal_error so a valid conversation_id is preserved.
				if errors.Is(err, state.ErrSessionNotFound) {
					slog.Info("session resume rejected — not found",
						"session", sessionKey, "channel", msg.ChannelID,
						"entity_id", entityID, "group_id", groupID)
					return errorFrame(msg, "This conversation is no longer available. Please start a new chat.", "session_expired"), nil
				}
				slog.Warn("session resume failed — infrastructure error",
					"session", sessionKey, "channel", msg.ChannelID,
					"entity_id", entityID, "group_id", groupID, "error", err)
				return errorFrame(msg, "Something went wrong loading your conversation. Please try again.", "internal_error"), nil
			}
		} else {
			cfg.CreateSession(sessionKey, entityID, groupID, interactionKind)
		}

		// Resume handshake: a reconnecting client sends one control frame right
		// after the socket opens, before the user types. The session was just
		// re-validated above; now, if a tool confirmation is still awaiting the
		// user's decision, re-emit its prompt so the reconnected UI redraws the
		// Approve/Reject buttons instead of showing a dead transcript. No LLM
		// runs. Nothing pending → an empty frame the registry drops.
		if msg.Metadata[pkg.ControlMetadataKey] == pkg.ControlResumeHello {
			if cfg.PendingConfirmation != nil {
				if content, meta, ok := cfg.PendingConfirmation(sessionKey); ok {
					return pkg.OutboundMessage{
						ConversationID: msg.ConversationID,
						ThreadID:       msg.ThreadID,
						Content:        content,
						Metadata:       meta,
					}, nil
				}
			}
			return pkg.OutboundMessage{}, nil
		}

		content := msg.Content
		// Content preparers register by channel KIND ("slack", "console")
		// not by instance. Two Slack bots in one process share the same
		// preparer; only their session/dedup/actor scopes are isolated.
		if prep := pkg.GetContentPreparer(kindOf(msg)); prep != nil {
			content = prep(ctx, content, cfg.RunAction, cfg.HasAction)
		}
		response, inputForDisplay, resultMeta, err := cfg.Runner.Run(ctx, sessionKey, content, msg.Files...)
		if err != nil {
			logger.FromContext(ctx).Error("handler run failed", "error", err)
			errText, errCode := friendlyError(err)
			return errorFrame(msg, errText, errCode), nil
		}
		outContent := response
		if outContent == "" {
			outContent = "(No response)"
		}
		if kindOf(msg) == "console" && inputForDisplay != "" && slog.Default().Enabled(context.Background(), slog.LevelDebug) {
			outContent = "Input to LLM:\n" + inputForDisplay + "\n\n---\n\nResponse:\n" + response
		}
		outMeta := safeMetadata(msg.Metadata)
		for k, v := range resultMeta {
			if outMeta == nil {
				outMeta = make(map[string]string)
			}
			outMeta[k] = v
		}
		return pkg.OutboundMessage{
			ConversationID: msg.ConversationID,
			ThreadID:       msg.ThreadID,
			Content:        outContent,
			Metadata:       outMeta,
		}, nil
	}
}

// kindOf returns the channel TYPE for a message. Prefers msg.Kind (set by
// channel adapters once they declare a kind distinct from instance id) and
// falls back to msg.ChannelID for older channels that haven't been updated
// yet — where there is only ever one instance, ChannelID still holds the
// kind by convention. Callers that need to distinguish instance from kind
// (preparers, console-debug check, WhoAmI channel_type) should route through
// this helper rather than reading msg.Kind directly.
func kindOf(msg pkg.InboundMessage) string {
	if msg.Kind != "" {
		return msg.Kind
	}
	return msg.ChannelID
}

// errorFrame builds a typed error response with the {type:error, error_code:<code>}
// metadata contract the channel-side recovery code (UI handleMessage, console
// printer, etc.) keys off. Every error_code is stable and may be referenced
// by frontends for translated copy — adding one is an API change, not just
// a log message tweak.
func errorFrame(msg pkg.InboundMessage, text, errorCode string) pkg.OutboundMessage {
	return errorResponse(msg, text, map[string]string{
		"type": "error", "error_code": errorCode,
	})
}

func errorResponse(msg pkg.InboundMessage, text string, meta ...map[string]string) pkg.OutboundMessage {
	outMeta := safeMetadata(msg.Metadata)
	if len(meta) > 0 && meta[0] != nil {
		if outMeta == nil {
			outMeta = make(map[string]string)
		}
		for k, v := range meta[0] {
			outMeta[k] = v
		}
	}
	return pkg.OutboundMessage{
		ConversationID: msg.ConversationID,
		ThreadID:       msg.ThreadID,
		Content:        text,
		Metadata:       outMeta,
	}
}

// safeMetadata returns a copy of m with sensitive keys removed.
func safeMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	delete(out, "profile_token")
	// Internal flags used to communicate inbound-enrichment failure between
	// yaml_ws.go and the handler. Never echo them back to channel adapters.
	delete(out, enrichmentFailedKey)
	delete(out, enrichmentFailedStepKey)
	return out
}

// friendlyError returns a user-facing message and machine-readable error code
// for known error conditions. The error_code is stable and can be used by
// frontends for i18n translations.
func friendlyError(err error) (message string, errorCode string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "maximum context length") || strings.Contains(msg, "context_length_exceeded"):
		return "Sorry, this conversation has grown too long for the model to process. Please start a new conversation or clear the session.", "context_length_exceeded"
	case strings.Contains(msg, "rate_limit") || strings.Contains(msg, "429"):
		return "I'm being rate-limited right now. Please try again in a moment.", "rate_limited"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "The request timed out. Please try again.", "timeout"
	default:
		return "Something went wrong processing your message. Please try again or start a new conversation.", "internal_error"
	}
}
