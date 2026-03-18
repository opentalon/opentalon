package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/lua"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/sessionlog"
	"github.com/opentalon/opentalon/internal/state"
)

const maxAgentLoopIterations = 20

// PermissionAction is the fixed action name the core uses when calling the permission plugin.
const PermissionAction = "check"

// ContentPreparerEntry configures a plugin action to run before the first LLM call.
// If Guard is true, the plugin also runs before every LLM call in the agent loop to sanitize messages and prevent prompt injection.
type ContentPreparerEntry struct {
	Plugin   string
	Action   string
	ArgKey   string // optional, default "text"
	Insecure bool   // if true (default), this preparer cannot run invoke steps; if false (trusted), can invoke
	Guard    bool   // if true, also runs before every LLM call to sanitize messages
	FailOpen bool   // if true, guard/preparer failures are logged and skipped; default false (fail-closed)
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

// PermissionChecker is called before running a tool to decide if the actor is allowed to use the plugin.
type PermissionChecker interface {
	Allowed(ctx context.Context, actorID, plugin string) (bool, error)
}

// ContextArgProvider returns a value for a named context arg (e.g. "session_id") from the request context.
// Used to inject args into tool calls when an action declares InjectContextArgs.
type ContextArgProvider func(ctx context.Context, name string) string

// OrchestratorOpts holds optional configuration for NewWithRules. Zero values mean defaults (no permission check, no summarization).
type OrchestratorOpts struct {
	CustomRules             []string
	ContentPreparers        []ContentPreparerEntry
	LuaScriptPaths          map[string]string
	PermissionChecker       PermissionChecker
	PermissionPluginName    string
	RuntimePromptPath       string                        // optional path to editable prompt file (e.g. data_dir/custom_prompt.txt); appended to system prompt
	ContextArgProviders     map[string]ContextArgProvider // optional; if nil, default providers (e.g. session_id) are used
	SummarizeAfterMessages  int                           // 0 = off
	MaxMessagesAfterSummary int                           // keep this many messages after summarization
	SummarizePrompt         string                        // empty = default English
	SummarizeUpdatePrompt   string                        // empty = default English
	PipelineEnabled         bool                          // when true, create Planner from llm
	PipelineConfig          pipeline.PipelineConfig
	ContextWindow           int                 // model context window in tokens; 0 = no trimming
	SessionLogManager       *sessionlog.Manager // optional; when set, per-session debug logs go to individual files
	MaxConcurrentSessions   int                 // max sessions running in parallel; default 1 (sequential)
}

// MemoryStoreInterface is the scoped memory store used for general + per-actor memories.
type MemoryStoreInterface interface {
	AddScoped(ctx context.Context, actorID string, content string, tags ...string) (*state.Memory, error)
	MemoriesForContext(ctx context.Context, tag string) ([]*state.Memory, error)
}

// SessionStoreInterface is the session store (in-memory or SQLite).
type SessionStoreInterface interface {
	Get(id string) (*state.Session, error)
	Create(id string) *state.Session
	AddMessage(id string, msg provider.Message) error
	SetModel(id string, model provider.ModelRef) error
	SetSummary(id string, summary string, messages []provider.Message) error // for summarization; optional, may be no-op
	Delete(id string) error                                                  // remove session (e.g. for clear_session command)
}

// sessionMutex is a per-session lock with reference counting for cleanup.
type sessionMutex struct {
	mu       sync.Mutex
	refCount int // goroutines currently waiting or holding this lock
}

type Orchestrator struct {
	sessionMuxMu            sync.Mutex               // guards sessionMuxes map
	sessionMuxes            map[string]*sessionMutex // per-session serialization
	semaphore               chan struct{}            // nil = unlimited; cap = MaxConcurrentSessions
	llm                     LLMClient
	parser                  ToolCallParser
	registry                *ToolRegistry
	memory                  MemoryStoreInterface
	sessions                SessionStoreInterface
	guard                   *Guard
	rules                   *RulesConfig
	preparers               []ContentPreparerEntry
	guards                  []ContentPreparerEntry        // subset of preparers with Guard:true; run before every LLM call
	luaScriptPaths          map[string]string             // optional; plugin name -> path to .lua script (for "lua:name" preparers)
	permissionChecker       PermissionChecker             // optional; when set, executeCall checks permission before running
	permissionPluginName    string                        // name of the permission plugin (skip permission check when executing it)
	runtimePromptPath       string                        // optional; if set, buildSystemPrompt appends file contents
	contextArgProviders     map[string]ContextArgProvider // name -> extract from context; used to inject args per action
	summarizeAfterMessages  int                           // 0 = off; after this many messages run summarization
	maxMessagesAfterSummary int                           // keep this many messages after summarization
	summarizePrompt         string                        // system prompt for initial summarization (config; empty = default English)
	summarizeUpdatePrompt   string                        // system prompt for updating summary (config; empty = default English)
	planner                 *pipeline.Planner             // nil = pipeline disabled
	pendingMu               sync.Mutex                    // guards pendingPipelines map
	pendingPipelines        map[string]*pipeline.Pipeline // sessionID -> pending pipeline (access via pendingMu)
	pipelineConfig          pipeline.PipelineConfig
	contextWindow           int                 // model context window in tokens; 0 = no trimming
	sessionLog              *sessionlog.Manager // optional; per-session log file manager
}

const (
	defaultSummarizePrompt       = "Summarize the following conversation in a short paragraph."
	defaultSummarizeUpdatePrompt = "Update the given conversation summary with the following new exchange. Keep the result to a short paragraph."
)

// defaultContextArgProviders returns built-in providers only for opaque identifiers (e.g. session_id).
// No session messages, conversation text, or other sensitive content is exposed to plugins via this mechanism.
func defaultContextArgProviders(custom map[string]ContextArgProvider) map[string]ContextArgProvider {
	builtin := map[string]ContextArgProvider{
		"session_id": func(ctx context.Context, _ string) string { return actor.SessionID(ctx) },
	}
	if len(custom) == 0 {
		return builtin
	}
	out := make(map[string]ContextArgProvider, len(builtin)+len(custom))
	for k, v := range builtin {
		out[k] = v
	}
	for k, v := range custom {
		out[k] = v
	}
	return out
}

func New(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory MemoryStoreInterface,
	sessions SessionStoreInterface,
) *Orchestrator {
	return NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{})
}

