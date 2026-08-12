package orchestrator

import (
	"encoding/json"
	"strings"
)

// Turning a plugin's declared parameters into the JSON Schema the model is
// shown. Kept out of orchestrator.go so the schema rules sit together and can
// be read without the agent loop around them.

// parameterSchema is the JSON Schema fragment the model is shown for one
// parameter. Exactly one of two branches produces it, and the precedence
// between them is fixed here:
//
//  1. the plugin supplied a usable fragment — every keyword in it survives,
//     enum values, array item types, nested object shapes and all. A supplied
//     fragment always wins; nothing is merged into it and nothing overrides
//     it, so what a plugin declares is what the model reads;
//  2. the plugin supplied none, or supplied one the host cannot safely
//     announce — the host synthesises the only two things it knows about the
//     parameter, its type and its description.
//
// Branch 2 is the fallback for plugins that predate the schema field, choose
// not to fill it, or fill it with something unusable. It is not a parallel
// path that can win over branch 1. The bool reports which branch ran, so the
// caller can say out loud that a supplied fragment was thrown away.
func parameterSchema(p Parameter) (map[string]interface{}, bool) {
	if frag, ok := usableSchemaFragment(p.Schema); ok {
		return frag, true
	}
	return map[string]interface{}{
		"type":        jsonSchemaType(p.Type),
		"description": p.Description,
	}, false
}

// usableSchemaFragment decodes a plugin-supplied fragment into the map the
// tools array is assembled from, and reports whether it can be announced at
// all.
//
// Decoding and re-encoding normalises the key order (Go marshals map keys
// alphabetically) and nothing else: every keyword and every value is kept.
// Numbers are decoded as json.Number so a constraint such as a large
// "maximum" survives unchanged rather than going through float64.
//
// Three things make a fragment unusable, and all three cost the same thing if
// they get through — the provider rejects the entire tools array, not the one
// property that carries them:
//
//   - it is not a single JSON object. A bare `true`, a string, an array,
//     trailing content, malformed bytes: none of those can sit under
//     `properties.<name>`;
//   - its "type" names something JSON Schema does not define. This is the
//     same guard jsonSchemaType applies on the synthesised branch, applied
//     here too so a fragment cannot walk past it;
//   - it contains a "$ref" at any depth. A per-parameter fragment travels
//     without the "$defs" its reference points at, so the reference can never
//     resolve on the far side.
//
// An unusable fragment is not dropped, it falls back to the synthesised form
// — which is exactly the behaviour a plugin got before it could supply a
// fragment at all, so this never leaves the model worse informed than it was.
func usableSchemaFragment(raw string) (map[string]interface{}, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var frag map[string]interface{}
	if err := dec.Decode(&frag); err != nil || frag == nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	if !declaresKnownTypes(frag["type"]) {
		return nil, false
	}
	if containsRef(frag) {
		return nil, false
	}
	return frag, true
}

// declaresKnownTypes reports whether a fragment's "type" keyword is one the
// host can announce. JSON Schema allows a single name or a list of them, and
// allows the keyword to be absent entirely — a fragment that only constrains
// by enum is perfectly valid. Anything else present under the key, including
// a non-string list member, is a type name no provider will accept.
func declaresKnownTypes(declared interface{}) bool {
	switch t := declared.(type) {
	case nil:
		return true
	case string:
		return isJSONSchemaType(t)
	case []interface{}:
		for _, member := range t {
			name, isString := member.(string)
			if !isString || !isJSONSchemaType(name) {
				return false
			}
		}
		return len(t) > 0
	default:
		return false
	}
}

// containsRef reports whether a decoded fragment references a definition
// anywhere inside it. Nesting matters: a "$ref" three levels down inside an
// items or properties block dangles just as badly as one at the top.
func containsRef(value interface{}) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		if _, present := v["$ref"]; present {
			return true
		}
		for _, nested := range v {
			if containsRef(nested) {
				return true
			}
		}
	case []interface{}:
		for _, nested := range v {
			if containsRef(nested) {
				return true
			}
		}
	}
	return false
}

// undeclaredParameterType is what a parameter is announced as when its plugin
// supplies neither a schema fragment nor a type. Every tool-call argument
// reaches a plugin as a string anyway, so "string" is the honest description
// of an undeclared parameter — this is a named fallback, not a leftover of the
// days when every parameter was announced this way.
const undeclaredParameterType = "string"

// jsonSchemaType maps a plugin-declared parameter type onto a type name JSON
// Schema actually defines. Parameter.Type is a free-form string on the wire,
// and plugins do use their own shorthand for shapes the wire cannot carry
// (e.g. "json" for "an object or an array, encoded as text"). Announcing such
// a name verbatim would make a provider reject the whole tools array, so
// anything outside the seven JSON Schema types degrades to
// undeclaredParameterType.
func jsonSchemaType(declared string) string {
	if isJSONSchemaType(declared) {
		return declared
	}
	return undeclaredParameterType
}

// isJSONSchemaType reports whether name is one of the seven types JSON Schema
// defines. Everything the host emits has to pass this: a provider answers an
// unknown type name by rejecting the entire tools array, not the one property
// that carries it.
func isJSONSchemaType(name string) bool {
	switch name {
	case "string", "number", "integer", "boolean", "array", "object", "null":
		return true
	default:
		return false
	}
}
