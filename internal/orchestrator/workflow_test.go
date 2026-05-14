package orchestrator

import "testing"

func TestIsWorkflowCandidate_Disabled(t *testing.T) {
	if isWorkflowCandidate("workflow", 3, false) {
		t.Error("should return false when workflow is disabled")
	}
}

func TestIsWorkflowCandidate(t *testing.T) {
	tests := []struct {
		name      string
		planType  string
		stepCount int
		want      bool
	}{
		{"planner says workflow", "workflow", 2, true},
		{"planner says pipeline with few steps", "pipeline", 2, false},
		{"planner says pipeline with many steps", "pipeline", 5, true},
		{"planner says direct", "direct", 1, false},
		{"large step count triggers heuristic", "pipeline", 10, true},
		{"exactly 5 steps triggers", "pipeline", 5, true},
		{"4 steps does not trigger", "pipeline", 4, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkflowCandidate(tt.planType, tt.stepCount, true); got != tt.want {
				t.Errorf("isWorkflowCandidate(%q, %d) = %v, want %v", tt.planType, tt.stepCount, got, tt.want)
			}
		})
	}
}
