package pipeline

// PluginCommand is the only command type for Phase 1.
type PluginCommand struct {
	Plugin string
	Action string
	Args   map[string]string
}