func NewWithRules(
	llm LLMClient,
	parser ToolCallParser,
	registry *ToolRegistry,
	memory MemoryStoreInterface,
	sessions SessionStoreInterface,
	opts OrchestratorOpts,
) *Orchestrator {
	if opts.SummarizePrompt == "" {
		opts.SummarizePrompt = defaultSummarizePrompt
	}
	if opts.SummarizeUpdatePrompt == "" {
		opts.SummarizeUpdatePrompt = defaultSummarizeUpdatePrompt
	}
	var planner *pipeline.Planner
	if opts.PipelineEnabled {
		planner = pipeline.NewPlanner(&plannerLLMAdapter{llm: llm})
	}
	pipelineCfg := opts.PipelineConfig
	if pipelineCfg.MaxStepRetries == 0 && pipelineCfg.StepTimeout == 0 {
		pipelineCfg = pipeline.DefaultConfig()
	}
	var preparers, guards []ContentPreparerEntry
	for _, p := range opts.ContentPreparers {
		if p.Guard {
			guards = append(guards, p)
		} else {
			preparers = append(preparers, p)
		}
	}
	maxConcurrent := opts.MaxConcurrentSessions
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	// Always create a semaphore. cap=1 = sequential (default); cap=N = N parallel sessions.
	semaphore := make(chan struct{}, maxConcurrent)

	return &Orchestrator{
		sessionMuxes:            make(map[string]*sessionMutex),
		semaphore:               semaphore,
		llm:                     llm,
		parser:                  parser,
		registry:                registry,
		memory:                  memory,
		sessions:                sessions,
		guard:                   NewGuard(),
		rules:                   NewRulesConfig(opts.CustomRules),
		preparers:               preparers,
		guards:                  guards,
		luaScriptPaths:          opts.LuaScriptPaths,
		permissionChecker:       opts.PermissionChecker,
		permissionPluginName:    opts.PermissionPluginName,
		runtimePromptPath:       opts.RuntimePromptPath,
		contextArgProviders:     defaultContextArgProviders(opts.ContextArgProviders),
		summarizeAfterMessages:  opts.SummarizeAfterMessages,
		maxMessagesAfterSummary: opts.MaxMessagesAfterSummary,
		summarizePrompt:         opts.SummarizePrompt,
		summarizeUpdatePrompt:   opts.SummarizeUpdatePrompt,
		planner:                 planner,
		pendingPipelines:        make(map[string]*pipeline.Pipeline),
		pipelineConfig:          pipelineCfg,
		contextWindow:           opts.ContextWindow,
		sessionLog:              opts.SessionLogManager,
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

func (o *Orchestrator) handlePreparerFailure(prep ContentPreparerEntry, details string) *RunResult {
	name := prep.Plugin + "." + prep.Action
	if strings.HasPrefix(prep.Plugin, "lua:") {
		name = prep.Plugin
	}
	log.Printf("Warning: guard %s failed: %s", name, details)
	if prep.FailOpen {
		return nil
	}
	return &RunResult{Response: fmt.Sprintf("Request blocked: guard %s failed.", name)}
}

func (o *Orchestrator) runSinglePreparer(ctx context.Context, prep ContentPreparerEntry, content, callPrefix string, allowInvoke bool) (string, *RunResult, error) {
	if strings.HasPrefix(prep.Plugin, "lua:") {
		scriptName := strings.TrimPrefix(prep.Plugin, "lua:")
		scriptPath := o.luaScriptPaths[scriptName]
		if scriptPath == "" {
			if blocked := o.handlePreparerFailure(prep, "Lua script path not found"); blocked != nil {
				return content, blocked, nil
			}
			return content, nil, nil
		}
		result, err := lua.RunPrepare(scriptPath, content)
		if err != nil {
			if blocked := o.handlePreparerFailure(prep, err.Error()); blocked != nil {
				return content, blocked, nil
			}
			return content, nil, nil
		}
		if !result.SendToLLM {
			if allowInvoke && len(result.InvokeSteps) > 0 {
				steps := make([]InvokeStep, len(result.InvokeSteps))
				for i, s := range result.InvokeSteps {
					steps[i] = InvokeStep{Plugin: s.Plugin, Action: s.Action, Args: s.Args}
				}
				invokeResult, err := o.runInvokeSteps(ctx, steps)
				return "", invokeResult, err
			}
			msg := result.Content
			if msg == "" {
				msg = "Request blocked by guard."
			}
			return "", &RunResult{Response: msg}, nil
		}
		return result.Content, nil, nil
	}

	argKey := prep.ArgKey
	if argKey == "" {
		argKey = "text"
	}
	if !o.registry.HasAction(prep.Plugin, prep.Action) {
		if blocked := o.handlePreparerFailure(prep, "action not found"); blocked != nil {
			return content, blocked, nil
		}
		return content, nil, nil
	}
	call := ToolCall{
		ID:     fmt.Sprintf("%s-%s-%s", callPrefix, prep.Plugin, prep.Action),
		Plugin: prep.Plugin,
		Action: prep.Action,
		Args:   map[string]string{argKey: content},
	}
	toolResult := o.executeCall(ctx, call)
	if toolResult.Error != "" {
		if blocked := o.handlePreparerFailure(prep, toolResult.Error); blocked != nil {
			return content, blocked, nil
		}
		return content, nil, nil
	}
	var pr preparerResponse
	if err := json.Unmarshal([]byte(toolResult.Content), &pr); err == nil && pr.SendToLLM != nil && !*pr.SendToLLM {
		if allowInvoke && len(pr.Invoke) > 0 {
			if prep.Insecure {
				log.Printf("Warning: insecure preparer %s.%s cannot run invoke; ignoring", prep.Plugin, prep.Action)
				return content, nil, nil
			}
			invokeResult, err := o.runInvokeSteps(ctx, pr.Invoke)
			return "", invokeResult, err
		}
		msg := pr.Message
		if msg == "" {
			msg = toolResult.Content
		}
		if msg == "" {
			msg = "Request blocked by guard."
		}
		return "", &RunResult{Response: msg}, nil
	}
	if pr.Message != "" {
		return pr.Message, nil, nil
	}
	return toolResult.Content, nil, nil
}

// acquireSessionLock returns the per-session mutex for sessionID, locked.
func (o *Orchestrator) acquireSessionLock(sessionID string) *sessionMutex {
	o.sessionMuxMu.Lock()
	sm, ok := o.sessionMuxes[sessionID]
	if !ok {
		sm = &sessionMutex{}
		o.sessionMuxes[sessionID] = sm
	}
	sm.refCount++
	o.sessionMuxMu.Unlock()

	sm.mu.Lock()
	return sm
}

// releaseSessionLock unlocks sm and removes it from the map if no other goroutine holds a reference.
func (o *Orchestrator) releaseSessionLock(sessionID string, sm *sessionMutex) {
	o.sessionMuxMu.Lock()
	sm.refCount--
	if sm.refCount == 0 {
		delete(o.sessionMuxes, sessionID)
	}
	o.sessionMuxMu.Unlock()

	sm.mu.Unlock()
}

func (o *Orchestrator) Run(ctx context.Context, sessionID, userMessage string) (*RunResult, error) {
	// Lock ordering (must always be acquired in this sequence to prevent deadlock):
	//   1. semaphore      – global concurrency cap
	//   2. sessionMuxes   – per-session serialization (via acquireSessionLock)
	//   3. pendingMu      – pending-pipeline map
	// Never acquire an earlier lock while holding a later one.
	select {
	case o.semaphore <- struct{}{}:
		defer func() { <-o.semaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Per-session lock: serializes concurrent messages for the same session.
	sm := o.acquireSessionLock(sessionID)
	defer o.releaseSessionLock(sessionID, sm)

	if _, err := o.sessions.Get(sessionID); err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}
	ctx = actor.WithSessionID(ctx, sessionID)

	// Get per-session logger (nil when session log manager is not configured).
	// A nil *sessionlog.Logger falls back to global log; a nil check gates debug-only output.
	var slog *sessionlog.Logger
	if o.sessionLog != nil {
		slog = o.sessionLog.Get(sessionID, userMessage)
	}

	// Block A: Check for pending pipeline confirmation.
	o.pendingMu.Lock()
	pendingPipeline := o.pendingPipelines[sessionID]
	if pendingPipeline != nil {
		delete(o.pendingPipelines, sessionID)
	}
	o.pendingMu.Unlock()
	if p := pendingPipeline; p != nil {
		decision := pipeline.ParseConfirmation(userMessage)
		if slog != nil {
			slog.Printf("[pipeline] pending pipeline %s for session %s, user input: %q, decision: %d", p.ID, sessionID, userMessage, decision)
		}
		_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: userMessage})
		if decision == pipeline.Approved {
			if slog != nil {
				slog.Printf("[pipeline] executing pipeline %s (%d steps)", p.ID, len(p.Steps))
			}
			return o.executePipeline(ctx, sessionID, p, slog)
		}
		resp := "Pipeline cancelled (expected y/yes to confirm)."
		_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: resp})
		if slog != nil {
			slog.Printf("[pipeline] pipeline %s rejected: %q", p.ID, userMessage)
		}
		return &RunResult{Response: resp}, nil
	}

	content := userMessage
	// Run content preparers before the first LLM call (config-driven).
	for _, prep := range o.preparers {
		guardedContent, blocked, err := o.runSinglePreparer(ctx, prep, content, "preparer", true)
		if err != nil {
			return nil, err
		}
		if blocked != nil {
			return blocked, nil
		}
		content = guardedContent
	}

	// Block B: Run planner to check if this requires a multi-step pipeline.
	if o.planner != nil {
		if slog != nil {
			slog.Printf("[pipeline] running planner for session %s, message: %q", sessionID, content)
		}
		planResult, err := o.planner.Plan(ctx, content, capabilitiesToPlannerInfo(o.registry.ListCapabilities()))
		if err != nil {
			if slog != nil {
				slog.Printf("[pipeline] planner error: %v — falling through to agent loop", err)
			}
		} else if slog != nil {
			slog.Printf("[pipeline] planner result: type=%s, steps=%d", planResult.Type, len(planResult.Steps))
		}
		if err == nil && planResult.Type == "pipeline" && len(planResult.Steps) > 1 {
			p := pipeline.NewPipeline(planResult.Steps, o.pipelineConfig)
			o.pendingMu.Lock()
			o.pendingPipelines[sessionID] = p
			o.pendingMu.Unlock()
			planText := p.FormatForConfirmation()
			_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: content})
			_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: planText})
			if slog != nil {
				slog.Printf("[pipeline] stored pending pipeline %s for session %s (%d steps), awaiting confirmation", p.ID, sessionID, len(p.Steps))
			}
			return &RunResult{Response: planText}, nil
		}
		// If "direct" or error or single step, fall through to normal agent loop
	}

	if err := o.sessions.AddMessage(sessionID, provider.Message{
		Role:    provider.RoleUser,
		Content: content,
	}); err != nil {
		return nil, fmt.Errorf("adding user message: %w", err)
	}
	// Run summarization asynchronously so it doesn't block the user's request.
	go o.maybeSummarizeSession(context.Background(), sessionID)

	result := &RunResult{}

	for i := 0; i < maxAgentLoopIterations; i++ {
		sess, _ := o.sessions.Get(sessionID)

		messages := o.buildMessages(ctx, sess, content, slog)
		guardedMessages, blocked, err := o.runGuardPlugins(ctx, messages)
		if err != nil {
			return nil, err
		}
		if blocked != nil {
			return blocked, nil
		}

		if slog != nil {
			slog.Printf("[LLM request] round %d, %d messages:", i+1, len(guardedMessages))
			for j, m := range guardedMessages {
				preview := m.Content
				if len(preview) > 2000 {
					preview = preview[:2000] + "... [truncated]"
				}
				slog.Printf("[LLM request]   [%d] %s: %s", j+1, m.Role, preview)
			}
		}

		resp, err := o.llm.Complete(ctx, &provider.CompletionRequest{
			Messages: guardedMessages,
		})
		if err != nil {
			return nil, fmt.Errorf("LLM completion: %w", err)
		}

		calls := o.parser.Parse(resp.Content)
		if calls == nil {
			result.Response = resp.Content
			if len(result.Results) > 0 {
				var parts []string
				for i, r := range result.Results {
					if i < len(result.ToolCalls) {
						parts = append(parts, formatToolCallMessage(result.ToolCalls[i]))
					}
					if r.Error != "" {
						parts = append(parts, "[tool_result] error: "+r.Error)
					} else if r.Content != "" {
						preview := r.Content
						if len(preview) > 500 {
							preview = preview[:500] + "..."
						}
						parts = append(parts, "[tool_result] "+preview)
					}
				}
				result.InputForDisplay = strings.TrimSpace(strings.Join(parts, "\n"))
			}
			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role:    provider.RoleAssistant,
				Content: resp.Content,
			})
			o.maybeRecordWorkflow(ctx, result, userMessage)
			return result, nil
		}

		for i := range calls {
			calls[i].FromLLM = true
			toolResult := o.executeCall(ctx, calls[i])
			call := calls[i]
			result.ToolCalls = append(result.ToolCalls, call)
			result.Results = append(result.Results, toolResult)

			if toolResult.Error != "" {
				// Always log tool errors (nil slog falls back to global log).
				slog.Printf("[tool_result] %s.%s error: %s", call.Plugin, call.Action, toolResult.Error)
			}

			// When the session is cleared, close the per-session log so the
			// next Run() creates a fresh file for the new conversation.
			if call.Action == "clear_session" && toolResult.Error == "" && o.sessionLog != nil {
				o.sessionLog.Close(sessionID)
				slog = nil
			}

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

// runGuardPlugins runs all guard plugins on the last non-system message before an LLM call.
// Guards sanitize content to prevent prompt injection from tool results or user input.
func (o *Orchestrator) runGuardPlugins(ctx context.Context, messages []provider.Message) ([]provider.Message, *RunResult, error) {
	if len(o.guards) == 0 {
		return messages, nil, nil
	}
	// Find the last non-system message (the content most at risk of injection).
	lastIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != provider.RoleSystem {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 {
		return messages, nil, nil
	}
	original := messages[lastIdx].Content
	content := original
	for _, g := range o.guards {
		nextContent, blocked, err := o.runSinglePreparer(ctx, g, content, "guard", false)
		if err != nil {
			return nil, nil, err
		}
		if blocked != nil {
			return nil, blocked, nil
		}
		content = nextContent
	}
	if content == original {
		return messages, nil, nil
	}
	result := make([]provider.Message, len(messages))
	copy(result, messages)
	result[lastIdx].Content = content
	return result, nil, nil
}

func (o *Orchestrator) executePipeline(ctx context.Context, sessionID string, p *pipeline.Pipeline, slog *sessionlog.Logger) (*RunResult, error) {
	runner := func(ctx context.Context, pluginName, action string, args map[string]string) pipeline.StepRunResult {
		if slog != nil {
			slog.Printf("[pipeline] executing step: %s.%s args=%v", pluginName, action, args)
		}
		call := ToolCall{
			ID:     fmt.Sprintf("pipeline-%s-%s", pluginName, action),
			Plugin: pluginName,
			Action: action,
			Args:   args,
		}
		result := o.executeCall(ctx, call)
		if slog != nil {
			if result.Error != "" {
				slog.Printf("[pipeline] step %s.%s failed: %s", pluginName, action, result.Error)
			} else {
				preview := result.Content
				if len(preview) > 500 {
					preview = preview[:500] + "... [truncated]"
				}
				slog.Printf("[pipeline] step %s.%s succeeded: %s", pluginName, action, preview)
			}
		}
		return pipeline.StepRunResult{Content: result.Content, Error: result.Error}
	}
	executor := pipeline.NewExecutor(runner, p.Config)
	execResult, err := executor.Run(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("pipeline execution: %w", err)
	}

	if slog != nil {
		slog.Printf("[pipeline] execution done: success=%v, steps_executed=%d", execResult.Success, len(execResult.Steps))
	}

	// Record step results in session history
	var toolCalls []ToolCall
	var toolResults []ToolResult
	for _, es := range execResult.Steps {
		tc := ToolCall{
			ID:     fmt.Sprintf("pipeline-%s-%s", es.Plugin, es.Action),
			Plugin: es.Plugin,
			Action: es.Action,
			Args:   es.Args,
		}
		tr := ToolResult{CallID: tc.ID, Content: es.Content, Error: es.Error}
		toolCalls = append(toolCalls, tc)
		toolResults = append(toolResults, tr)
		_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(tc)})
		_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(tr)})
	}
	_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: execResult.Summary})

	return &RunResult{
		Response:  execResult.Summary,
		ToolCalls: toolCalls,
		Results:   toolResults,
	}, nil
}

