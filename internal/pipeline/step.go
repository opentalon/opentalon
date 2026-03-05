package pipeline

// StepState represents the execution state of a step.
type StepState string

const (
	StepPending   StepState = "pending"
	StepRunning   StepState = "running"
	StepSucceeded StepState = "succeeded"
	StepFailed    StepState = "failed"
	StepSkipped   StepState = "skipped"
)

// Step is a single unit of work in a pipeline.
type Step struct {
	ID         string
	Name       string
	Command    *PluginCommand
	DependsOn  []string
	State      StepState
	Attempts   int
	MaxRetries int // -1 = use pipeline default
	Result     *StepResult
}

// StepResult holds the outcome of executing a step.
type StepResult struct {
	Success bool
	Output  string
	Data    map[string]any
}
