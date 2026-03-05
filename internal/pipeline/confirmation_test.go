package pipeline

import "testing"

func TestParseConfirmationApproved(t *testing.T) {
	approved := []string{"yes", "y", "Yes", "YES", "Y", " yes ", " Y "}
	for _, input := range approved {
		if got := ParseConfirmation(input); got != Approved {
			t.Errorf("ParseConfirmation(%q) = %d, want Approved", input, got)
		}
	}
}

func TestParseConfirmationRejected(t *testing.T) {
	// Anything that's not y/yes is rejected
	rejected := []string{
		"no", "n", "cancel", "stop",
		"what is the weather",
		"actually can you also check logs",
		"maybe", "", "yess", "ye", "nope",
	}
	for _, input := range rejected {
		if got := ParseConfirmation(input); got != Rejected {
			t.Errorf("ParseConfirmation(%q) = %d, want Rejected", input, got)
		}
	}
}
