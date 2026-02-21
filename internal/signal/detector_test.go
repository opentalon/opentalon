package signal

import (
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/router"
)

func TestDetectExplicit(t *testing.T) {
	d := NewDetector()

	tests := []struct {
		action string
		want   router.Signal
	}{
		{"regenerate", router.SignalRegenerated},
		{"thumbs_down", router.SignalRejected},
		{"thumbs_up", router.SignalAccepted},
		{"unknown", router.SignalAccepted},
	}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			if got := d.DetectExplicit(tt.action); got != tt.want {
				t.Errorf("DetectExplicit(%q) = %d, want %d", tt.action, got, tt.want)
			}
		})
	}
}

func TestDetectImplicitRejection(t *testing.T) {
	d := NewDetector()
	prev := &provider.CompletionResponse{Content: "Here is the answer."}

	tests := []struct {
		name    string
		message string
		want    router.Signal
	}{
		{"try again", "No, try again please", router.SignalRejected},
		{"that's wrong", "That's wrong, the answer should be 42", router.SignalRejected},
		{"incorrect", "That's incorrect", router.SignalRejected},
		{"not helpful", "This is not helpful at all", router.SignalRejected},
		{"doesnt work", "This doesn't work", router.SignalRejected},
		{"please fix", "Can you fix this?", router.SignalRejected},
		{"no prefix", "No. That is not what I asked", router.SignalRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &provider.Message{Role: provider.RoleUser, Content: tt.message}
			if got := d.DetectImplicit(prev, msg); got != tt.want {
				t.Errorf("DetectImplicit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDetectImplicitAcceptance(t *testing.T) {
	d := NewDetector()
	prev := &provider.CompletionResponse{Content: "Here is the answer about quantum computing."}

	tests := []struct {
		name    string
		message string
	}{
		{"follow up question", "Great, now can you explain entanglement?"},
		{"thanks", "Thanks, that helps a lot"},
		{"new topic", "What about the weather today?"},
		{"elaboration", "Can you tell me more about the second point?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &provider.Message{Role: provider.RoleUser, Content: tt.message}
			if got := d.DetectImplicit(prev, msg); got != router.SignalAccepted {
				t.Errorf("DetectImplicit() = %d, want Accepted", got)
			}
		})
	}
}

func TestDetectImplicitNilInputs(t *testing.T) {
	d := NewDetector()

	if got := d.DetectImplicit(nil, nil); got != router.SignalAccepted {
		t.Errorf("nil inputs should return Accepted, got %d", got)
	}

	prev := &provider.CompletionResponse{Content: "test"}
	if got := d.DetectImplicit(prev, nil); got != router.SignalAccepted {
		t.Errorf("nil followUp should return Accepted, got %d", got)
	}
}

func TestWordOverlap(t *testing.T) {
	a := significantWords("hello world testing overlap")
	b := significantWords("testing overlap different words")

	overlap := wordOverlap(a, b)
	if overlap < 0.3 {
		t.Errorf("expected overlap > 0.3, got %f", overlap)
	}
}

func TestWordOverlapEmpty(t *testing.T) {
	a := significantWords("")
	b := significantWords("hello world")

	if got := wordOverlap(a, b); got != 0 {
		t.Errorf("expected 0 for empty, got %f", got)
	}
}