func (o *Orchestrator) buildMessages(ctx context.Context, sess *state.Session, userMessage string, slog *sessionlog.Logger) []provider.Message {
	messages := make([]provider.Message, 0, len(sess.Messages)+4)

	systemPrompt := o.buildSystemPrompt(ctx, userMessage)
	messages = append(messages, provider.Message{
		Role:    provider.RoleSystem,
		Content: systemPrompt,
	})
	if sess.Summary != "" {
		messages = append(messages, provider.Message{
			Role:    provider.RoleSystem,
			Content: "Previous conversation summary: " + sess.Summary,
		})
	}
	messages = append(messages, sess.Messages...)

	if o.contextWindow > 0 {
		messages = trimToContextWindow(messages, o.contextWindow, slog)
	}

	return messages
}

// estimateTokens returns a rough token count for a string.
// Uses ~4 characters per token which is a reasonable average for most LLMs.
func estimateTokens(s string) int {
	return len(s) / 4
}

// trimToContextWindow drops the oldest conversation messages (preserving
// system messages at the front) until the estimated token count fits within
// the model's context window. Reserves 10% of the window for the response.
func trimToContextWindow(messages []provider.Message, contextWindow int, slog *sessionlog.Logger) []provider.Message {
	maxInputTokens := contextWindow * 9 / 10 // reserve 10% for output

	total := 0
	for _, m := range messages {
		total += estimateTokens(m.Content)
	}
	if total <= maxInputTokens {
		return messages
	}

	// Find where conversation messages start (skip leading system messages).
	convStart := 0
	for convStart < len(messages) && messages[convStart].Role == provider.RoleSystem {
		convStart++
	}

	// Drop oldest conversation messages (pairs of assistant+user typically)
	// until we fit. Always keep at least the last conversation message.
	for total > maxInputTokens && convStart < len(messages)-1 {
		total -= estimateTokens(messages[convStart].Content)
		convStart++
	}

	trimmed := make([]provider.Message, 0, len(messages)-convStart+convStart)
	// Keep system messages.
	for i := 0; i < len(messages); i++ {
		if messages[i].Role == provider.RoleSystem {
			trimmed = append(trimmed, messages[i])
		} else {
			break
		}
	}
	// Append remaining conversation messages.
	trimmed = append(trimmed, messages[convStart:]...)

	if len(trimmed) < len(messages) {
		// Always log trimming (nil slog falls back to global log).
		slog.Printf("context trimming: dropped %d old messages to fit context window (%d tokens, limit %d)",
			len(messages)-len(trimmed), total, maxInputTokens)
	}

	return trimmed
}

