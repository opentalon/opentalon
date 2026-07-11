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
