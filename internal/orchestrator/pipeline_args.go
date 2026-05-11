package orchestrator

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// pipelineArgsToWire converts typed pipeline-step args to the wire-level
// map[string]string used by ToolCall and the gRPC plugin protocol.
//
// The pipeline package keeps args typed (map[string]any) so the planner's
// JSON numbers and step-output substitution survive round-trip. The wire
// format is map<string,string> by definition (proto/plugin.proto), so this
// is the boundary where types collapse into strings. Each plugin re-coerces
// per its declared input schema on the receiving side; correctness here is
// "no information loss in the round-trip".
//
// Specifically: integer-valued floats render as plain decimals (not
// scientific notation), booleans render as "true"/"false", strings pass
// through unchanged, and compound values (slice/map) round-trip via JSON.
//
// nil values are emitted as "null" — sending a key with an empty string
// would be ambiguous with a deliberately-blank string param.
func pipelineArgsToWire(args map[string]any) map[string]string {
	out := make(map[string]string, len(args))
	for k, v := range args {
		// Drop nil values — the planner sometimes emits null for optional
		// params. Sending "null" as a string causes schema validation failures.
		if k == "" || v == nil {
			continue
		}
		out[k] = argToWireString(v)
	}
	return out
}

// argToWireString formats a single typed value for the wire layer.
//
// The plain-decimal float rendering matters for IDs: a Timly item id like
// 2_037_838 becomes float64 after JSON unmarshal, and fmt.Sprintf("%v", v)
// (the previous behaviour) renders it as "2.037838e+06", which the
// downstream MCP server then rejects as "type string did not match the
// following type: integer". strconv.FormatFloat with 'f', -1 keeps the
// integer-valued float in plain decimal form regardless of magnitude.
func argToWireString(v any) string {
	if v == nil {
		return "null"
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case json.Number:
		return string(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float32:
		return formatFloat(float64(x))
	case float64:
		return formatFloat(x)
	default:
		// Slice / map / anything richer — JSON-marshal so the downstream
		// plugin re-parses with structure intact (its coerce() reads the
		// schema type and json.Unmarshals as needed).
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// formatFloat renders a float64 in the way users expect when staring at IDs
// in a log line — integers stay integer, fractions stay decimal, never
// scientific notation. We mirror Go's strconv 'f' verb with -1 precision
// (shortest round-trip representation) but special-case integer values to
// avoid the "1.7e+02" surprise on whole numbers above the %v threshold.
func formatFloat(x float64) string {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		// JSON marshal handles these as errors; fall through to %v which
		// renders "NaN" / "+Inf" rather than crashing the call.
		return fmt.Sprintf("%v", x)
	}
	if x == math.Trunc(x) && !math.IsInf(x, 0) && math.Abs(x) < 1e18 {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'f', -1, 64)
}
