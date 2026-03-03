package channel

import "strings"

// ChunkMessage splits a message into chunks that fit within maxLen.
// It tries to split at newline boundaries. If maxLen is <= 0, the
// message is returned as a single chunk.
func ChunkMessage(msg string, maxLen int) []string {
	if maxLen <= 0 || len(msg) <= maxLen {
		return []string{msg}
	}

	var chunks []string
	for len(msg) > 0 {
		if len(msg) <= maxLen {
			chunks = append(chunks, msg)
			break
		}

		// Try to split at the last newline within maxLen.
		cut := maxLen
		if idx := strings.LastIndex(msg[:maxLen], "\n"); idx > 0 {
			cut = idx + 1 // include the newline in this chunk
		}
		chunks = append(chunks, msg[:cut])
		msg = msg[cut:]
	}
	return chunks
}
