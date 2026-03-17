package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
)

// LLMClient is the interface the planner uses for LLM calls, matching orchestrator.LLMClient.
type LLMClient interface {
	Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
}

// CompletionRequest mirrors provider.CompletionRequest for planner use.
type CompletionRequest struct {
	Messages []Message
}

// CompletionResponse mirrors provider.CompletionResponse.
type CompletionResponse struct {
	Content string
}

// Message mirrors provider.Message.
type Message struct {
	Role    string
	Content string
}

// CapabilityInfo describes a plugin's capabilities for the planner prompt.
type CapabilityInfo struct {
	Name        string
	Description string
	Actions     []ActionInfo
}

// ActionInfo describes a single action.
type ActionInfo struct {
	Name        string
	Description string
	Parameters  []ParamInfo
}

// ParamInfo describes a parameter.
type ParamInfo struct {
	Name        string
	Description string
	Required    bool
}

// Planner uses an LLM to decompose user requests into pipeline steps.
type Planner struct {
	llm LLMClient
}

// PlanResult holds the planner's decision: either "direct" (single action, use normal agent loop)
// or "pipeline" with multiple steps.
type PlanResult struct {
	Type  string  // "direct" | "pipeline"
	Steps []*Step // only when Type == "pipeline"
}

// NewPlanner creates a planner backed by the given LLM client.
func NewPlanner(llm LLMClient) *Planner {
	return &Planner{llm: llm}
}

// planResponse is the JSON shape we expect from the LLM.
type planResponse struct {
	Type  string         `json:"type"`
	Steps []planStepJSON `json:"steps"`
}

type planStepJSON struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Plugin    string            `json:"plugin"`
	Action    string            `json:"action"`
	Args      map[string]string `json:"args"`
	DependsOn []string          `json:"depends_on"`
}

// Plan asks the LLM whether the user's message requires a multi-step pipeline or a direct action.
func (p *Planner) Plan(ctx context.Context, message string, capabilities []CapabilityInfo) (*PlanResult, error) {
	systemPrompt := buildPlannerPrompt(capabilities)
	debug := os.Getenv("LOG_LEVEL") == "debug"

	if debug {
		log.Printf("[pipeline:planner] system prompt:\n%s", systemPrompt)
		log.Printf("[pipeline:planner] user message: %q", message)
	}

	resp, err := p.llm.Complete(ctx, &CompletionRequest{
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: message},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("planner LLM call: %w", err)
	}

	if debug {
		log.Printf("[pipeline:planner] LLM response: %s", resp.Content)
	}

	return parsePlanResponse(resp.Content)
}

func buildPlannerPrompt(capabilities []CapabilityInfo) string {
	var sb strings.Builder
	sb.WriteString(`You are a task planner. Given the user's request and available tools, decide:
1. If this is a simple single-action request, return: {"type": "direct"}
2. If this requires multiple steps, return: {"type": "pipeline", "steps": [...]}

Each step must have: {"id": "<unique>", "name": "<human description>", "plugin": "<plugin>", "action": "<action>", "args": {}, "depends_on": []}

Available tools:
`)
	for _, cap := range capabilities {
		for _, action := range cap.Actions {
			fmt.Fprintf(&sb, "- %s.%s: %s\n", cap.Name, action.Name, action.Description)
			for _, param := range action.Parameters {
				req := ""
				if param.Required {
					req = " (required)"
				}
				fmt.Fprintf(&sb, "  - %s: %s%s\n", param.Name, param.Description, req)
			}
		}
	}
	sb.WriteString("\nRespond ONLY with valid JSON, no other text.")
	return sb.String()
}

func parsePlanResponse(content string) (*PlanResult, error) {
	// Try to extract JSON from the response (LLMs sometimes wrap in markdown code blocks)
	jsonStr := extractJSON(content)

	var resp planResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		// Parse failure → fallback to direct
		return &PlanResult{Type: "direct"}, nil
	}

	if resp.Type != "pipeline" || len(resp.Steps) == 0 {
		return &PlanResult{Type: "direct"}, nil
	}

	steps := make([]*Step, len(resp.Steps))
	for i, s := range resp.Steps {
		id := s.ID
		if id == "" {
			id = fmt.Sprintf("%d", i+1)
		}
		args := s.Args
		if args == nil {
			args = make(map[string]string)
		}
		steps[i] = &Step{
			ID:   id,
			Name: s.Name,
			Command: &PluginCommand{
				Plugin: s.Plugin,
				Action: s.Action,
				Args:   args,
			},
			DependsOn:  s.DependsOn,
			State:      StepPending,
			MaxRetries: -1, // use pipeline default
		}
	}

	return &PlanResult{Type: "pipeline", Steps: steps}, nil
}

// extractJSON tries to pull JSON from a response that may be wrapped in markdown code fences.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)

	// Try to find ```json ... ``` blocks
	if idx := strings.Index(s, "```json"); idx >= 0 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end >= 0 {
			return strings.TrimSpace(s[:end])
		}
	}
	if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end >= 0 {
			return strings.TrimSpace(s[:end])
		}
	}

	return s
}
