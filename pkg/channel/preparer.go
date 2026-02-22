package channel

import "sync"

var (
	contentPreparersMu sync.RWMutex
	contentPreparers   = make(map[string]ContentPreparer)
)

// RegisterContentPreparer registers a content preparer for a channel ID.
// Called from channel packages in init().
func RegisterContentPreparer(channelID string, p ContentPreparer) {
	contentPreparersMu.Lock()
	defer contentPreparersMu.Unlock()
	contentPreparers[channelID] = p
}

// GetContentPreparer returns the content preparer for the given channel ID, or nil.
// Used by the core when building the message handler.
func GetContentPreparer(channelID string) ContentPreparer {
	contentPreparersMu.RLock()
	defer contentPreparersMu.RUnlock()
	return contentPreparers[channelID]
}
