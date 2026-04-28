package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
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
	Name                 string
	Description          string
	Actions              []ActionInfo
	SystemPromptAddition string
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

// PlanTimeout is the maximum time the planner waits for an LLM response.
// The planner is a lightweight classifier (direct vs pipeline); if it takes
// longer than this the agent loop handles the request instead.
const PlanTimeout = 15 * time.Second

// Plan asks the LLM whether the user's message requires a multi-step pipeline or a direct action.
func (p *Planner) Plan(ctx context.Context, message string, capabilities []CapabilityInfo) (*PlanResult, error) {
	lang := ""
	if prof := profile.FromContext(ctx); prof != nil {
		lang = prof.Language
	}
	systemPrompt := buildPlannerPrompt(capabilities, lang)
	slog.Debug("planner system prompt", "prompt", systemPrompt)
	slog.Debug("planner user message", "message", message)

	planCtx, cancel := context.WithTimeout(ctx, PlanTimeout)
	defer cancel()

	resp, err := p.llm.Complete(planCtx, &CompletionRequest{
		Messages: []Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: message},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("planner LLM call: %w", err)
	}

	slog.Debug("planner LLM response", "content", resp.Content)

	return parsePlanResponse(resp.Content)
}

func buildPlannerPrompt(capabilities []CapabilityInfo, language string) string {
	var sb strings.Builder
	sb.WriteString(prompts.PlannerPreamble)
	for _, cap := range capabilities {
		// Server instructions are NOT included in the planner prompt —
		// the planner only decides "direct" vs "pipeline" and doesn't need
		// 18KB of domain knowledge. The main LLM system prompt has them.
		for _, action := range cap.Actions {
			desc := truncatePlannerDescription(action.Description, 200)
			fmt.Fprintf(&sb, "- plugin=%s | action=%s: %s\n", cap.Name, action.Name, desc)
			for _, param := range action.Parameters {
				req := ""
				if param.Required {
					req = " (required)"
				}
				fmt.Fprintf(&sb, "  - %s: %s%s\n", param.Name, truncatePlannerDescription(param.Description, 80), req)
			}
		}
	}
	if language != "" {
		fmt.Fprintf(&sb, "\nThe step names and descriptions must be written in %s.\n", language)
	}
	sb.WriteString("\n")
	sb.WriteString(prompts.PlannerSuffix)
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

// truncatePlannerDescription shortens a description to maxLen characters for the planner prompt.
// The planner only needs a brief summary to decide direct vs pipeline — full descriptions
// (which can include output schemas and usage examples) waste tokens.
func truncatePlannerDescription(s string, maxLen int) string {
	// Strip output schema blocks that inflate descriptions.
	if idx := strings.Index(s, "\n\nOutput schema"); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	// Cut at last space before maxLen to avoid mid-word truncation.
	cut := strings.LastIndex(s[:maxLen], " ")
	if cut < maxLen/2 {
		cut = maxLen
	}
	return s[:cut] + "..."
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

// NarratePlan asks the LLM to describe the pipeline steps in natural language
// and invite the user to confirm or cancel.
func (p *Planner) NarratePlan(ctx context.Context, steps []*Step) (string, error) {
	lang := ""
	if prof := profile.FromContext(ctx); prof != nil {
		lang = prof.Language
	}
	var sb strings.Builder
	sb.WriteString("Plan steps:\n")
	for i, s := range steps {
		fmt.Fprintf(&sb, "%d. %s", i+1, s.Name)
		if s.Command != nil {
			fmt.Fprintf(&sb, " (%s.%s)", s.Command.Plugin, s.Command.Action)
		}
		sb.WriteString("\n")
	}
	systemContent := prompts.PlannerNarrate
	if lang != "" {
		systemContent += fmt.Sprintf(" Respond in %s.", lang)
	}
	resp, err := p.llm.Complete(ctx, &CompletionRequest{
		Messages: []Message{
			{Role: "system", Content: systemContent},
			{Role: "user", Content: sb.String()},
		},
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// ClassifyConfirmation asks the LLM whether the user approved or rejected the plan.
// Returns Rejected on parse failure.
func (p *Planner) ClassifyConfirmation(ctx context.Context, userReply string) (ConfirmationDecision, error) {
	prompt := fmt.Sprintf(
		"The user was shown a multi-step task plan and asked to confirm. They replied: %q\nDid they approve? Respond ONLY with valid JSON: {\"approved\": true} or {\"approved\": false}",
		userReply,
	)
	resp, err := p.llm.Complete(ctx, &CompletionRequest{
		Messages: []Message{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return Rejected, err
	}
	jsonStr := extractJSON(resp.Content)
	var result struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return Rejected, fmt.Errorf("classify confirmation parse: %w", err)
	}
	if result.Approved {
		return Approved, nil
	}
	return Rejected, nil
}
