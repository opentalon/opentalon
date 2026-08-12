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
//   - its own "type" names something JSON Schema does not define. This is the
//     same seven-name guard jsonSchemaType applies on the synthesised branch,
//     applied here too so the fragment path cannot walk past the check the
//     type path has. It reaches the fragment's own "type" only, not the
//     "type" of a subschema nested inside it — see declaresKnownTypes;
//   - it references a definition at any depth. A per-parameter fragment
//     travels without the "$defs" the reference points at, so it can never
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
	if !declaresKnownTypes(frag) {
		return nil, false
	}
	if containsRef(frag) {
		return nil, false
	}
	return frag, true
}

// declaresKnownTypes reports whether a fragment's own "type" keyword is one
// the host can announce.
//
// Absent is fine — a fragment that constrains only by enum or const is a valid
// schema. Present is held to the seven names, as a single string or as a list
// of them. An explicitly null "type" is neither: JSON Schema has no such form,
// so it is rejected rather than read as "absent", which is the reading a plain
// nil check would give it.
//
// This deliberately looks at the fragment's own keyword and does not recurse.
// A nested map inside a schema is not necessarily itself a schema — the value
// under "default", "const" or an "enum" member is data, and a "type" key
// inside it means nothing. Walking every nested "type" would reject valid
// fragments over values that were never type declarations, which is a worse
// trade than letting a wrong nested name through: the surrounding keyword
// already tells the provider how to read it.
func declaresKnownTypes(frag map[string]interface{}) bool {
	declared, present := frag["type"]
	if !present {
		return true
	}
	switch t := declared.(type) {
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
// anywhere inside it. Nesting matters: a reference three levels down inside an
// items or properties block dangles just as badly as one at the top.
//
// All three of JSON Schema's reference keywords count, because all three
// resolve against something a per-parameter fragment cannot bring with it.
// This is a key-name scan, so it also fires on the rare fragment that uses one
// of those names as a property name or inside a literal value — that costs
// such a fragment its detail and nothing else, which is the right way round
// for a check whose failure mode is the provider rejecting the whole tools
// array.
func containsRef(value interface{}) bool {
	switch v := value.(type) {
	case map[string]interface{}:
		for _, keyword := range [...]string{"$ref", "$dynamicRef", "$recursiveRef"} {
			if _, present := v[keyword]; present {
				return true
			}
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
