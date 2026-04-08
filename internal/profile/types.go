// Package profile implements profile-based multi-tenancy for OpenTalon.
// A profile is a verified identity returned by an external "Who Am I" HTTP server.
// Each inbound message carries a bearer token in Metadata["profile_token"]; the token
// is verified once per TTL window and the resulting Profile is stored in the request context.
package profile

// Profile is the verified identity of an inbound message's sender.
type Profile struct {
	EntityID  string   // stable identifier from the WhoAmI server (used for session/memory scoping)
	Group     string   // group name from the WhoAmI server (used for tool-access control)
	Token     string   // original bearer token (may be forwarded to MCP servers)
	Plugins   []string // plugin IDs returned by WhoAmI (auto-saved to group_plugins table)
	ChannelID string   // set by the handler; used for usage statistics
}
