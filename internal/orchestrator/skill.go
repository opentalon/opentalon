package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
)

// SkillExtractionConfig configures automatic skill extraction from sessions.
type SkillExtractionConfig struct {
	Enabled      bool `yaml:"enabled"`
	MinToolCalls int  `yaml:"min_tool_calls"` // default 5
	MaxSkills    int  `yaml:"max_skills"`     // max stored skills per actor; default 50
}

func (c SkillExtractionConfig) minToolCalls() int {
	if c.MinToolCalls <= 0 {
		return 5
	}
	return c.MinToolCalls
}

func (c SkillExtractionConfig) maxSkills() int {
	if c.MaxSkills <= 0 {
		return 50
	}
	return c.MaxSkills
}

// SkillExtractor extracts reusable skills from successful multi-tool sessions
// and stores them as tagged memories for injection into future system prompts.
type SkillExtractor struct {
	llm    LLMClient
	memory MemoryStoreInterface
	config SkillExtractionConfig
}

// NewSkillExtractor creates a skill extractor. Returns nil if config is not enabled.
func NewSkillExtractor(llm LLMClient, memory MemoryStoreInterface, config SkillExtractionConfig) *SkillExtractor {
	if !config.Enabled {
		return nil
	}
	return &SkillExtractor{
		llm:    llm,
		memory: memory,
		config: config,
	}
}

// IsSkillWorthy returns true when a session result is complex enough to
// warrant skill extraction.
func (se *SkillExtractor) IsSkillWorthy(result *RunResult) bool {
	if len(result.ToolCalls) >= se.config.minToolCalls() {
		return true
	}
	// Error recovery: a ToolResult with Error followed by a successful call to the same plugin.
	if hasErrorRecovery(result) {
		return true
	}
	return false
}

// hasErrorRecovery detects when the LLM recovered from a tool error by retrying
// the same plugin (possibly with different args).
func hasErrorRecovery(result *RunResult) bool {
	failedPlugins := make(map[string]bool)
	for i, r := range result.Results {
		if i >= len(result.ToolCalls) {
			break
		}
		plugin := result.ToolCalls[i].Plugin
		if r.Error != "" {
			failedPlugins[plugin] = true
		} else if failedPlugins[plugin] {
			return true // success after a prior failure on the same plugin
		}
	}
	return false
}

// ExtractAndStore runs skill extraction asynchronously. Safe to call from a goroutine.
func (se *SkillExtractor) ExtractAndStore(ctx context.Context, result *RunResult, userMessage string) {
	if !se.IsSkillWorthy(result) {
		return
	}
	log := slog.Default()

	trace := buildToolTrace(result)
	prompt := strings.ReplaceAll(prompts.SkillExtract, "{{.UserMessage}}", userMessage)
	prompt = strings.ReplaceAll(prompt, "{{.ToolTrace}}", trace)

	resp, err := se.llm.Complete(ctx, &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "You extract reusable skill definitions from task execution traces."},
			{Role: provider.RoleUser, Content: prompt},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		log.Warn("skill extraction: LLM call failed", "error", err)
		return
	}

	skillYAML := strings.TrimSpace(resp.Content)
	if skillYAML == "" {
		return
	}

	// Prune if at capacity before storing.
	se.maybePrune(ctx)

	actorID := actor.Actor(ctx)
	_, err = se.memory.AddScoped(ctx, actorID, skillYAML, "skill")
	if err != nil {
		log.Warn("skill extraction: failed to store skill", "error", err)
		return
	}
	log.Info("skill extraction: stored new skill", "actor", actorID)
}

