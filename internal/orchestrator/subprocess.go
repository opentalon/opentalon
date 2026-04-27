package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
)

// SubprocessConfig configures the subprocess (sub-agent) system.
type SubprocessConfig struct {
	Enabled        bool          // master switch
	MaxDepth       int           // max nesting depth (default 2, hard cap 3)
	MaxIterations  int           // default iterations per child (default 5, hard cap 10)
	DefaultTimeout time.Duration // per-subprocess timeout (default 60s)
}

// subprocessRequest is parsed from the LLM's _subprocess.run tool call args.
type subprocessRequest struct {
	Task          string
	AllowedTools  []string
	MaxIterations int
}

// subprocessDepthKey is the context key for tracking subprocess nesting depth.
type subprocessDepthKey struct{}

func withSubprocessDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, subprocessDepthKey{}, depth)
}

func subprocessDepth(ctx context.Context) int {
	v, _ := ctx.Value(subprocessDepthKey{}).(int)
	return v
}

// subprocessExecutor implements PluginExecutor for the built-in _subprocess plugin.
type subprocessExecutor struct {
	orch *Orchestrator
}

func (s *subprocessExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	req, err := parseSubprocessRequest(call.Args)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}

	currentDepth := subprocessDepth(ctx)
	maxDepth := s.orch.subprocessConfig.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	if maxDepth > 3 {
		maxDepth = 3
	}
	if currentDepth >= maxDepth {
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("subprocess depth limit reached (depth %d of %d)", currentDepth+1, maxDepth),
		}
	}

	result, err := s.orch.runSubprocess(ctx, req, currentDepth+1)
	if err != nil {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("subprocess failed: %v", err)}
	}

	return ToolResult{CallID: call.ID, Content: result.Response}
}

// subprocessResult is the outcome of a subprocess execution.
type subprocessResult struct {
	Response   string
	Iterations int
}

// runSubprocess runs a mini agent loop for a focused sub-task.
func (o *Orchestrator) runSubprocess(ctx context.Context, req subprocessRequest, depth int) (*subprocessResult, error) {
	timeout := o.subprocessConfig.DefaultTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ctx = withSubprocessDepth(ctx, depth)

	maxIter := req.MaxIterations
	if maxIter <= 0 {
		defaultIter := o.subprocessConfig.MaxIterations
		if defaultIter <= 0 {
			defaultIter = 5
		}
		maxIter = defaultIter
	}
	if maxIter > 10 {
		maxIter = 10
	}

	log := slog.With("component", "subprocess", "depth", depth)
	taskPreview := req.Task
	if len(taskPreview) > 100 {
		taskPreview = taskPreview[:100] + "..."
	}
	log.Info("subprocess started", "task", taskPreview, "max_iterations", maxIter, "allowed_tools", len(req.AllowedTools))

	systemPrompt := o.buildSubprocessSystemPrompt(ctx, req)

	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: systemPrompt},
		{Role: provider.RoleUser, Content: req.Task},
	}

	result := &subprocessResult{}
	for i := 0; i < maxIter; i++ {
		guardedMessages, blocked, err := o.runGuardPlugins(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("subprocess guard: %w", err)
		}
		if blocked != nil {
			result.Response = blocked.Response
			result.Iterations = i + 1
			log.Info("subprocess blocked by guard", "iterations", result.Iterations)
			return result, nil
		}

		resp, err := o.llm.Complete(ctx, &provider.CompletionRequest{Messages: guardedMessages})
		if err != nil {
			return nil, fmt.Errorf("subprocess LLM: %w", err)
		}

		calls := o.parser.Parse(resp.Content)
		if calls == nil {
			result.Response = StripInternalBlocks(resp.Content)
			result.Iterations = i + 1
			log.Info("subprocess completed", "iterations", result.Iterations)
			return result, nil
		}

		perCallInput, perCallOutput := 0, 0
		if len(calls) > 0 {
			perCallInput = resp.Usage.InputTokens / len(calls)
			perCallOutput = resp.Usage.OutputTokens / len(calls)
		}

		for _, call := range calls {
			call.FromLLM = true

			if !isSubprocessToolAllowed(call, req.AllowedTools) {
				tr := ToolResult{
					CallID: call.ID,
					Error:  fmt.Sprintf("tool %s.%s not allowed in this subprocess", call.Plugin, call.Action),
				}
				messages = append(messages,
					provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(call)},
					provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(tr)},
				)
				continue
			}

			toolResult := o.executeCall(ctx, call)

			if o.pluginCallObserver != nil {
				o.pluginCallObserver.ObservePluginCall(call.Plugin, call.Action, toolResult.Error != "", perCallInput, perCallOutput)
			}

			messages = append(messages,
				provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(call)},
				provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(toolResult)},
			)
		}
	}

	result.Response = "(subprocess reached iteration limit without a final answer)"
	result.Iterations = maxIter
	log.Warn("subprocess hit iteration limit", "iterations", maxIter)
	return result, nil
}

