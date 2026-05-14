package orchestrator

import "testing"

func TestIsWorkflowCandidate_Disabled(t *testing.T) {
	if isWorkflowCandidate("delete all items", false) {
		t.Error("should return false when workflow is disabled")
	}
}

func TestIsWorkflowCandidate_Keywords(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{"delete all items with Test in name", true},
		{"assign every item to John", true},
		{"bulk update status", true},
		{"mass delete obsolete tools", true},
		{"lösche alle Test-Objekte", true}, // German "alle"
		{"show me item 123", false},
		{"what is a Person?", false},
		{"create one item", false},
	}
	for _, tt := range tests {
		if got := isWorkflowCandidate(tt.msg, true); got != tt.want {
			t.Errorf("isWorkflowCandidate(%q) = %v, want %v", tt.msg, got, tt.want)
		}
	}
}