func (o *Orchestrator) buildSystemPrompt(ctx context.Context, userMessage string) string {
	var sb strings.Builder
	sb.WriteString("You are an AI assistant with access to the following tools.\n\n")
	sb.WriteString("To call a tool, use EXACTLY this format (no other format will be recognized):\n\n")
	sb.WriteString("[tool_call]\n{\"tool\": \"plugin.action\", \"args\": {\"key\": \"value\"}}\n[/tool_call]\n\n")
	sb.WriteString("Example:\n[tool_call]\n{\"tool\": \"brave-search.search\", \"args\": {\"query\": \"latest news\"}}\n[/tool_call]\n\n")
	sb.WriteString("You may include multiple [tool_call]...[/tool_call] blocks in a single response. Any text outside these blocks is ignored when tool calls are present.\n\n")
	sb.WriteString("When you receive plugin or tool results, reply to the user in a brief natural language answer. Do not simply repeat or echo the tool output; use it to answer the user's question or confirm what was done.\n\n")

	sb.WriteString(o.rules.BuildPromptSection())

	if o.runtimePromptPath != "" {
		if data, err := os.ReadFile(o.runtimePromptPath); err == nil {
			sb.WriteString("\n## Additional instructions (editable from chat)\n")
			sb.WriteString(string(data))
			sb.WriteString("\n\n")
		}
	}

	// Don't list content-preparer or guard actions as tools; they run automatically before LLM calls.
	preparerAction := make(map[string]bool)
	for _, prep := range o.preparers {
		preparerAction[prep.Plugin+"."+prep.Action] = true
	}
	for _, g := range o.guards {
		preparerAction[g.Plugin+"."+g.Action] = true
	}
	caps := o.registry.ListCapabilities()
	for _, cap := range caps {
		fmt.Fprintf(&sb, "## %s\n%s\n", cap.Name, cap.Description)
		for _, action := range cap.Actions {
			if preparerAction[cap.Name+"."+action.Name] || action.UserOnly {
				continue
			}
			fmt.Fprintf(&sb, "- %s.%s: %s\n", cap.Name, action.Name, action.Description)
			for _, p := range action.Parameters {
				req := ""
				if p.Required {
					req = " (required)"
				}
				fmt.Fprintf(&sb, "  - %s: %s%s\n", p.Name, p.Description, req)
			}
		}
		sb.WriteString("\n")
	}

	workflowMemories, _ := o.memory.MemoriesForContext(ctx, "workflow")
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

	actorID := actor.Actor(ctx)
	// permission_plugin gates all plugin actions (including e.g. install_skill); configure it for team deployments.
	if actorID != "" && o.permissionChecker != nil && call.Plugin != o.permissionPluginName {
		allowed, err := o.permissionChecker.Allowed(ctx, actorID, call.Plugin)
		if err != nil {
			log.Printf("Warning: permission check for actor %s plugin %s: %v", actorID, call.Plugin, err)
			return ToolResult{
				CallID: call.ID,
				Error:  "permission denied",
			}
		}
		if !allowed {
			return ToolResult{
				CallID: call.ID,
				Error:  "permission denied",
			}
		}
	}

	// Single lookup: get capability and find the action for context injection, audit logging, and UserOnly enforcement.
	var action *Action
	if cap, ok := o.registry.GetCapability(call.Plugin); ok {
		for i := range cap.Actions {
			if cap.Actions[i].Name == call.Action {
				action = &cap.Actions[i]
				break
			}
		}
	}
	if call.FromLLM && action != nil && action.UserOnly {
		log.Printf("audit: BLOCKED LLM attempt to invoke user_only action: actor %s plugin %s action %s args %v", actorID, call.Plugin, call.Action, call.Args)
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("action %q can only be invoked by the user, not the LLM", call.Action),
		}
	}
	if action != nil {
		// Inject only declared context arg names that have a provider (e.g. session_id). Plugins never receive session content or message history.
		if len(action.InjectContextArgs) > 0 {
			args := make(map[string]string)
			for k, v := range call.Args {
				args[k] = v
			}
			for _, name := range action.InjectContextArgs {
				if provide := o.contextArgProviders[name]; provide != nil {
					if v := provide(ctx, name); v != "" {
						args[name] = v
					}
				}
			}
			call.Args = args
		}
		// Audit log when the action declares AuditLog (no hardcoded plugin/action names).
		if actorID != "" && action.AuditLog {
			log.Printf("audit: actor %s plugin %s action %s args %v", actorID, call.Plugin, call.Action, call.Args)
		}
	}

	result := o.guard.ExecuteWithTimeout(ctx, exec, call)
	result = o.guard.ValidateResult(call, result)
	result = o.guard.Sanitize(result)
	return result
}