// buildSubprocessSystemPrompt builds a focused system prompt for a subprocess
// with only the allowed tools listed. It excludes _subprocess itself to prevent fork bombs.
func (o *Orchestrator) buildSubprocessSystemPrompt(ctx context.Context, req subprocessRequest) string {
	var sb strings.Builder
	sb.WriteString("You are a focused sub-agent. Complete the following task using the available tools.\n")
	sb.WriteString("When you have the answer, respond in plain text without any tool calls.\n\n")
	sb.WriteString("To call a tool, use EXACTLY this format:\n\n")
	sb.WriteString("[tool_call]\n{\"tool\": \"plugin.action\", \"args\": {\"key\": \"value\"}}\n[/tool_call]\n\n")

	// Build allowlist set for fast lookup.
	allowSet := make(map[string]bool, len(req.AllowedTools))
	for _, t := range req.AllowedTools {
		allowSet[t] = true
	}
	hasAllowlist := len(allowSet) > 0

	// Don't list preparer/guard actions.
	internalActions := make(map[string]bool)
	for _, prep := range o.preparers {
		internalActions[prep.Plugin+"."+prep.Action] = true
	}
	for _, g := range o.guards {
		internalActions[g.Plugin+"."+g.Action] = true
	}

	allowedPlugins := o.resolveAllowedPlugins(ctx)
	caps := o.registry.ListCapabilities()

	for _, cap := range caps {
		// Exclude _subprocess to prevent recursion.
		if cap.Name == "_subprocess" {
			continue
		}

		if !o.pluginAllowed(cap, allowedPlugins) {
			continue
		}

		var visibleActions []Action
		for _, action := range cap.Actions {
			if internalActions[cap.Name+"."+action.Name] || action.UserOnly {
				continue
			}
			if hasAllowlist && !allowSet[cap.Name+"."+action.Name] {
				continue
			}
			visibleActions = append(visibleActions, action)
		}

		if len(visibleActions) == 0 {
			continue
		}

		fmt.Fprintf(&sb, "## %s\n%s\n", cap.Name, cap.Description)
		for _, action := range visibleActions {
			fmt.Fprintf(&sb, "- %s.%s: %s\n", cap.Name, action.Name, action.Description)
			for _, p := range action.Parameters {
				reqMark := ""
				if p.Required {
					reqMark = " (required)"
				}
				fmt.Fprintf(&sb, "  - %s: %s%s\n", p.Name, p.Description, reqMark)
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// parseSubprocessRequest parses tool call args into a subprocessRequest.
func parseSubprocessRequest(args map[string]string) (subprocessRequest, error) {
	task := args["task"]
	if task == "" {
		return subprocessRequest{}, fmt.Errorf("subprocess requires a 'task' argument")
	}

	req := subprocessRequest{Task: task}

	if tools := args["tools"]; tools != "" {
		for _, t := range strings.Split(tools, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				req.AllowedTools = append(req.AllowedTools, t)
			}
		}
	}

	if maxIterStr := args["max_iterations"]; maxIterStr != "" {
		n, err := strconv.Atoi(maxIterStr)
		if err != nil {
			return subprocessRequest{}, fmt.Errorf("invalid max_iterations: %s", maxIterStr)
		}
		req.MaxIterations = n
	}

	return req, nil
}

// isSubprocessToolAllowed checks whether a tool call is permitted in a subprocess.
// _subprocess is always blocked (prevents fork bombs). When an allowlist is provided,
// only listed tools are permitted; when empty, all tools except _subprocess are allowed.
func isSubprocessToolAllowed(call ToolCall, allowedTools []string) bool {
	if call.Plugin == "_subprocess" {
		return false
	}
	if len(allowedTools) == 0 {
		return true
	}
	key := call.Plugin + "." + call.Action
	for _, t := range allowedTools {
		if t == key {
			return true
		}
	}
	return false
}
