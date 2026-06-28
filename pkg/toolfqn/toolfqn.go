// Package toolfqn defines the canonical encoding of a tool's fully-qualified
// name (FQN) — the "<plugin><Separator><action>" string used as the function
// name when a tool is offered to an LLM and as the key under which tools are
// tracked across the orchestrator and its retrieval plugins.
//
// The separator is a DOUBLE UNDERSCORE. LLM providers (OpenAI, Anthropic) require
// a function name to match ^[a-zA-Z0-9_-]{1,64}$ (see the provider docs), so the
// separator MUST come from that character set. A dot ('.') — used historically —
// is rejected by every compliant endpoint, which is why a single source of truth
// is shared between opentalon core and the retrieval plugins: both sides must
// compose the FQN with the exact same separator or the allowed_tools / relevant_tools
// wire contract silently mismatches.
//
// Decoding (Split) still tolerates the legacy dot form so names emitted before
// this change — persisted scheduler jobs, session history rows, and dot-trained
// LLM output — keep resolving. Encoding (Join) always uses Separator.
package toolfqn

import (
	"fmt"
	"strings"
)

// Separator joins a plugin name and an action name into a tool FQN. It is a
// double underscore because that is the only namespacing character, besides the
// single hyphen/underscore already used inside plugin and action names, that
// satisfies the provider name charset ^[a-zA-Z0-9_-]{1,64}$.
const Separator = "__"

// legacySeparator is the dot form composed before the move to Separator. It is
// still accepted by Split for backward compatibility but is never produced.
const legacySeparator = "."

// Join builds the canonical FQN "<plugin><Separator><action>". It does not
// validate its inputs — Split is the validating decoder; callers must pass a
// non-empty plugin and action, since Join("","") would yield "__", which Split
// rejects (Join is therefore not a total inverse of Split for malformed input).
func Join(plugin, action string) string {
	return plugin + Separator + action
}

// Split decodes a tool FQN into its plugin and action parts. Both parts must be
// valid identifiers, which rejects natural-language fragments an LLM might emit.
//
// An action name may itself contain "__" (e.g. the MCP-bridged "timly__create-item",
// whose canonical FQN is "timly__timly__create-item"), so both branches resolve the
// boundary deterministically:
//   - Legacy dot form is tried FIRST, splitting on the LAST '.', so a dotted
//     "<plugin>.<action>" is disambiguated by its dot before any "__" is considered.
//   - Canonical names carry no dot and fall through to the "__" branch, which splits
//     on the FIRST "__". No plugin name contains "__", so the first occurrence is
//     always the plugin/action boundary even when the action itself contains "__".
func Split(s string) (plugin, action string, err error) {
	if dot := strings.LastIndex(s, legacySeparator); dot > 0 && dot < len(s)-1 {
		plugin, action = s[:dot], s[dot+1:]
		if isValidPluginName(plugin) && isValidActionName(action) {
			return plugin, action, nil
		}
	}

	if i := strings.Index(s, Separator); i > 0 && i < len(s)-len(Separator) {
		plugin, action = s[:i], s[i+len(Separator):]
		if isValidPluginName(plugin) && isValidActionName(action) {
			return plugin, action, nil
		}
	}

	return "", "", fmt.Errorf("invalid tool name %q", s)
}

// isValidPluginName reports whether s is a syntactically valid plugin name.
// The dot is allowed only so the legacy dotted form still parses via Split; it
// is never produced by Join.
func isValidPluginName(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// isValidActionName reports whether s is a syntactically valid action name.
func isValidActionName(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}