func (o *Orchestrator) maybeRecordWorkflow(ctx context.Context, result *RunResult, userMessage string) {
	if len(result.ToolCalls) < 2 {
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "trigger: %s\nsteps:\n", userMessage)
	for i, call := range result.ToolCalls {
		fmt.Fprintf(&sb, "  - plugin: %s, action: %s, order: %d\n", call.Plugin, call.Action, i+1)
	}
	sb.WriteString("outcome: success\n")

	actorID := actor.Actor(ctx)
	_, _ = o.memory.AddScoped(ctx, actorID, sb.String(), "workflow")
}

// maybeSummarizeSession runs summarization when the session has enough messages and config is set.
// It acquires the per-session lock so it cannot race with a concurrent Run() on the same session.
func (o *Orchestrator) maybeSummarizeSession(ctx context.Context, sessionID string) {
	if o.summarizeAfterMessages <= 0 || o.maxMessagesAfterSummary <= 0 {
		return
	}
	sm := o.acquireSessionLock(sessionID)
	defer o.releaseSessionLock(sessionID, sm)

	sess, err := o.sessions.Get(sessionID)
	if err != nil {
		return
	}
	if len(sess.Messages) < o.summarizeAfterMessages {
		return
	}
	keep := o.maxMessagesAfterSummary
	if keep > len(sess.Messages) {
		keep = len(sess.Messages)
	}
	toSummarize := sess.Messages[:len(sess.Messages)-keep]
	keepMessages := sess.Messages[len(sess.Messages)-keep:]

	var sysPrompt, userContent string
	if sess.Summary != "" {
		sysPrompt = o.summarizeUpdatePrompt
		if sysPrompt == "" {
			sysPrompt = defaultSummarizeUpdatePrompt
		}
		var b strings.Builder
		b.WriteString("Previous summary: ")
		b.WriteString(sess.Summary)
		b.WriteString("\n\nNew messages:\n")
		for _, m := range toSummarize {
			b.WriteString(string(m.Role) + ": " + m.Content + "\n")
		}
		userContent = b.String()
	} else {
		sysPrompt = o.summarizePrompt
		if sysPrompt == "" {
			sysPrompt = defaultSummarizePrompt
		}
		var b strings.Builder
		for _, m := range toSummarize {
			b.WriteString(string(m.Role) + ": " + m.Content + "\n")
		}
		userContent = b.String()
	}
	req := &provider.CompletionRequest{
		Model: "",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: sysPrompt},
			{Role: provider.RoleUser, Content: userContent},
		},
	}
	resp, err := o.llm.Complete(ctx, req)
	if err != nil {
		log.Printf("Warning: session summarization: %v", err)
		return
	}
	newSummary := strings.TrimSpace(resp.Content)
	if newSummary == "" {
		return
	}
	if err := o.sessions.SetSummary(sessionID, newSummary, keepMessages); err != nil {
		log.Printf("Warning: set session summary: %v", err)
	}
}

