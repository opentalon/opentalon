// Package contextargs declares the wire names for orchestrator-managed
// context arguments that plugins may opt into receiving via
// ActionMsg.InjectContextArgs. Both the host (opentalon-core's
// ContextArgProvider registry) and external plugins (their Capability
// manifests AND their Execute-time Args parse) reference the same
// constants here, so a typo in any one site fails to compile rather
// than silently drifting and leaving the consumer with empty args.
//
// Adding a new context arg name: declare it as a const here AND register
// a matching provider in the host's defaultContextArgProviders. Plugins
// then opt in by listing the name in InjectContextArgs on each action
// that needs it.
//
// A DECLARED inject name is host-owned on every call to that action: the
// host's value replaces a caller-supplied one, and when the host resolves
// nothing the key is removed. An action that does not declare a name gets
// whatever the caller put under it, as an ordinary untrusted argument. A
// plugin therefore must not declare one of these names as a tool Parameter
// — the model's value could never reach it — and a scheduled job cannot
// supply one through its stored args: the host resolves them from the run's
// context or not at all.
package contextargs

// SessionID is the opaque session identifier carried in the request
// context. Resolves to the empty string when called outside a session.
// It is the PACKED session key (channelID:conversationID[:threadID]),
// not the bare conversation id — see ConversationID when a consumer needs
// the client-round-trippable id.
const SessionID = "session_id"

// ConversationID is the bare, client-round-trippable conversation id (the
// value a channel echoes back as ?conversation_id=), distinct from the
// packed SessionID key. A consumer that must address the exact same
// conversation the browser holds — e.g. delivering an out-of-band message
// back into a live chat session — needs this raw id, because the packed
// SessionID would be re-prefixed with the entity and miss on resume.
// Resolves to the empty string when called outside a conversation.
const ConversationID = "conversation_id"

// AllowedPlugins is the sorted JSON array of plugin names the current
// profile permits. Plugins may use it as a coarse pre-filter (e.g.
// MCPActions WHERE pluginName ContainsAny […]) before applying
// AllowedTools. Empty string "" means "no profile is loaded; do not
// apply this filter".
const AllowedPlugins = "allowed_plugins"

// AllowedTools is the sorted JSON array of fully-qualified tool names
// ("<plugin>__<action>") the current session can invoke right now —
// profile-level plugin allowance + preparer-action exclusion +
// UserOnly exclusion. Always emitted; "[]" is a legitimate value
// meaning "the session can call zero tools" (fail-closed). Plugins
// consuming this value MUST fail-closed when the arg is absent: an
// older or misconfigured host that omitted it would otherwise silently
// drop the chokepoint that the per-session palette enforces.
const AllowedTools = "allowed_tools"

// GroupID is the authenticated tenant/account scope of the actor, as
// resolved by the profile verifier (Profile.Group) and carried on the
// request context. Resolves to the empty string when the actor has no
// group (e.g. a profile-less local dev setup). A plugin that scopes
// per-tenant state MUST fail-closed on the empty value — an unscoped
// call must never fall back to another tenant's data.
const GroupID = "group_id"

// EntityID is the actor identity — the profile's EntityID when a profile
// system is active, else the classic channel:sender id. Resolves to the
// empty string outside any actor context. Used to record who authored a
// resource; distinct from GroupID, which scopes access.
const EntityID = "entity_id"

// InteractionKind is the kind of run the current turn belongs to, as
// resolved by the profile verifier (Profile.Kind): "chat" for a
// human-driven turn, "system" for a backend-originated one. A verified
// profile always carries one of the two. Absent (the host injects nothing)
// when no profile is on the run: a dispatcher-run action, or a profile-less
// local dev setup. A nested RunAction callback inherits the labels of the
// verified turn it runs inside. A consumer that gates on the kind MUST treat
// an absent value as "unlabelled" and fall back to whatever it did before
// labels existed.
const InteractionKind = "interaction_kind"

// SystemSource is the per-feature label a system run carries
// (Profile.SystemSource) — the name of the backend feature that opened
// the run, as the WhoAmI server reported it. Absent for a chat turn and
// whenever no profile is on the run. A downstream service may use it to
// give one named run a narrower capability set than an interactive turn;
// the host only reports the label and enforces nothing itself.
const SystemSource = "system_source"

// LastUserMessageID is an opaque id for the latest message the user wrote
// in this chat session. The host replaces it only when a turn starts with a
// message the user wrote while nothing was waiting for their approval. It
// stays the same for an answer to an open confirmation prompt (an
// Approve/Reject click, or a typed reply the host reads as approve, reject
// or a correction), for a hidden turn the host injects, for a run a backend
// feature opened (interaction_kind "system", visible or not), and for a
// click on a confirmation that is no longer active. The host keeps it with
// the session, so it survives restarts and every host instance sees the
// same value: a tool call the user approves on a later turn still carries
// the id of the message that led to the proposal.
//
// A plugin can use it to require that the second step of a two-step
// destructive action (look first, then run) is confirmed only after the user
// has written again: remember the id with the first step and refuse the
// second while the id is unchanged. Resolves to the empty string outside a
// chat turn (a scheduled job, a webhook), in a nested callback that moved
// to another session, and for a turn in which the host could not save or
// read back the session's id; a consumer that gates on it MUST treat an
// absent value as "unknown" and fail closed.
const LastUserMessageID = "last_user_message_id"

// Callback identity carriers. A plugin that fires a host RunAction callback
// for a background/system action (e.g. the agents plugin running a
// scheduled workflow) has no profile on the wire — the CallbackRequest
// carries only args. These reserved arg keys let such a plugin declare the
// actor identity the whole nested action chain should run as. The host
// (handleCallback) pops them off req.Args, enriches the callback ctx with
// actor/group/session, and STRIPS them so the target action never sees
// them as tool arguments. Distinct from the inject names above so they can
// never collide with a value the host injects on the way back down. Both
// the host and external plugins reference these constants, so a typo fails
// to compile rather than silently dropping the identity.
const (
	CallbackEntityID  = "__ot_cb_entity_id"
	CallbackGroupID   = "__ot_cb_group_id"
	CallbackSessionID = "__ot_cb_session_id"
)
