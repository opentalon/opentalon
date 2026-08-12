package orchestrator

// Turning a plugin's declared parameters into the JSON Schema the model is
// shown. Kept out of orchestrator.go so the schema rules sit together and can
// be read without the agent loop around them.

// undeclaredParameterType is what a parameter is announced as when its plugin
// declares no type at all. Every tool-call argument reaches a plugin as a
// string anyway, so "string" is the honest description of an undeclared
// parameter — this is a named fallback, not a leftover of the days when every
// parameter was announced this way.
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
