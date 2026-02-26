package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/opentalon/opentalon/internal/lua"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

const maxAgentLoopIterations = 20

// ContentPreparerEntry configures a plugin action to run before the first LLM call.
type ContentPreparerEntry struct {
	Plugin   string
	Action   string
	ArgKey   string // optional, default "text"
	Insecure bool   // if true (default), this preparer cannot run invoke steps; if false (trusted), can invoke
}

type LLMClient interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// ToolCallParser extracts tool calls from LLM response text.
// Returns nil if the response is a final answer (no tool calls).
type ToolCallParser interface {
	Parse(response string) []ToolCall
}

// NoopParser is a parser that never returns tool calls (LLM replies as plain text only).
var NoopParser ToolCallParser = noopParser{}

type noopParser struct{}

func (noopParser) Parse(_ string) []ToolCall { return nil }

type Orchestrator struct {
	mu             sync.Mutex
	llm            LLMClient
	parser         ToolCallParser
	registry       *ToolRegistry
	memory         *state.MemoryStore
	sessions       *state.SessionStore
	guard          *Guard
	rules          *RulesConfig
	preparers      []ContentPreparerEntry
	luaScriptPaths map[string]string // optional; plugin name -> path to .lua script (for "lua:name" preparers)
}

func New(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory *state.MemoryStore,
	sessions *state.SessionStore,
) *Orchestrator {
	return NewWithRules(llm, parser, registry, memory, sessions, nil, nil, nil)
}

func NewWithRules(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory *state.MemoryStore,
	sessions *state.SessionStore,
	customRules []string,
	contentPreparers []ContentPreparerEntry,
	luaScriptPaths map[string]string,
) *Orchestrator {
	return &Orchestrator{
		llm:            llm,
		parser:         parser,
		registry:       registry,
		memory:         memory,
		sessions:       sessions,
		guard:          NewGuard(),
		rules:          NewRulesConfig(customRules),
		preparers:      contentPreparers,
		luaScriptPaths: luaScriptPaths,
	}
}

type RunResult struct {
	Response        string // LLM answer
	InputForDisplay string // optional: what we sent to the LLM (e.g. tool results), for channels that want to show it
	ToolCalls       []ToolCall
	Results         []ToolResult
}

// InvokeStep is one step in a preparer-driven invoke (run this plugin action without LLM).
type InvokeStep struct {
	Plugin string            `json:"plugin"`
	Action string            `json:"action"`
	Args   map[string]string `json:"args"`
}

// invokeStepsUnmarshal accepts "invoke" as either a single object or an array of steps.
type invokeStepsUnmarshal []InvokeStep

func (s *invokeStepsUnmarshal) UnmarshalJSON(data []byte) error {
	var arr []InvokeStep
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	var single InvokeStep
	if err := json.Unmarshal(data, &single); err == nil {
		*s = []InvokeStep{single}
		return nil
	}
	return fmt.Errorf("invoke must be an object or array of { plugin, action, args }")
}

// preparerResponse is the optional JSON shape from a content preparer (guard or invoke).
type preparerResponse struct {
	SendToLLM *bool                `json:"send_to_llm"`
	Message   string               `json:"message"`
	Invoke    invokeStepsUnmarshal `json:"invoke"`
}