// RunAction executes a single plugin action directly, bypassing the LLM loop.
// Used by the scheduler and other subsystems that need to invoke tools programmatically.
//
// It intentionally skips the semaphore and session lock:
//   - It does not read or write session state (o.sessions), so the per-session lock
//     is not needed and would only cause unnecessary contention.
//   - Scheduler/system calls are not user sessions and should not compete for the
//     user-facing concurrency cap enforced by the semaphore.
//
// Plugin executors must be safe for concurrent use — the same requirement that applies
// when multiple sessions call the same plugin in parallel via Run.
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
	fmt.Fprintf(&sb, "[tool_call] %s.%s", call.Plugin, call.Action)
	if len(call.Args) > 0 {
		sb.WriteString("(")
		first := true
		for k, v := range call.Args {
			if !first {
				sb.WriteString(", ")
			}
			fmt.Fprintf(&sb, "%s=%s", k, v)
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

// plannerLLMAdapter adapts orchestrator.LLMClient to pipeline.LLMClient.
type plannerLLMAdapter struct {
	llm LLMClient
}

func (a *plannerLLMAdapter) Complete(ctx context.Context, req *pipeline.CompletionRequest) (*pipeline.CompletionResponse, error) {
	msgs := make([]provider.Message, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = provider.Message{Role: provider.Role(m.Role), Content: m.Content}
	}
	resp, err := a.llm.Complete(ctx, &provider.CompletionRequest{Messages: msgs})
	if err != nil {
		return nil, err
	}
	return &pipeline.CompletionResponse{Content: resp.Content}, nil
}

// capabilitiesToPlannerInfo converts orchestrator PluginCapability to pipeline CapabilityInfo.
func capabilitiesToPlannerInfo(caps []PluginCapability) []pipeline.CapabilityInfo {
	result := make([]pipeline.CapabilityInfo, len(caps))
	for i, cap := range caps {
		var filteredActions []Action
		for _, a := range cap.Actions {
			if !a.UserOnly {
				filteredActions = append(filteredActions, a)
			}
		}
		actions := make([]pipeline.ActionInfo, len(filteredActions))
		for j, a := range filteredActions {
			params := make([]pipeline.ParamInfo, len(a.Parameters))
			for k, p := range a.Parameters {
				params[k] = pipeline.ParamInfo{Name: p.Name, Description: p.Description, Required: p.Required}
			}
			actions[j] = pipeline.ActionInfo{Name: a.Name, Description: a.Description, Parameters: params}
		}
		result[i] = pipeline.CapabilityInfo{Name: cap.Name, Description: cap.Description, Actions: actions}
	}
	return result
}

// permissionCheckerImpl invokes the permission plugin with action "check" and args actor, plugin.
type permissionCheckerImpl struct {
	registry   *ToolRegistry
	guard      *Guard
	pluginName string
}

// NewPermissionChecker returns a PermissionChecker that calls the given plugin with action PermissionAction.
func NewPermissionChecker(registry *ToolRegistry, guard *Guard, pluginName string) PermissionChecker {
	if pluginName == "" {
		return nil
	}
	return &permissionCheckerImpl{registry: registry, guard: guard, pluginName: pluginName}
}

func (p *permissionCheckerImpl) Allowed(ctx context.Context, actorID, plugin string) (bool, error) {
	if !p.registry.HasAction(p.pluginName, PermissionAction) {
		return false, nil // deny if permission plugin doesn't expose the action
	}
	exec, ok := p.registry.GetExecutor(p.pluginName)
	if !ok {
		return false, nil
	}
	call := ToolCall{
		ID:     fmt.Sprintf("permission-check-%s-%s", actorID, plugin),
		Plugin: p.pluginName,
		Action: PermissionAction,
		Args:   map[string]string{"actor": actorID, "plugin": plugin},
	}
	result := p.guard.ExecuteWithTimeout(ctx, exec, call)
	if result.Error != "" {
		return false, nil // deny on error
	}
	return parsePermissionResult(result.Content), nil
}

// parsePermissionResult interprets permission plugin output: "true" or JSON {"allowed": true} -> true.
func parsePermissionResult(content string) bool {
	content = strings.TrimSpace(content)
	if strings.EqualFold(content, "true") {
		return true
	}
	var v struct {
		Allowed bool `json:"allowed"`
	}
	if err := json.Unmarshal([]byte(content), &v); err == nil && v.Allowed {
		return true
	}
	return false
}
