package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

const maxAgentLoopIterations = 20

type LLMClient interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// ToolCallParser extracts tool calls from LLM response text.
// Returns nil if the response is a final answer (no tool calls).
type ToolCallParser interface {
	Parse(response string) []ToolCall
}

type Orchestrator struct {
	mu       sync.Mutex
	llm      LLMClient
	parser   ToolCallParser
	registry *ToolRegistry
	memory   *state.MemoryStore
	sessions *state.SessionStore
	guard    *Guard
	rules    *RulesConfig
}

func New(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory *state.MemoryStore,
	sessions *state.SessionStore,
) *Orchestrator {
	return NewWithRules(llm, parser, registry, memory, sessions, nil)
}

func NewWithRules(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory *state.MemoryStore,
	sessions *state.SessionStore,
	customRules []string,
) *Orchestrator {
	return &Orchestrator{
		llm:      llm,
		parser:   parser,
		registry: registry,
		memory:   memory,
		sessions: sessions,
		guard:    NewGuard(),
		rules:    NewRulesConfig(customRules),
	}
}

type RunResult struct {
	Response  string
	ToolCalls []ToolCall
	Results   []ToolResult
}

func (o *Orchestrator) Run(ctx context.Context, sessionID, userMessage string) (*RunResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, err := o.sessions.Get(sessionID); err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}

	if err := o.sessions.AddMessage(sessionID, provider.Message{
		Role:    provider.RoleUser,
		Content: userMessage,
	}); err != nil {
		return nil, fmt.Errorf("adding user message: %w", err)
	}

	result := &RunResult{}

	for i := 0; i < maxAgentLoopIterations; i++ {
		sess, _ := o.sessions.Get(sessionID)

		messages := o.buildMessages(sess, userMessage)

		resp, err := o.llm.Complete(ctx, &provider.CompletionRequest{
			Messages: messages,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM completion: %w", err)
		}

		calls := o.parser.Parse(resp.Content)
		if calls == nil {
			result.Response = resp.Content
			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role:    provider.RoleAssistant,
				Content: resp.Content,
			})
			o.maybeRecordWorkflow(result, userMessage)
			return result, nil
		}

		for _, call := range calls {
			toolResult := o.executeCall(ctx, call)
			result.ToolCalls = append(result.ToolCalls, call)
			result.Results = append(result.Results, toolResult)

			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role:    provider.RoleAssistant,
				Content: formatToolCallMessage(call),
			})
			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role:    provider.RoleUser,
				Content: o.guard.WrapContent(toolResult),
			})
		}
	}

	return nil, fmt.Errorf("agent loop exceeded %d iterations", maxAgentLoopIterations)
}

func (o *Orchestrator) buildMessages(sess *state.Session, userMessage string) []provider.Message {
	messages := make([]provider.Message, 0, len(sess.Messages)+3)

	systemPrompt := o.buildSystemPrompt(userMessage)
	messages = append(messages, provider.Message{
		Role:    provider.RoleSystem,
		Content: systemPrompt,
	})

	messages = append(messages, sess.Messages...)

	return messages
}

func (o *Orchestrator) buildSystemPrompt(userMessage string) string {
	var sb strings.Builder
	sb.WriteString("You are an AI assistant with access to the following tools:\n\n")

	sb.WriteString(o.rules.BuildPromptSection())

	caps := o.registry.ListCapabilities()
	for _, cap := range caps {
		sb.WriteString(fmt.Sprintf("## %s\n%s\n", cap.Name, cap.Description))
		for _, action := range cap.Actions {
			sb.WriteString(fmt.Sprintf("- %s.%s: %s\n", cap.Name, action.Name, action.Description))
			for _, p := range action.Parameters {
				req := ""
				if p.Required {
					req = " (required)"
				}
				sb.WriteString(fmt.Sprintf("  - %s: %s%s\n", p.Name, p.Description, req))
			}
		}
		sb.WriteString("\n")
	}

	workflows := o.memory.Search(userMessage)
	workflowMemories := filterByTag(workflows, "workflow")
	if len(workflowMemories) > 0 {
		sb.WriteString("## Relevant past workflows\n")
		for _, m := range workflowMemories {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func filterByTag(memories []*state.Memory, tag string) []*state.Memory {
	var result []*state.Memory
	for _, m := range memories {
		if m.HasTag(tag) {
			result = append(result, m)
		}
	}
	return result
}

func (o *Orchestrator) executeCall(ctx context.Context, call ToolCall) ToolResult {
	exec, ok := o.registry.GetExecutor(call.Plugin)
	if !ok {
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("plugin %q not found", call.Plugin),
		}
	}
	if !o.registry.HasAction(call.Plugin, call.Action) {
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("action %q not found in plugin %q", call.Action, call.Plugin),
		}
	}

	result := o.guard.ExecuteWithTimeout(ctx, exec, call)
	result = o.guard.ValidateResult(call, result)
	result = o.guard.Sanitize(result)
	return result
}

func (o *Orchestrator) maybeRecordWorkflow(result *RunResult, userMessage string) {
	if len(result.ToolCalls) < 2 {
		return
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("trigger: %s\nsteps:\n", userMessage))
	for i, call := range result.ToolCalls {
		sb.WriteString(fmt.Sprintf("  - plugin: %s, action: %s, order: %d\n", call.Plugin, call.Action, i+1))
	}
	sb.WriteString("outcome: success\n")

	o.memory.Add(sb.String(), "workflow")
}

// RunAction executes a single plugin action directly, bypassing the LLM loop.
// Used by the scheduler and other subsystems that need to invoke tools programmatically.
func (o *Orchestrator) RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error) {
	call := ToolCall{
		ID:     fmt.Sprintf("direct-%s-%s", plugin, action),
		Plugin: plugin,
		Action: action,
		Args:   args,
	}

	result := o.executeCall(ctx, call)
	if result.Error != "" {
		return "", fmt.Errorf("%s.%s: %s", plugin, action, result.Error)
	}
	return result.Content, nil
}

func formatToolCallMessage(call ToolCall) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[tool_call] %s.%s", call.Plugin, call.Action))
	if len(call.Args) > 0 {
		sb.WriteString("(")
		first := true
		for k, v := range call.Args {
			if !first {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%s=%s", k, v))
			first = false
		}
		sb.WriteString(")")
	}
	return sb.String()
}

func formatToolResultMessage(result ToolResult) string {
	if result.Error != "" {
		return fmt.Sprintf("[tool_result] error: %s", result.Error)
	}
	return fmt.Sprintf("[tool_result] %s", result.Content)
}
