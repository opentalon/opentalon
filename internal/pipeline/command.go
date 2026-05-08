package pipeline

// PluginCommand is the only command type for Phase 1.
//
// Args carries typed values (string / number / bool / slice / map). The
// pipeline-internal model is typed because the planner emits JSON whose
// numbers become float64 / bool / nested structures, and step-output
// substitution resolves typed values from prior step results — both lose
// information when collapsed to map[string]string mid-pipeline (notably
// large integer IDs rendered with default fmt.Sprintf("%v", float64(N))
// switch to scientific notation, e.g. "2.037838e+06"). The conversion to
// the wire-level map[string]string happens once, at the orchestrator's
// pipeline-runner boundary, where typed-aware stringification preserves
// integer form and JSON-marshals non-scalars.
type PluginCommand struct {
	Plugin string
	Action string
	Args   map[string]any
}