func (o *Orchestrator) Run(ctx context.Context, sessionID, userMessage string) (*RunResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, err := o.sessions.Get(sessionID); err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}

	content := userMessage
	// Run content preparers before the first LLM call (config-driven).
	for _, prep := range o.preparers {
		// Lua preparer: plugin "lua:hello-world" runs the script at luaScriptPaths["hello-world"]
		if strings.HasPrefix(prep.Plugin, "lua:") {
			scriptName := strings.TrimPrefix(prep.Plugin, "lua:")
			scriptPath := o.luaScriptPaths[scriptName]
			if scriptPath == "" {
				continue
			}
			result, err := lua.RunPrepare(scriptPath, content)
			if err != nil {
				log.Printf("Warning: lua preparer %s: %v", scriptName, err)
				continue
			}
			if !result.SendToLLM {
				if len(result.InvokeSteps) > 0 {
					steps := make([]InvokeStep, len(result.InvokeSteps))
					for i, s := range result.InvokeSteps {
						steps[i] = InvokeStep{Plugin: s.Plugin, Action: s.Action, Args: s.Args}
					}
					return o.runInvokeSteps(ctx, steps)
				}
				msg := result.Content
				if msg == "" {
					msg = "Request not sent to LLM (Lua guard)."
				}
				return &RunResult{Response: msg}, nil
			}
			content = result.Content
			continue
		}
		argKey := prep.ArgKey
		if argKey == "" {
			argKey = "text"
		}
		if !o.registry.HasAction(prep.Plugin, prep.Action) {
			continue
		}
		call := ToolCall{
			ID:     fmt.Sprintf("preparer-%s-%s", prep.Plugin, prep.Action),
			Plugin: prep.Plugin,
			Action: prep.Action,
			Args:   map[string]string{argKey: content},
		}
		toolResult := o.executeCall(ctx, call)
		if toolResult.Error != "" {
			continue
		}
		// Preparer response convention: JSON with send_to_llm, optional message, optional invoke (single or list).
		var pr preparerResponse
		if err := json.Unmarshal([]byte(toolResult.Content), &pr); err == nil && pr.SendToLLM != nil && !*pr.SendToLLM {
			if len(pr.Invoke) > 0 {
				if prep.Insecure {
					log.Printf("Warning: insecure preparer %s.%s cannot run invoke; ignoring", prep.Plugin, prep.Action)
					continue
				}
				// Run invoke steps in order; pass previous step output as previous_result to next step.
				return o.runInvokeSteps(ctx, pr.Invoke)
			}
			msg := pr.Message
			if msg == "" {
				msg = toolResult.Content
			}
			if msg == "" {
				msg = "Request not sent to LLM (plugin guard)."
			}
			return &RunResult{Response: msg}, nil
		}
		if pr.Message != "" {
			content = pr.Message
		} else {
			content = toolResult.Content
		}
	}

	if err := o.sessions.AddMessage(sessionID, provider.Message{
		Role:    provider.RoleUser,
		Content: content,
	}); err != nil {
		return nil, fmt.Errorf("adding user message: %w", err)
	}

	result := &RunResult{}

	for i := 0; i < maxAgentLoopIterations; i++ {
		sess, _ := o.sessions.Get(sessionID)

		messages := o.buildMessages(sess, content)

		if os.Getenv("LOG_LEVEL") == "debug" {
			log.Printf("[LLM request] round %d, %d messages:", i+1, len(messages))
			for j, m := range messages {
				preview := m.Content
				if len(preview) > 2000 {
					preview = preview[:2000] + "... [truncated]"
				}
				log.Printf("[LLM request]   [%d] %s: %s", j+1, m.Role, preview)
			}
		}

		resp, err := o.llm.Complete(ctx, &provider.CompletionRequest{
			Messages: messages,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM completion: %w", err)
		}

		calls := o.parser.Parse(resp.Content)
		if calls == nil {
			result.Response = resp.Content
			if len(result.Results) > 0 {
				var parts []string
				for _, r := range result.Results {
					if r.Error != "" {
						parts = append(parts, "[error] "+r.Error)
					} else if r.Content != "" {
						parts = append(parts, r.Content)
					}
				}
				result.InputForDisplay = strings.TrimSpace(strings.Join(parts, "\n"))
			}
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
	sb.WriteString("You are an AI assistant with access to the following tools.\n\n")
	sb.WriteString("When you receive plugin or tool results, reply to the user in a brief natural language answer. Do not simply repeat or echo the tool output; use it to answer the user's question or confirm what was done.\n\n")

	sb.WriteString(o.rules.BuildPromptSection())

	// Don't list content-preparer actions as tools; they already ran before this turn.
	preparerAction := make(map[string]bool)
	for _, prep := range o.preparers {
		preparerAction[prep.Plugin+"."+prep.Action] = true
	}
	caps := o.registry.ListCapabilities()
	for _, cap := range caps {
		sb.WriteString(fmt.Sprintf("## %s\n%s\n", cap.Name, cap.Description))
		for _, action := range cap.Actions {
			if preparerAction[cap.Name+"."+action.Name] {
				continue
			}
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

// runInvokeSteps runs a list of plugin actions in order without calling the LLM.
// Each step's result content is passed to the next step as args["previous_result"].
func (o *Orchestrator) runInvokeSteps(ctx context.Context, steps []InvokeStep) (*RunResult, error) {
	const previousResultKey = "previous_result"
	var lastContent string
	var toolCalls []ToolCall
	var results []ToolResult
	for i, step := range steps {
		if step.Plugin == "" || step.Action == "" {
			log.Printf("Warning: invoke step %d missing plugin or action", i+1)
			continue
		}
		if !o.registry.HasAction(step.Plugin, step.Action) {
			log.Printf("Warning: invoke step %d: unknown action %s.%s", i+1, step.Plugin, step.Action)
			continue
		}
		args := make(map[string]string)
		for k, v := range step.Args {
			args[k] = v
		}
		if i > 0 && lastContent != "" {
			args[previousResultKey] = lastContent
		}
		call := ToolCall{
			ID:     fmt.Sprintf("invoke-%d-%s-%s", i+1, step.Plugin, step.Action),
			Plugin: step.Plugin,
			Action: step.Action,
			Args:   args,
		}
		toolResult := o.executeCall(ctx, call)
		toolCalls = append(toolCalls, call)
		results = append(results, toolResult)
		if toolResult.Error != "" {
			return &RunResult{
				Response:  "Invoke step failed: " + toolResult.Error,
				ToolCalls: toolCalls,
				Results:   results,
			}, nil
		}
		lastContent = toolResult.Content
	}
	if lastContent == "" {
		lastContent = "(No output from invoke steps.)"
	}
	return &RunResult{
		Response:        lastContent,
		ToolCalls:       toolCalls,
		Results:         results,
		InputForDisplay: lastContent,
	}, nil
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
