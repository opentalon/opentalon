package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
)

// SubprocessConfig configures the subprocess (sub-agent) system.
type SubprocessConfig struct {
	Enabled        bool          // master switch
	MaxDepth       int           // max nesting depth (default 2, hard cap 3)
	MaxIterations  int           // default iterations per child (default 5, hard cap 10)
	DefaultTimeout time.Duration // per-subprocess timeout (default 60s)
	MaxParallel    int           // max concurrent children per _subprocess.parallel CALL (default 4, hard cap 8); process-wide in-flight can reach N_calls x MaxParallel under MaxConcurrentSessions > 1
	Models         []string      // chat model ids of the primary provider a RunAction caller may pick with `model`; empty = no check
}

// Bounds for _subprocess.parallel. defaultMaxParallel/maxMaxParallel clamp the
// configured concurrency; maxParallelTasks caps how many tasks one call may
// fan out (the LLM is told to split larger batches) to bound worst-case cost.
const (
	defaultMaxParallel = 4
	maxMaxParallel     = 8
	maxParallelTasks   = 16
)

const subprocessDeadlineMargin = 10 * time.Second

func (o *Orchestrator) subprocessTimeout() time.Duration {
	if o.subprocessConfig.DefaultTimeout <= 0 {
		return 60 * time.Second
	}
	return o.subprocessConfig.DefaultTimeout
}

func (o *Orchestrator) subprocessMaxParallel() int {
	if o.subprocessConfig.MaxParallel <= 0 {
		return defaultMaxParallel
	}
	return min(o.subprocessConfig.MaxParallel, maxMaxParallel)
}

func (o *Orchestrator) callDeadline(call ToolCall) time.Duration {
	if call.Plugin != "_subprocess" {
		return o.guard.Timeout
	}
	batches := 1
	if call.Action == "parallel" {
		if reqs, err := parseParallelRequest(call.Args); err == nil {
			batches = (len(reqs) + o.subprocessMaxParallel() - 1) / o.subprocessMaxParallel()
		}
	}
	return max(o.guard.Timeout, o.subprocessTimeout()*time.Duration(batches)+subprocessDeadlineMargin)
}

// subprocessRequest is parsed from the LLM's _subprocess.run tool call args.
type subprocessRequest struct {
	Task          string   `json:"task"`
	AllowedTools  []string `json:"allowed_tools,omitempty"`
	MaxIterations int      `json:"max_iterations,omitempty"`
	// NoTools runs the sub-agent with zero tools: none are advertised and any
	// tool call is refused. It is distinct from an empty AllowedTools (which
	// means "all tools") — set by tools:"none" for a pure-reasoning turn that
	// must answer directly, e.g. a judgement call that must not have side effects
	// from a prompt-injected task.
	NoTools bool `json:"no_tools,omitempty"`
	// Model is the chat model id the sub-agent runs on; empty means the
	// parent sub-agent's model, or the default model at the top level. It must
	// be one of SubprocessConfig.Models. Only host-side RunAction callers may
	// set it; the LLM cannot (see checkSubprocessModel).
	Model string `json:"model,omitempty"`
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

// Only the model's call in the agent loop may ask the user: only the agent loop takes the answer on the next turn.
type interactiveTurnKey struct{}

func withInteractiveTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveTurnKey{}, true)
}

func withoutInteractiveTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, interactiveTurnKey{}, false)
}

func isInteractiveTurn(ctx context.Context) bool {
	interactive, _ := ctx.Value(interactiveTurnKey{}).(bool)
	return interactive
}

const subprocessNeedsNativeTools = "sub-agents need native tool calling, which this deployment's models do not support"

// subprocessExecutor implements PluginExecutor for the built-in _subprocess plugin.
type subprocessExecutor struct {
	orch *Orchestrator
}

func (s *subprocessExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	if !s.orch.supportsNativeTools() {
		return ToolResult{CallID: call.ID, Error: subprocessNeedsNativeTools}
	}
	ctx = s.orch.withPluginStage(ctx, call)
	if call.Action == "parallel" {
		return s.executeParallel(ctx, call)
	}

	req, err := parseSubprocessRequest(call.Args)
	if err == nil {
		req.Model, err = s.orch.resolveSubprocessModel(ctx, call, req.Model)
	}
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
	if result.Paused != nil {
		return ToolResult{CallID: call.ID, Suspended: &subprocessContinuation{Children: []subprocessChild{*result.Paused}}}
	}

	return ToolResult{CallID: call.ID, Content: result.Response}
}

