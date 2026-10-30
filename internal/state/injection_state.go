package state

// InjectionState is the per-session tool-promotion bookkeeping
// persisted by the orchestrator's preparer phase. KnownTools carries the
// sticky-promotion state (load_tools promotion + tool-error demotion).
//
// A legacy `known_knowledge` JSON key is ignored on read: knowledge is
// pull-only now, so the orchestrator no longer tracks per-session known
// articles. Existing rows that still carry the key unmarshal cleanly —
// the unknown key is dropped.
//
// Lives in the `state` package alongside Session so the orchestrator
// can wire its optional InjectionStateStore dependency without
// importing the DB-backed store package — same separation Session
// already follows.
type InjectionState struct {
	KnownTools []KnownToolEntry `json:"known_tools,omitempty"`
}

// KnownToolEntry is the sticky tool-promotion bookkeeping shape:
// loading a tool records it here so it stays in the LLM's native tools
// array across turns, and the tool-error tracker can flip Demoted to
// leave it out of that sticky selection until an explicit load_tools
// re-promotes it or the tracker clears the flag. LRURank carries the
// turn of the tool's most recent promotion; the orchestrator's sticky
// selection skips Demoted entries and keeps the highest LRURank ones up
// to its cap (no tiers).
//
// A legacy `tier` JSON key is ignored on read — older rows that still
// carry "tier":"tier1" unmarshal cleanly, the unknown key is dropped,
// same as the removed known_knowledge key the InjectionState doc above
// describes.
type KnownToolEntry struct {
	ToolName string `json:"tool_name"`
	LRURank  int    `json:"lru_rank"`
	Demoted  bool   `json:"demoted"`
}
