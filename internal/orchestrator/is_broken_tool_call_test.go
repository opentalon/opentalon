package orchestrator

import "testing"

// TestIsBrokenToolCall pins the malformed-vs-valid classification, including the
// compact canonical "[tool_call] plugin__action(args)" branch added when the FQN
// separator moved from "." to "__": a valid double-underscore call must NOT be
// treated as broken (and thus must not be stripped from history).
func TestIsBrokenToolCall(t *testing.T) {
	tests := []struct {
		name    string
		content string
		broken  bool
	}{
		{"compact canonical __", "[tool_call] inventory__list-items(category_id: 5)", false},
		{"compact legacy dot", "[tool_call] reminder.say", false},
		{"json body", "[tool_call]\n{\"tool\":\"timly__list-items\",\"args\":{}}\n[/tool_call]", false},
		{"no tool_call marker", "Just a normal assistant answer.", false},

		{"dot only", "[tool_call] .", true},
		{"empty after marker", "[tool_call]", true},
		{"separator only, too short", "[tool_call] __", true},
		{"single word, no separator", "[tool_call] listitems", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBrokenToolCall(tt.content); got != tt.broken {
				t.Errorf("isBrokenToolCall(%q) = %v, want %v", tt.content, got, tt.broken)
			}
		})
	}
}
