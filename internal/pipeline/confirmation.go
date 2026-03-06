package pipeline

import "strings"

// ConfirmationDecision represents the user's response to a pipeline plan.
type ConfirmationDecision int

const (
	Approved ConfirmationDecision = iota
	Rejected
)

var approvedWords = map[string]bool{
	"yes": true, "y": true,
}

// ParseConfirmation determines whether user input is an explicit approval.
// Anything other than y/yes (case-insensitive) is treated as rejection.
func ParseConfirmation(input string) ConfirmationDecision {
	normalized := strings.TrimSpace(strings.ToLower(input))
	if approvedWords[normalized] {
		return Approved
	}
	return Rejected
}