// parallelTaskResult is one child's outcome, kept for deterministic, in-order
// joining and for the structured payload.
type parallelTaskResult struct {
	Task       string `json:"task"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
}

// executeParallel fans out several independent sub-agent tasks concurrently and
// joins their answers in task order. It reuses runSubprocess per task, so every
// per-child guardrail (iteration cap, timeout, no-recursion) is inherited; the
// only new bound is MaxParallel (concurrency) plus a total-tasks cap.
func (s *subprocessExecutor) executeParallel(ctx context.Context, call ToolCall) ToolResult {
	reqs, err := parseParallelRequest(call.Args)
	for i := 0; err == nil && i < len(reqs); i++ {
		reqs[i].Model, err = s.orch.resolveSubprocessModel(ctx, call, reqs[i].Model)
	}
	if err != nil {
		return ToolResult{CallID: call.ID, Error: err.Error()}
	}

	// Depth clamp mirrors the run path: a parallel call already at the depth
	// cap spawns nothing. Children run one level deeper; because _subprocess is
	// excluded from a sub-agent's tools, they cannot recurse into more parallel/run.
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

	maxParallel := s.orch.subprocessMaxParallel()

	log := slog.With("component", "subprocess", "depth", currentDepth+1, "mode", "parallel")
	log.Info("parallel subprocess started", "tasks", len(reqs), "max_parallel", maxParallel)

	// The semaphore bounds concurrency for THIS call only. Total in-flight
	// sub-agents across the process can reach N_calls x maxParallel when
	// MaxConcurrentSessions > 1 (or, hypothetically, several parallel calls in
	// one turn) — see the config note on max_parallel. This is also the first
	// place runSubprocess runs concurrently within a single session, so every
	// shared dependency it touches (o.llm, o.executeCall, o.guard,
	// o.pluginCallObserver) must be safe for concurrent use — the same contract
	// that already lets MaxConcurrentSessions > 1 run turns in parallel. The one
	// observer implementation, metrics.Collector, is goroutine-safe (Prometheus
	// CounterVec ops are).
	children := make([]subprocessChild, len(reqs))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func(i int, req subprocessRequest) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res, err := s.orch.runSubprocess(ctx, req, currentDepth+1)
			if err == nil && res.Paused != nil {
				children[i] = *res.Paused
			} else {
				children[i] = subprocessChild{Request: req}
				children[i].finish(res, err)
			}
			children[i].Position = i
		}(i, req)
	}
	wg.Wait()

	c := &subprocessContinuation{Children: children}
	if c.Asking = c.waitingChild(); c.Asking >= 0 {
		log.Info("parallel subprocess paused for the user's approval", "tasks", len(reqs))
		return ToolResult{CallID: call.ID, Suspended: c}
	}
	log.Info("parallel subprocess completed", "tasks", len(reqs))
	return joinParallel(call.ID, children)
}

func joinParallel(callID string, children []subprocessChild) ToolResult {
	results := make([]parallelTaskResult, len(children))
	for i, child := range children {
		results[i] = *child.Result
	}
	var sb strings.Builder
	for i, r := range results {
		fmt.Fprintf(&sb, "## Task %d: %s\n", i+1, r.Task)
		if r.Error != "" {
			fmt.Fprintf(&sb, "error: %s\n", r.Error)
		} else {
			sb.WriteString(r.Response)
			sb.WriteString("\n")
		}
		if i < len(results)-1 {
			sb.WriteString("\n")
		}
	}

	// Error ignored: parallelTaskResult is a flat struct of strings/ints, which
	// json.Marshal cannot fail on. structured is "" on the impossible error.
	structured, _ := json.Marshal(results)
	return ToolResult{CallID: callID, Content: strings.TrimRight(sb.String(), "\n"), StructuredContent: string(structured)}
}

// subprocessResult is the outcome of a subprocess execution.
type subprocessResult struct {
	Response   string
	Iterations int
	Ran        []ranCall
	Paused     *subprocessChild
}

// runSubprocess runs a mini agent loop for a focused sub-task.
func (o *Orchestrator) runSubprocess(ctx context.Context, req subprocessRequest, depth int) (*subprocessResult, error) {
	return o.continueSubprocess(ctx, subprocessChild{
		Request: req, Depth: depth, Remaining: o.subprocessTimeout(),
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: buildSubprocessSystemPrompt(req)},
			{Role: provider.RoleUser, Content: req.Task},
		},
	})
}

func (o *Orchestrator) continueSubprocess(ctx context.Context, child subprocessChild) (*subprocessResult, error) {
	req := child.Request
	deadline := time.Now().Add(child.Remaining)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ctx, mayAsk := subprocessContext(ctx, req, child.Depth)

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
	// A no-tools sub-agent has nothing to loop for: it answers in one turn. Pin
	// it to a single iteration so an adversarial task that keeps emitting
	// (refused) tool calls can't burn several model calls — the caller gets
	// exactly one, which is what a determinism/budget guarantee on top of this
	// needs (e.g. talooner's llm_review).
	if req.NoTools {
		maxIter = 1
	}

	log := slog.With("component", "subprocess", "depth", child.Depth)
	log.Info("subprocess started", "task", taskPreview(req.Task), "max_iterations", maxIter, "allowed_tools", len(req.AllowedTools), "model", req.Model,
		"iterations", child.Iterations)

	permitted, tools := o.subprocessTools(ctx, req)
	messages := child.Messages
	result := &subprocessResult{Ran: child.Ran}
	for i := child.Iterations; i < maxIter; i++ {
		guardedMessages, blocked, err := o.runGuardPlugins(ctx, messages)
		if err != nil {
			return result, fmt.Errorf("subprocess guard: %w", err)
		}
		if blocked != nil {
			result.Response = blocked.Response
			result.Iterations = i + 1
			log.Info("subprocess blocked by guard", "iterations", result.Iterations)
			return result, nil
		}

		resp, err := o.llm.Complete(ctx, &provider.CompletionRequest{Messages: guardedMessages, Model: req.Model, Tools: tools})
		if err != nil {
			return result, fmt.Errorf("subprocess LLM: %w", err)
		}
		model := resp.Model
		if model == "" {
			model = req.Model
		}
		o.observePluginUsage(ctx, model, resp.Usage)
		o.recordSubprocessUsage(ctx, model, resp.Usage)

		calls := structuredCalls(resp.ToolCalls)
		if len(calls) == 0 {
			result.Response = StripInternalBlocks(resp.Content)
			result.Iterations = i + 1
			log.Info("subprocess completed", "iterations", result.Iterations)
			return result, nil
		}

		perCallInput := resp.Usage.InputTokens / len(calls)
		perCallOutput := resp.Usage.OutputTokens / len(calls)

		// The response's thinking and text go on its first call's message only, as in the main loop: a replay sends each once.
		thinking := responseThinking{Blocks: resp.Thinking, Fingerprint: resp.ThinkingFingerprint}
		text := resp.Content
		for k, call := range calls {
			toolResult, outcome, err := o.subprocessCall(ctx, permitted, call, mayAsk, perCallInput, perCallOutput)
			if err != nil {
				return result, fmt.Errorf("subprocess call check: %w", err)
			}
			if outcome == gateAwaitApproval {
				messages = append(messages, interruptedCallMessages(call, calls[k+1:], &thinking)...)
				log.Info("subprocess paused for the user's approval", "iterations", i+1, "plugin", call.Plugin, "action", call.Action)
				child.Messages, child.Iterations, child.Remaining, child.Ran = messages, i+1, time.Until(deadline), result.Ran
				child.Waiting = waitingCall{
					continuationCall: continuationCallOf(call), Text: text, responseThinking: takeThinking(&thinking),
					InputTokens: perCallInput, OutputTokens: perCallOutput,
				}
				return &subprocessResult{Paused: &child}, nil
			}
			if outcome == gateExecute {
				result.Ran = append(result.Ran, ranCall{continuationCall: continuationCallOf(call), Error: toolResult.Error})
			}
			assistant, tool := nativeCallMessages(call, text, toolResult)
			messages = append(messages, takeThinking(&thinking).attach(assistant), tool)
			text = ""
		}
	}

	result.Response = "(subprocess reached iteration limit without a final answer)"
	result.Iterations = maxIter
	log.Warn("subprocess hit iteration limit", "iterations", maxIter)
	return result, nil
}

func (o *Orchestrator) subprocessCall(ctx context.Context, permitted map[string]struct{}, call ToolCall, mayAsk bool, inputTokens, outputTokens int) (ToolResult, gateOutcome, error) {
	name := toolFQN(call.Plugin, call.Action)
	if _, ok := permitted[name]; !ok {
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf("tool %s not allowed in this subprocess", name)}, gateAnswered, nil
	}
	decision, err := o.decideConfirmation(ctx, call)
	if err != nil {
		return ToolResult{}, gateAnswered, err
	}
	switch decision.outcome {
	case gateAnswered:
		return decision.result, gateAnswered, nil
	case gateAwaitApproval:
		if mayAsk {
			return ToolResult{}, gateAwaitApproval, nil
		}
		return ToolResult{CallID: call.ID, Error: fmt.Sprintf(
			"tool %s needs the user's approval, which this sub-agent cannot ask for; the call did not run", name)}, gateAnswered, nil
	}
	return o.executeSubprocessCall(ctx, call, inputTokens, outputTokens), gateExecute, nil
}

func (o *Orchestrator) executeSubprocessCall(ctx context.Context, call ToolCall, inputTokens, outputTokens int) ToolResult {
	result := o.executeCall(ctx, call)
	if o.pluginCallObserver != nil {
		o.pluginCallObserver.ObservePluginCall(call.Plugin, call.Action, result.Error != "", inputTokens, outputTokens)
	}
	return result
}

func subprocessContext(ctx context.Context, req subprocessRequest, depth int) (_ context.Context, mayAsk bool) {
	ctx = withSubprocessDepth(ctx, depth)
	if req.Model != "" {
		ctx = context.WithValue(ctx, subprocessModelKey{}, req.Model)
	}
	// The tool-load gate must not judge these calls by the parent's sent set: a load on call would change the parent's tools.
	ctx = withoutSentNativeTools(ctx)
	return withoutInteractiveTurn(ctx), isInteractiveTurn(ctx)
}

func (o *Orchestrator) recordSubprocessUsage(ctx context.Context, model string, u provider.Usage) {
	if o.usageRecorder == nil {
		return
	}
	p := profile.FromContext(ctx)
	if p == nil || p.EntityID == "" {
		if _, warned := o.decideUnattributed.LoadOrStore("_subprocess", true); !warned {
			slog.Warn("subprocess call carries no identity; metering under the unattributed entity",
				"entity", unattributedEntity)
		}
		o.usageRecorder.RecordUsage(ctx, unattributedEntity, "", "", actor.SessionID(ctx),
			model, profile.KindSystem, "subprocess", u.InputTokens, u.OutputTokens, 0, u.InputCost, u.OutputCost, u.Price)
		return
	}
	o.usageRecorder.RecordUsage(ctx, p.EntityID, p.Group, p.ChannelID, actor.SessionID(ctx),
		model, p.Kind, p.SystemSource, u.InputTokens, u.OutputTokens, 0, u.InputCost, u.OutputCost, u.Price)
}

func taskPreview(task string) string {
	if len(task) <= 100 {
		return task
	}
	cut := 100
	for cut > 0 && !utf8.RuneStart(task[cut]) {
		cut--
	}
	return task[:cut] + "..."
}

func buildSubprocessSystemPrompt(req subprocessRequest) string {
	var sb strings.Builder
	sb.WriteString(prompts.SubprocessPreamble)

	if req.NoTools {
		sb.WriteString("\nYou have no tools available. Answer the task directly from what you are given.\n")
	}
	return sb.String()
}

// An allowlist only narrows the tools the caller may use, never adds to them.
func (o *Orchestrator) subprocessTools(ctx context.Context, req subprocessRequest) (map[string]struct{}, []provider.ToolDefinition) {
	if req.NoTools {
		return nil, nil
	}
	callerTools := allowedToolsSet(ctx, o)
	listed := make(map[string]bool, len(req.AllowedTools))
	for _, t := range req.AllowedTools {
		listed[t] = true
	}
	permitted := make(map[string]struct{})
	var defs []provider.ToolDefinition
	for _, cap := range o.registry.ListCapabilities() {
		if cap.Name == "_subprocess" {
			continue
		}
		for _, action := range cap.Actions {
			fqn := toolFQN(cap.Name, action.Name)
			if _, ok := callerTools[fqn]; !ok {
				continue
			}
			if len(listed) > 0 && !listed[fqn] {
				continue
			}
			permitted[fqn] = struct{}{}
			defs = append(defs, toolDefinition(fqn, action, true))
		}
	}
	return permitted, defs
}

// resolveSubprocessModel returns the model a sub-agent runs on. An explicit
// `model` is only accepted from host-side callers (a plugin's RunAction, the
// scheduler): the LLM is never offered the arg, so a prompt-injected task
// cannot fan out work onto the most expensive model. It must be one of
// SubprocessConfig.Models, so a typo fails loudly instead of 4xx-ing at the
// provider or silently running on the default; with no list nothing is
// checked. With no explicit model a nested sub-agent inherits its parent's.
func (o *Orchestrator) resolveSubprocessModel(ctx context.Context, call ToolCall, model string) (string, error) {
	if model == "" {
		return subprocessModel(ctx), nil
	}
	if call.FromLLM {
		return "", fmt.Errorf("model is not selectable from a tool call; omit it")
	}
	models := o.subprocessConfig.Models
	if len(models) == 0 || slices.Contains(models, model) {
		return model, nil
	}
	return "", fmt.Errorf("unknown model %q for subprocess (configured: %s)", model, strings.Join(models, ", "))
}

// subprocessModelKey carries a sub-agent's model so its own sub-agents inherit it.
type subprocessModelKey struct{}

func subprocessModel(ctx context.Context) string {
	m, _ := ctx.Value(subprocessModelKey{}).(string)
	return m
}

// parseSubprocessRequest parses tool call args into a subprocessRequest.
func parseSubprocessRequest(args map[string]string) (subprocessRequest, error) {
	task := args["task"]
	if task == "" {
		return subprocessRequest{}, fmt.Errorf("subprocess requires a 'task' argument")
	}

	req := subprocessRequest{Task: task, Model: strings.TrimSpace(args["model"])}

	if tools := strings.TrimSpace(args["tools"]); strings.EqualFold(tools, noToolsSentinel) {
		req.NoTools = true
	} else if tools != "" {
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

// parseParallelRequest parses the _subprocess.parallel `tasks` arg — a JSON
// array of {task, tools?, max_iterations?} — into per-child requests. Each
// entry reuses the same fields as _subprocess.run. Rejects empty batches,
// entries without a task, and batches over the total-tasks cap (the LLM is
// told to split larger ones).
func parseParallelRequest(args map[string]string) ([]subprocessRequest, error) {
	raw := strings.TrimSpace(args["tasks"])
	if raw == "" {
		return nil, fmt.Errorf("_subprocess.parallel requires a 'tasks' argument (a JSON array of task objects)")
	}

	var items []struct {
		Task          string `json:"task"`
		Tools         string `json:"tools"`
		MaxIterations int    `json:"max_iterations"`
		Model         string `json:"model"`
	}
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("invalid 'tasks' JSON: %v (expected an array like [{\"task\":\"...\"}])", err)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("'tasks' must contain at least one task")
	}
	if len(items) > maxParallelTasks {
		return nil, fmt.Errorf("too many parallel tasks: %d (max %d); split into smaller batches", len(items), maxParallelTasks)
	}

	reqs := make([]subprocessRequest, 0, len(items))
	for i, it := range items {
		task := strings.TrimSpace(it.Task)
		if task == "" {
			return nil, fmt.Errorf("task %d is missing a 'task' field", i+1)
		}
		req := subprocessRequest{Task: task, MaxIterations: it.MaxIterations, Model: strings.TrimSpace(it.Model)}
		if strings.EqualFold(strings.TrimSpace(it.Tools), noToolsSentinel) {
			req.NoTools = true
		} else {
			for _, t := range strings.Split(it.Tools, ",") {
				if t = strings.TrimSpace(t); t != "" {
					req.AllowedTools = append(req.AllowedTools, t)
				}
			}
		}
		reqs = append(reqs, req)
	}
	return reqs, nil
}

// noToolsSentinel is the value of the `tools` arg that requests a sub-agent with
// no tools at all. It is a distinct word rather than the empty string because an
// empty allowlist already means "all tools", and a caller needs a way to say the
// opposite (a pure-reasoning turn with no side effects).
const noToolsSentinel = "none"
