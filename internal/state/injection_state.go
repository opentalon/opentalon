package state

// InjectionState is the per-session tool-promotion bookkeeping
// persisted by the orchestrator's preparer phase. KnownTools carries the
// sticky tool-tier state (load_tools promotion + tool-error demotion).
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

// KnownToolEntry is the tool-tier bookkeeping shape: load_tools
// promotes a tool to tier1 (sticky across turns) and the tool-error
// tracker can demote it.
type KnownToolEntry struct {
	ToolName string        `json:"tool_name"`
	Tier     KnownToolTier `json:"tier"`
	LRURank  int           `json:"lru_rank"`
	Demoted  bool          `json:"demoted"`
}

// KnownToolTier is the Phase-4 visibility bucket for a tool. Typed
// string keeps the wire format identical to the previous raw-string
// shape (encoding/json marshals named-string types as their underlying
// value) while making the four valid bucket names visible at compile
// time. A typo like `"teir1"` was previously a silent demotion to
// Tier 3; with the constants it is a build error.
type KnownToolTier string

const (
	KnownToolTier0 KnownToolTier = "tier0"
	KnownToolTier1 KnownToolTier = "tier1"
	KnownToolTier2 KnownToolTier = "tier2"
	KnownToolTier3 KnownToolTier = "tier3"
)
