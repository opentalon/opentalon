package channel

import (
	"encoding/json"
	"os"
	"regexp"
)

var contextRe = regexp.MustCompile(`\{\{(\w+)\.([^}]+)\}\}`)

// substituteTemplate replaces all {{namespace.key}} placeholders in s.
// The "env" namespace reads from os.Getenv. All other namespaces do map
// lookup in contexts. Missing keys resolve to empty string.
func substituteTemplate(s string, contexts map[string]map[string]string) string {
	return substituteWith(s, contexts, false)
}

// substituteTemplateJSON is like substituteTemplate but JSON-escapes all
// substituted values (for safe embedding inside JSON string literals).
func substituteTemplateJSON(s string, contexts map[string]map[string]string) string {
	return substituteWith(s, contexts, true)
}

func substituteWith(s string, contexts map[string]map[string]string, jsonEscape bool) string {
	return contextRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := contextRe.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		ns, key := parts[1], parts[2]
		var val string
		if ns == "env" {
			val = os.Getenv(key)
		} else if ctx, ok := contexts[ns]; ok {
			val = ctx[key]
		}
		if jsonEscape {
			return jsonEscapeString(val)
		}
		return val
	})
}

// jsonEscapeString escapes a string for embedding inside a JSON string literal
// (without the surrounding quotes).
func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	// json.Marshal wraps in quotes: "value" → strip them
	return string(b[1 : len(b)-1])
}
