// Package profile implements profile-based multi-tenancy for OpenTalon.
// A profile is a verified identity returned by an external "Who Am I" HTTP server.
// Each inbound message carries a bearer token in Metadata["profile_token"]; the token
// is verified once per TTL window and the resulting Profile is stored in the request context.
package profile

import "time"

// CredentialHeader is a per-MCP-server credential returned by the WhoAmI server.
// Header is the HTTP header name to set (e.g. "X-Timly-User") and Value is the
// header value. When making requests to the named MCP server, these are merged
// with the server's static config headers; WhoAmI-returned headers take priority.
type CredentialHeader struct {
	Header string `json:"header"` // HTTP header name (e.g. "X-App-User", "Authorization")
	Value  string `json:"value"`  // header value (e.g. "user-123", "Bearer tok")
}

// Profile is the verified identity of an inbound message's sender.
type Profile struct {
	EntityID    string                      // stable identifier from the WhoAmI server (used for session/memory scoping)
	Group       string                      // group name from the WhoAmI server (used for tool-access control)
	Token       string                      // original bearer token (may be forwarded to MCP servers)
	Plugins     []string                    // plugin IDs returned by WhoAmI (auto-saved to group_plugins table)
	ChannelID   string                      // set by the handler; used for usage statistics
	Model       string                      // optional model override returned by WhoAmI (e.g. "anthropic/claude-3-5-sonnet-20241022")
	ChannelType string                      // optional channel type returned by WhoAmI (e.g. "slack", "web", "api")
	Limit       int                         // token spend limit per LimitWindow (0 = unlimited)
	LimitWindow time.Duration               // rolling window for Limit (0 = unlimited)
	Credentials map[string]CredentialHeader // per-MCP-server credentials from WhoAmI, keyed by server name
}
