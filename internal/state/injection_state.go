package state

// InjectionState is the per-session knowledge / tool dedup bookkeeping
// persisted by the orchestrator's preparer phase (RFC #249). Phase 3
// populates KnownKnowledge; KnownTools is reserved for Phase 4 and
// stays empty on the Phase-3 write path.
//
// Lives in the `state` package alongside Session so the orchestrator
// can wire its optional InjectionStateStore dependency without
// importing the DB-backed store package — same separation Session
// already follows.
type InjectionState struct {
	KnownKnowledge []KnownKnowledgeEntry `json:"known_knowledge,omitempty"`
	KnownTools     []KnownToolEntry      `json:"known_tools,omitempty"`
}

// KnownKnowledgeEntry is one knowledge-article chunk the orchestrator
// has already seen this session. ContentSHA256 is the dedup key —
// different chunks of the same article have different SHAs, so chunk-
// level disjoint information correctly triggers re-injection.
// ArticleID is auxiliary: O(1) lookup for truncation/summarization
// release-paths and human-meaningful event-log strings.
type KnownKnowledgeEntry struct {
	ArticleID         string `json:"article_id"`
	ContentSHA256     string `json:"content_sha256"`
	FirstInjectedTurn int    `json:"first_injected_turn,omitempty"`
}

// KnownToolEntry is the Phase-4 tool-tier bookkeeping shape. Phase 3
// readers unmarshal existing entries to preserve forward-compatible
// rows, but the Phase-3 writer never produces a non-empty slice.
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