// ImproveSkill compares an existing skill with a new execution and updates it if the
// new execution was better.
func (se *SkillExtractor) ImproveSkill(ctx context.Context, existingSkillID, existingSkillContent string, result *RunResult, userMessage string) {
	log := slog.Default()

	trace := buildToolTrace(result)
	prompt := strings.ReplaceAll(prompts.SkillImprove, "{{.ExistingSkill}}", existingSkillContent)
	prompt = strings.ReplaceAll(prompt, "{{.UserMessage}}", userMessage)
	prompt = strings.ReplaceAll(prompt, "{{.ToolTrace}}", trace)

	resp, err := se.llm.Complete(ctx, &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "You improve reusable skill definitions based on new execution traces."},
			{Role: provider.RoleUser, Content: prompt},
		},
		MaxTokens: 1024,
	})
	if err != nil {
		log.Warn("skill improvement: LLM call failed", "error", err)
		return
	}

	improved := strings.TrimSpace(resp.Content)
	if improved == "" || improved == existingSkillContent {
		return
	}

	// Replace: delete old, store new.
	if err := se.memory.Delete(existingSkillID); err != nil {
		log.Warn("skill improvement: failed to delete old skill", "error", err)
		// Continue anyway — storing the new one is still valuable.
	}

	actorID := actor.Actor(ctx)
	_, err = se.memory.AddScoped(ctx, actorID, improved, "skill")
	if err != nil {
		log.Warn("skill improvement: failed to store improved skill", "error", err)
		return
	}
	log.Info("skill improvement: updated skill", "actor", actorID)
}

// FindMatchingSkills returns all skills visible to the current actor.
// Returns the skill memories for injection into the system prompt.
func (se *SkillExtractor) FindMatchingSkills(ctx context.Context) []*matchedSkill {
	memories, err := se.memory.MemoriesForContext(ctx, "skill")
	if err != nil {
		slog.Default().Warn("skill lookup failed", "error", err)
		return nil
	}
	var out []*matchedSkill
	for _, m := range memories {
		out = append(out, &matchedSkill{
			memoryID: m.ID,
			content:  m.Content,
		})
	}
	return out
}

// matchedSkill holds a skill that was injected into the system prompt for this session.
type matchedSkill struct {
	memoryID string
	content  string
}

// maybePrune removes the oldest skill if at capacity.
func (se *SkillExtractor) maybePrune(ctx context.Context) {
	memories, err := se.memory.MemoriesForContext(ctx, "skill")
	if err != nil || len(memories) < se.config.maxSkills() {
		return
	}
	// Memories are returned DESC by created_at; last one is oldest.
	oldest := memories[len(memories)-1]
	if err := se.memory.Delete(oldest.ID); err != nil {
		slog.Default().Warn("skill prune: failed to delete oldest skill", "error", err)
	}
}

// buildToolTrace formats tool calls and results into a readable trace for the LLM.
func buildToolTrace(result *RunResult) string {
	var sb strings.Builder
	for i, call := range result.ToolCalls {
		fmt.Fprintf(&sb, "- Plugin: %s, Action: %s", call.Plugin, call.Action)
		if len(call.Args) > 0 {
			fmt.Fprintf(&sb, ", Args: %v", call.Args)
		}
		sb.WriteString("\n")
		if i < len(result.Results) {
			r := result.Results[i]
			if r.Error != "" {
				fmt.Fprintf(&sb, "  Result: ERROR: %s\n", r.Error)
			} else {
				preview := r.Content
				if len(preview) > 300 {
					preview = preview[:300] + "..."
				}
				fmt.Fprintf(&sb, "  Result: %s\n", preview)
			}
		}
	}
	return sb.String()
}

// skillContextKey is the context key for matched skills injected during a session.
type skillContextKeyType struct{}

var skillContextKey = skillContextKeyType{}

type matchedSkillsCtx struct {
	skills []*matchedSkill
}

func withMatchedSkills(ctx context.Context, skills []*matchedSkill) context.Context {
	return context.WithValue(ctx, skillContextKey, &matchedSkillsCtx{skills: skills})
}

func matchedSkillsFromContext(ctx context.Context) []*matchedSkill {
	v, ok := ctx.Value(skillContextKey).(*matchedSkillsCtx)
	if !ok || v == nil {
		return nil
	}
	return v.skills
}

// skillNudgeInterval is the number of tool-calling rounds after which the
// orchestrator will self-check for skill extraction.
const skillNudgeInterval = 10

// shouldNudgeSkillExtraction returns true every skillNudgeInterval rounds.
func shouldNudgeSkillExtraction(round int) bool {
	return round > 0 && round%skillNudgeInterval == 0
}
