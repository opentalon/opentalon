package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/lua"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

const maxAgentLoopIterations = 20

// PermissionAction is the fixed action name the core uses when calling the permission plugin.
const PermissionAction = "check"

// ContentPreparerEntry configures a plugin action to run before the first LLM call.
// If Guard is true, the plugin also runs before every LLM call in the agent loop to sanitize messages and prevent prompt injection.
// If STT is true, the preparer handles audio transcription: audio/* files are passed as base64 args (file_data, file_mime) and the response is used as transcript text.
type ContentPreparerEntry struct {
	Plugin   string
	Action   string
	ArgKey   string // optional, default "text"
	Insecure bool   // if true (default), this preparer cannot run invoke steps; if false (trusted), can invoke
	Guard    bool   // if true, also runs before every LLM call to sanitize messages
	FailOpen bool   // if true, guard/preparer failures are logged and skipped; default false (fail-closed)
	STT      bool   // if true, receives audio/* files as base64 args and returns transcript text
}

// ResponseFormatterEntry configures a plugin or Lua script that transforms the
// LLM response text after the agent loop, before returning to the channel handler.
type ResponseFormatterEntry struct {
	Plugin   string // "my-plugin" for gRPC or "lua:my-script" for Lua
	Action   string // gRPC action name; ignored for Lua scripts
	FailOpen bool   // default true: log and skip on failure (don't block responses)
}

type LLMClient interface {
	Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error)
}

// StreamingLLMClient is an optional extension of LLMClient that supports
// streaming responses. When the orchestrator's LLM implements this interface
// and an OnStreamChunk callback is provided, the final-answer LLM round
// streams tokens to the caller in real-time.
type StreamingLLMClient interface {
	LLMClient
	Stream(ctx context.Context, req *provider.CompletionRequest) (provider.ResponseStream, error)
}

// StreamChunkCallback is called for each text chunk during a streaming LLM
// response. The ctx carries the same trace/actor information as the Run call.
// done is true on the last invocation (content may be empty).
type StreamChunkCallback func(ctx context.Context, content string, done bool)

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

// GroupPluginLookup returns the plugin IDs allowed for a group.
// It is called per-request when a profile is present to filter the tool list.
// An empty slice means the group has no assignments (all restricted plugins are hidden).
type GroupPluginLookup interface {
	PluginsForGroup(ctx context.Context, groupID string) ([]string, error)
}

// UsageRecorder records LLM usage statistics after an orchestrator run.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, entityID, groupID, channelID, sessionID, modelID string, inputTokens, outputTokens, toolCalls int)
}

// PromptSnapshotUpserter persists content-addressed prompt bodies so a
// consumer reading a turn_start event can resolve its system_prompt_sha256,
// server_instructions[].sha256, and available_tools[].desc_sha256 back to
// the original text. SessionEventStore satisfies this interface in
// production; tests may inject a fake. Implementations must be safe to
// call concurrently and idempotent on conflicting sha256 (first insert
// wins; later identical writes are no-ops).
type PromptSnapshotUpserter interface {
	UpsertPromptSnapshot(ctx context.Context, sha256, kind, content string) error
}

// PluginCallObserver is notified after each plugin/tool call executed by the orchestrator.
type PluginCallObserver interface {
	ObservePluginCall(plugin, action string, failed bool, inputTokens, outputTokens int)
}

// KnowledgeConfig configures the knowledge directory scanning feature.
type KnowledgeConfig struct {
	Plugin string // plugin name to call for ingestion (e.g. "weaviate")
	Action string // action name for single-article ingestion (e.g. "ingest")
	Dir    string // directory to scan for .md files
}

// OrchestratorOpts holds optional configuration for NewWithRules. Zero values mean defaults (no permission check, no summarization).
type OrchestratorOpts struct {
	CustomRules             []string
	ContentPreparers        []ContentPreparerEntry
	ResponseFormatters      []ResponseFormatterEntry
	LuaScriptPaths          map[string]string
	PermissionChecker       PermissionChecker
	PermissionPluginName    string
	RuntimePromptPath       string                        // optional path to editable prompt file (e.g. data_dir/custom_prompt.txt); appended to system prompt
	ContextArgProviders     map[string]ContextArgProvider // optional; if nil, default providers (e.g. session_id) are used
	ContextMessages         int                           // send only last N messages to LLM (0 = all)
	SummarizeAfterMessages  int                           // 0 = off
	MaxMessagesAfterSummary int                           // keep this many messages after summarization
	SummarizePrompt         string                        // empty = default English
	SummarizeUpdatePrompt   string                        // empty = default English
	PipelineEnabled         bool                          // when true, create Planner from llm
	PlanTimeout             time.Duration                 // max time for planner LLM call; 0 = default 15s
	PipelineConfig          pipeline.PipelineConfig
	ConfirmationPlugin      string                 // optional; plugin name for confirmation strategy (e.g. "planner")
	ConfirmationAction      string                 // optional; action name (e.g. "check_confirmation")
	ContextWindow           int                    // model context window in tokens; 0 = no trimming
	MaxConcurrentSessions   int                    // max sessions running in parallel; default 1 (sequential)
	GroupPluginLookup       GroupPluginLookup      // optional; when set, filters tool list by profile group
	UsageRecorder           UsageRecorder          // optional; when set, records LLM usage after each run
	PluginCallObserver      PluginCallObserver     // optional; when set, notified after each plugin/tool call
	EventSink               emit.Sink              // optional; nil defaults to emit.NoOpSink (helpers run unconditionally, the no-op sink discards them)
	PromptSnapshotStore     PromptSnapshotUpserter // optional; when set, system prompt + server instructions + tool descriptions are persisted by sha256 so turn_start hashes resolve to content
	SyncActionsPlugin       string                 // optional; plugin name for action sync (e.g. "weaviate")
	SyncActionsAction       string                 // optional; action name for sync (e.g. "sync_actions"); requires SyncActionsPlugin
	SyncGlossaryAction      string                 // optional; action name for glossary sync (e.g. "sync_glossary"); uses SyncActionsPlugin
	Knowledge               KnowledgeConfig        // optional; knowledge directory ingestion
	Subprocess              SubprocessConfig       // optional; subprocess (sub-agent) support
	OnStreamChunk           StreamChunkCallback    // optional; when set and LLM supports streaming, final answers are streamed
	ShowToolCalls           string                 // "raw" = debug blocks, "friendly" = short labels, "" = hidden
}

// MemoryStoreInterface is the scoped memory store used for general + per-actor memories.
type MemoryStoreInterface interface {
	AddScoped(ctx context.Context, actorID string, content string, tags ...string) (*state.Memory, error)
	MemoriesForContext(ctx context.Context, tag string) ([]*state.Memory, error)
}

// SessionStoreInterface is the session store (in-memory or SQLite).
type SessionStoreInterface interface {
	Get(id string) (*state.Session, error)
	Create(id, entityID, groupID string) *state.Session
	AddMessage(id string, msg provider.Message) error
	SetModel(id string, model provider.ModelRef) error
	SetSummary(id string, summary string, messages []provider.Message) error // for summarization; optional, may be no-op
	SetMetadata(id, key, value string) error                                 // upsert a single metadata key; empty value removes it
	// ClearMessages drops Messages and Summary; preserves EntityID, GroupID,
	// ActiveModel, Metadata, and CreatedAt; bumps UpdatedAt. The audit log
	// (session_events) is untouched. Missing id is a no-op — destructive
	// operations on this interface are idempotent. Used by the clear_session
	// command to reset LLM context without losing the session's identity.
	ClearMessages(id string) error
	Delete(id string) error // remove session entirely (admin / retention; missing id no-op)
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
	formatters              []ResponseFormatterEntry      // run after final response; text-in/text-out
	guards                  []ContentPreparerEntry        // subset of preparers with Guard:true; run before every LLM call
	luaScriptPaths          map[string]string             // optional; plugin name -> path to .lua script (for "lua:name" preparers)
	permissionChecker       PermissionChecker             // optional; when set, executeCall checks permission before running
	permissionPluginName    string                        // name of the permission plugin (skip permission check when executing it)
	runtimePromptPath       string                        // optional; if set, buildSystemPrompt appends file contents
	contextArgProviders     map[string]ContextArgProvider // name -> extract from context; used to inject args per action
	contextMessages         int                           // send only last N messages to LLM (0 = all)
	summarizeAfterMessages  int                           // 0 = off; after this many messages run summarization
	maxMessagesAfterSummary int                           // keep this many messages after summarization
	summarizePrompt         string                        // system prompt for initial summarization (config; empty = default English)
	summarizeUpdatePrompt   string                        // system prompt for updating summary (config; empty = default English)
	planner                 *pipeline.Planner             // nil = pipeline disabled
	pendingMu               sync.Mutex                    // guards pendingPipelines, pendingToolCalls, pendingConfirmationIDs
	pendingPipelines        map[string]*pipeline.Pipeline // sessionID -> pending pipeline
	pendingToolCalls        map[string]*ToolCall          // sessionID -> pending tool call awaiting confirmation
	pendingConfirmationIDs  map[string]string             // sessionID -> session-event id of the confirmation_requested event; stamped as parent on the matching confirmation_resolved emit
	pipelineConfig          pipeline.PipelineConfig
	confirmationPlugin      string                 // optional; plugin for confirmation strategy
	confirmationAction      string                 // optional; action name for confirmation check
	contextWindow           int                    // model context window in tokens; 0 = no trimming
	groupPluginLookup       GroupPluginLookup      // optional; nil = no group-based filtering
	usageRecorder           UsageRecorder          // optional; nil = no usage tracking
	pluginCallObserver      PluginCallObserver     // optional; nil = no plugin call observation
	eventSink               emit.Sink              // structured session event sink; always non-nil (NoOpSink default)
	snapshotStore           PromptSnapshotUpserter // optional; nil = turn_start hashes are emitted but content is not persisted
	syncActionsPlugin       string                 // optional; plugin name for action sync
	syncActionsAction       string                 // optional; action name for action sync
	syncGlossaryAction      string                 // optional; action name for glossary sync (uses syncActionsPlugin)
	knowledge               KnowledgeConfig        // optional; knowledge directory ingestion
	subprocessConfig        SubprocessConfig       // optional; subprocess (sub-agent) support
	onStreamChunk           StreamChunkCallback    // optional; when set, final answers stream to caller
	showToolCalls           string                 // "raw" = debug blocks, "friendly" = short labels, "" = hidden
}

// resolveAllowedPluginNames returns a JSON array of allowed plugin names for the
// current profile, or "" when unrestricted. Used to inject allowed_plugins into
// preparer and LLM-called actions so RAG plugins can filter by profile.
func resolveAllowedPluginNames(ctx context.Context, o *Orchestrator) string {
	allowed := o.resolveAllowedPlugins(ctx)
	if allowed.m == nil {
		return ""
	}
	names := mapKeys(allowed.m)
	sort.Strings(names)
	b, _ := json.Marshal(names)
	return string(b)
}

// defaultContextArgProviders returns built-in providers only for opaque identifiers (e.g. session_id).
// No session messages, conversation text, or other sensitive content is exposed to plugins via this mechanism.
func defaultContextArgProviders(o *Orchestrator, custom map[string]ContextArgProvider) map[string]ContextArgProvider {
	builtin := map[string]ContextArgProvider{
		"session_id": func(ctx context.Context, _ string) string { return actor.SessionID(ctx) },
		"allowed_plugins": func(ctx context.Context, _ string) string {
			return resolveAllowedPluginNames(ctx, o)
		},
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
		opts.SummarizePrompt = prompts.SummarizeDefault
	}
	if opts.SummarizeUpdatePrompt == "" {
		opts.SummarizeUpdatePrompt = prompts.SummarizeUpdate
	}
	var planner *pipeline.Planner
	if opts.PipelineEnabled {
		planner = pipeline.NewPlanner(&plannerLLMAdapter{llm: llm}, opts.PlanTimeout)
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

	eventSink := opts.EventSink
	if eventSink == nil {
		eventSink = emit.NoOpSink{}
	}

	o := &Orchestrator{
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
		formatters:              opts.ResponseFormatters,
		guards:                  guards,
		luaScriptPaths:          opts.LuaScriptPaths,
		permissionChecker:       opts.PermissionChecker,
		permissionPluginName:    opts.PermissionPluginName,
		runtimePromptPath:       opts.RuntimePromptPath,
		contextMessages:         opts.ContextMessages,
		summarizeAfterMessages:  opts.SummarizeAfterMessages,
		maxMessagesAfterSummary: opts.MaxMessagesAfterSummary,
		summarizePrompt:         opts.SummarizePrompt,
		summarizeUpdatePrompt:   opts.SummarizeUpdatePrompt,
		planner:                 planner,
		pendingPipelines:        make(map[string]*pipeline.Pipeline),
		pendingToolCalls:        make(map[string]*ToolCall),
		pendingConfirmationIDs:  make(map[string]string),
		pipelineConfig:          pipelineCfg,
		confirmationPlugin:      opts.ConfirmationPlugin,
		confirmationAction:      opts.ConfirmationAction,
		contextWindow:           opts.ContextWindow,
		groupPluginLookup:       opts.GroupPluginLookup,
		usageRecorder:           opts.UsageRecorder,
		pluginCallObserver:      opts.PluginCallObserver,
		eventSink:               eventSink,
		snapshotStore:           opts.PromptSnapshotStore,
		syncActionsPlugin:       opts.SyncActionsPlugin,
		syncActionsAction:       opts.SyncActionsAction,
		syncGlossaryAction:      opts.SyncGlossaryAction,
		knowledge:               opts.Knowledge,
		onStreamChunk:           opts.OnStreamChunk,
		showToolCalls:           opts.ShowToolCalls,
	}
	// Context arg providers need access to 'o' for allowed_plugins resolution.
	o.contextArgProviders = defaultContextArgProviders(o, opts.ContextArgProviders)

	// Register the built-in _subprocess plugin when enabled.
	o.subprocessConfig = opts.Subprocess
	if opts.Subprocess.Enabled {
		subCap := PluginCapability{
			Name:        "_subprocess",
			Description: "Fork a focused sub-agent process to handle a sub-task",
			Actions: []Action{{
				Name:        "run",
				Description: "Fork a subprocess that runs its own agent loop. Use for sub-tasks like research, multi-step tool usage, or focused questions that benefit from independent reasoning.",
				Parameters: []Parameter{
					{Name: "task", Description: "Clear description of what the subprocess should accomplish", Required: true},
					{Name: "tools", Description: "Comma-separated plugin.action allowlist (empty = all available tools)", Required: false},
					{Name: "max_iterations", Description: "Max agent loop iterations 1-10 (default 5)", Required: false},
				},
			}},
		}
		_ = o.registry.Register(subCap, &subprocessExecutor{orch: o})
	}

	return o
}

type RunResult struct {
	Response        string // LLM answer
	InputForDisplay string // optional: what we sent to the LLM (e.g. tool results), for channels that want to show it
	ToolCalls       []ToolCall
	Results         []ToolResult
	Metadata        map[string]string // optional key-value pairs passed to the channel response (e.g. type=system for commands)
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
	SendToLLM     *bool                `json:"send_to_llm"`
	Message       string               `json:"message"`
	Invoke        invokeStepsUnmarshal `json:"invoke"`
	RelevantTools []string             `json:"relevant_tools,omitempty"` // optional: filter system prompt to these tools (format: "plugin.action")
}

func (o *Orchestrator) handlePreparerFailure(prep ContentPreparerEntry, details string) *RunResult {
	name := prep.Plugin + "." + prep.Action
	if strings.HasPrefix(prep.Plugin, "lua:") {
		name = prep.Plugin
	}
	slog.Warn("guard failed", "guard", name, "details", details)
	if prep.FailOpen {
		return nil
	}
	return &RunResult{
		Response: fmt.Sprintf("Request blocked: guard %s failed.", name),
		Metadata: map[string]string{
			"type":       "error",
			"error_code": "guard_blocked",
		},
	}
}

// runSTTPreparers transcribes audio/* files using STT-flagged preparers.
// Each audio file is passed to every STT preparer as base64 args; the returned transcript
// is prepended to content and the audio file is removed from the slice.
// Non-audio files and non-STT preparers are unaffected.
// On error with FailOpen=true the audio file is passed through; with FailOpen=false the
// original content and files are returned unchanged.
func (o *Orchestrator) runSTTPreparers(ctx context.Context, content string, files []provider.MessageFile) (string, []provider.MessageFile) {
	hasSTT := false
	for _, p := range o.preparers {
		if p.STT {
			hasSTT = true
			break
		}
	}

	var audioFiles, remaining []provider.MessageFile
	for _, f := range files {
		if strings.HasPrefix(f.MimeType, "audio/") {
			audioFiles = append(audioFiles, f)
		} else {
			remaining = append(remaining, f)
		}
	}
	if !hasSTT || len(audioFiles) == 0 {
		return content, files
	}

	for _, af := range audioFiles {
		transcribed := false
		for _, prep := range o.preparers {
			if !prep.STT {
				continue
			}
			transcript, err := o.runSTTPreparer(ctx, prep, af)
			if err != nil {
				slog.WarnContext(ctx, "stt transcription failed", "plugin", prep.Plugin, "action", prep.Action, "error", err)
				if !prep.FailOpen {
					return content, files // fail-closed: abort and return original
				}
				continue // try next STT preparer
			}
			if content == "" {
				content = transcript
			} else {
				content = transcript + "\n\n" + content
			}
			transcribed = true
			break // file handled, don't try more preparers
		}
		if !transcribed {
			remaining = append(remaining, af) // no preparer succeeded, pass through
		}
	}
	return content, remaining
}

// maxSTTFileSize is the maximum audio file size accepted for STT transcription.
// Larger files are rejected to avoid doubling peak memory during base64 encoding.
const maxSTTFileSize = 25 << 20 // 25 MB

// runSTTPreparer calls a single STT preparer plugin with a base64-encoded audio file.
// It always returns an error on plugin failure so that runSTTPreparers can apply FailOpen logic.
func (o *Orchestrator) runSTTPreparer(ctx context.Context, prep ContentPreparerEntry, f provider.MessageFile) (string, error) {
	if len(f.Data) > maxSTTFileSize {
		return "", fmt.Errorf("audio file too large for STT (%d bytes, max %d)", len(f.Data), maxSTTFileSize)
	}
	if !o.registry.HasAction(prep.Plugin, prep.Action) {
		return "", fmt.Errorf("stt plugin %q action %q not found", prep.Plugin, prep.Action)
	}
	call := ToolCall{
		ID:     fmt.Sprintf("stt-%s-%s", prep.Plugin, prep.Action),
		Plugin: prep.Plugin,
		Action: prep.Action,
		Args: map[string]string{
			"file_data": base64.StdEncoding.EncodeToString(f.Data),
			"file_mime": f.MimeType,
		},
	}
	result := o.executeCall(ctx, call)
	if result.Error != "" {
		return "", fmt.Errorf("stt plugin %q: %s", prep.Plugin, result.Error)
	}
	return result.Content, nil
}

// runSinglePreparer executes one content preparer. Returns (new content, blocked result, relevant tools, error).
// relevantTools is non-nil only when the preparer explicitly returns a relevant_tools list.
// runSinglePreparerWithSearch calls runSinglePreparer and injects a
// `search_text` arg when the enriched search query differs from the content.
// The preparer can use search_text for RAG lookup while returning the
// original content for the LLM.
func (o *Orchestrator) runSinglePreparerWithSearch(ctx context.Context, prep ContentPreparerEntry, content, searchText, callPrefix string, allowInvoke bool) (string, *RunResult, []string, error) {
	ctx = withSearchQuery(ctx, searchText)
	return o.runSinglePreparer(ctx, prep, content, callPrefix, allowInvoke)
}

func (o *Orchestrator) runSinglePreparer(ctx context.Context, prep ContentPreparerEntry, content, callPrefix string, allowInvoke bool) (string, *RunResult, []string, error) {
	if strings.HasPrefix(prep.Plugin, "lua:") {
		scriptName := strings.TrimPrefix(prep.Plugin, "lua:")
		scriptPath := o.luaScriptPaths[scriptName]
		if scriptPath == "" {
			if blocked := o.handlePreparerFailure(prep, "Lua script path not found"); blocked != nil {
				return content, blocked, nil, nil
			}
			return content, nil, nil, nil
		}
		result, err := lua.RunPrepare(scriptPath, content)
		if err != nil {
			if blocked := o.handlePreparerFailure(prep, err.Error()); blocked != nil {
				return content, blocked, nil, nil
			}
			return content, nil, nil, nil
		}
		if !result.SendToLLM {
			if allowInvoke && len(result.InvokeSteps) > 0 {
				steps := make([]InvokeStep, len(result.InvokeSteps))
				for i, s := range result.InvokeSteps {
					steps[i] = InvokeStep{Plugin: s.Plugin, Action: s.Action, Args: s.Args}
				}
				invokeResult, err := o.runInvokeSteps(ctx, steps)
				return "", invokeResult, nil, err
			}
			msg := result.Content
			if msg == "" {
				msg = "Request blocked by guard."
			}
			return "", &RunResult{Response: msg}, nil, nil
		}
		return result.Content, nil, nil, nil
	}

	argKey := prep.ArgKey
	if argKey == "" {
		argKey = "text"
	}
	if !o.registry.HasAction(prep.Plugin, prep.Action) {
		if blocked := o.handlePreparerFailure(prep, "action not found"); blocked != nil {
			return content, blocked, nil, nil
		}
		return content, nil, nil, nil
	}
	call := ToolCall{
		ID:     fmt.Sprintf("%s-%s-%s", callPrefix, prep.Plugin, prep.Action),
		Plugin: prep.Plugin,
		Action: prep.Action,
		Args:   map[string]string{argKey: content},
	}
	// Inject allowed_plugins for preparers so RAG plugins can filter by profile.
	if allowed := resolveAllowedPluginNames(ctx, o); allowed != "" {
		call.Args["allowed_plugins"] = allowed
	}
	// Inject enriched search query so RAG preparers can use it for semantic
	// search while the main text arg stays as the original user message.
	if sq := SearchQueryFromContext(ctx); sq != "" && sq != content {
		call.Args["search_text"] = sq
	}
	toolResult := o.executeCall(ctx, call)
	if toolResult.Error != "" {
		if blocked := o.handlePreparerFailure(prep, toolResult.Error); blocked != nil {
			return content, blocked, nil, nil
		}
		return content, nil, nil, nil
	}
	var pr preparerResponse
	if err := json.Unmarshal([]byte(toolResult.Content), &pr); err == nil && pr.SendToLLM != nil && !*pr.SendToLLM {
		if allowInvoke && len(pr.Invoke) > 0 {
			if prep.Insecure {
				slog.Warn("insecure preparer cannot run invoke, ignoring", "plugin", prep.Plugin, "action", prep.Action)
				return content, nil, nil, nil
			}
			invokeResult, err := o.runInvokeSteps(ctx, pr.Invoke)
			return "", invokeResult, nil, err
		}
		msg := pr.Message
		if msg == "" {
			msg = toolResult.Content
		}
		if msg == "" {
			msg = "Request blocked by guard."
		}
		return "", &RunResult{Response: msg}, nil, nil
	}
	if pr.Message != "" {
		return pr.Message, nil, pr.RelevantTools, nil
	}
	return toolResult.Content, nil, pr.RelevantTools, nil
}

// applyShowToolCalls prepends tool call details to result.Response based on show_tool_calls mode.
// Called before formatResponse so Lua formatters can restyle the combined output.
//
//   - "raw":      full debug blocks ([tool_call] …, [tool_result] …) + separator
//   - "friendly": short "_plugin → action…_" labels per tool call
//   - "" / other: nothing prepended
func (o *Orchestrator) applyShowToolCalls(result *RunResult) {
	if result == nil || len(result.ToolCalls) == 0 {
		return
	}
	switch o.showToolCalls {
	case "raw", "true": // "true" for backward compat with bool config
		if result.InputForDisplay == "" {
			return
		}
		result.Response = result.InputForDisplay + "\n\n---\n\n" + result.Response
	case "friendly":
		var labels []string
		for _, call := range result.ToolCalls {
			if call.Plugin != "" {
				labels = append(labels, fmt.Sprintf("_%s → %s…_", call.Plugin, call.Action))
			}
		}
		if len(labels) > 0 {
			result.Response = strings.Join(labels, "\n") + "\n\n" + result.Response
		}
	}
}

// formatResponse runs all configured response formatters on result.Response.
// Each formatter receives the response text and the channel's response_format.
// Formatters are text-in/text-out (no invoke, no blocking). On error the
// formatter is skipped (FailOpen true) or the error is returned (FailOpen false).
func (o *Orchestrator) formatResponse(ctx context.Context, result *RunResult) error {
	if len(o.formatters) == 0 || result == nil || result.Response == "" {
		return nil
	}
	responseFormat := string(pkgchannel.CapabilitiesFromContext(ctx).ResponseFormat)

	for _, f := range o.formatters {
		var formatted string
		var err error

		if strings.HasPrefix(f.Plugin, "lua:") {
			scriptName := strings.TrimPrefix(f.Plugin, "lua:")
			scriptPath := o.luaScriptPaths[scriptName]
			if scriptPath == "" {
				err = fmt.Errorf("lua script path not found for %q", scriptName)
			} else {
				formatted, err = lua.RunFormat(scriptPath, result.Response, responseFormat)
			}
		} else {
			if !o.registry.HasAction(f.Plugin, f.Action) {
				err = fmt.Errorf("action %s.%s not found", f.Plugin, f.Action)
			} else {
				call := ToolCall{
					ID:     fmt.Sprintf("formatter-%s-%s", f.Plugin, f.Action),
					Plugin: f.Plugin,
					Action: f.Action,
					Args: map[string]string{
						"text":            result.Response,
						"response_format": responseFormat,
					},
				}
				toolResult := o.executeCall(ctx, call)
				if toolResult.Error != "" {
					err = fmt.Errorf("%s", toolResult.Error)
				} else {
					formatted = toolResult.Content
				}
			}
		}

		if err != nil {
			name := f.Plugin
			if f.Action != "" {
				name += "." + f.Action
			}
			if f.FailOpen {
				slog.Warn("response formatter failed, skipping", "formatter", name, "error", err)
				continue
			}
			return fmt.Errorf("response formatter %s: %w", name, err)
		}
		// Guard: ignore empty results so a broken formatter can't blank the
		// response. A formatter that intentionally returns "" is treated as a
		// no-op, preserving the previous response text.
		if formatted != "" {
			result.Response = formatted
		}
	}
	return nil
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

func (o *Orchestrator) Run(ctx context.Context, sessionID, userMessage string, files ...provider.MessageFile) (*RunResult, error) {
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

	// Request-scoped session cache: avoids redundant DB roundtrips for
	// sessions.Get() on every agent loop iteration. The per-session lock
	// above guarantees single-writer access.
	sessions := newCachedSessionStore(o.sessions)

	sess, err := sessions.Get(sessionID)
	if err != nil {
		return nil, fmt.Errorf("session lookup: %w", err)
	}
	ctx = actor.WithSessionID(ctx, sessionID)

	// Set up trace_id for this session so all logs are correlated.
	traceID := logger.TraceIDFromSessionKey(sessionID)
	ctx = logger.WithTraceID(ctx, traceID)

	// Emit user_message exactly as the orchestrator received it, before any
	// preparer mutates it. Fires on every Run — including paths that exit
	// without reaching the agent loop (pending pipeline / tool-call
	// confirmation), because the user did send a message regardless of how
	// the orchestrator routes it.
	emit.EmitUserMessage(ctx, o.eventSink, userMessage)

	// Per-session deep debug: enabled by the set_debug_mode command, which
	// stores debug=true in session metadata. With the flag set, the slog
	// session-debug handler promotes all Debug records on this ctx to Info
	// and the OpenAI provider tees raw HTTP bodies into the persistent
	// ai_debug_events table. Sessions without the flag are unaffected.
	if sess != nil && sess.Metadata["debug"] == "true" {
		ctx = logger.WithSessionDebug(ctx)
	}

	log := logger.FromContext(ctx)

	// Phase timing: when debug is active, track wall-clock duration of each
	// major phase so operators can see where time is spent.
	var timing *runTiming
	if logger.IsSessionDebug(ctx) {
		timing = newRunTiming()
		defer timing.log(log)
	}

	// Snapshot session state before this Run so we can rollback on rejection.
	var msgCountAtStart int
	if sess != nil {
		msgCountAtStart = len(sess.Messages)
	}

	// Block A: Check for pending pipeline confirmation.
	if timing != nil {
		timing.begin("confirmation_check")
	}
	o.pendingMu.Lock()
	pendingPipeline := o.pendingPipelines[sessionID]
	pendingPipelineConfID := o.pendingConfirmationIDs[sessionID]
	if pendingPipeline != nil {
		delete(o.pendingPipelines, sessionID)
		delete(o.pendingConfirmationIDs, sessionID)
	}
	o.pendingMu.Unlock()
	if p := pendingPipeline; p != nil {
		// Parent the resolved event onto the original confirmation_requested
		// so analytics can pair the two across turns. Empty id (no sink at
		// request time, or pre-instrumentation pending state) leaves the
		// turn_start parent in place.
		resolvedCtx := ctx
		if pendingPipelineConfID != "" {
			resolvedCtx = emit.WithParent(ctx, pendingPipelineConfID)
		}
		var decision pipeline.ConfirmationDecision
		if o.planner != nil {
			d, classErr := o.planner.ClassifyConfirmation(ctx, userMessage)
			if classErr != nil {
				log.Warn("confirmation classification failed, falling back to keyword match", "error", classErr)
				decision = pipeline.ParseConfirmation(userMessage)
			} else {
				decision = d
			}
		} else {
			decision = pipeline.ParseConfirmation(userMessage)
		}
		log.Debug("pipeline pending", "pipeline_id", p.ID, "session", sessionID, "input", userMessage, "decision", decision)
		_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: userMessage})
		if decision == pipeline.Approved {
			emit.EmitConfirmationResolved(resolvedCtx, o.eventSink, emit.ConfirmationResolvedArgs{Choice: "approve"})
			log.Debug("pipeline executing", "pipeline_id", p.ID, "steps", len(p.Steps))
			res, err := o.executePipeline(ctx, sessionID, p)
			if err == nil && res != nil {
				o.applyShowToolCalls(res)
				if fmtErr := o.formatResponse(ctx, res); fmtErr != nil {
					return nil, fmtErr
				}
			}
			return res, err
		}
		emit.EmitConfirmationResolved(resolvedCtx, o.eventSink, emit.ConfirmationResolvedArgs{Choice: "reject"})
		resp := "Pipeline cancelled."
		_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: resp})
		log.Debug("pipeline rejected", "pipeline_id", p.ID, "input", userMessage)
		return &RunResult{
			Response: resp,
			Metadata: map[string]string{
				"type":   "system",
				"action": "pipeline_cancelled",
			},
		}, nil
	}

	// Block A2: Check for pending tool call confirmation.
	// First check the in-memory map (fast path), then fall back to session
	// metadata (survives restarts and multi-instance deployments).
	o.pendingMu.Lock()
	pendingCall := o.pendingToolCalls[sessionID]
	pendingCallConfID := o.pendingConfirmationIDs[sessionID]
	if pendingCall != nil {
		delete(o.pendingToolCalls, sessionID)
		delete(o.pendingConfirmationIDs, sessionID)
	}
	o.pendingMu.Unlock()
	if pendingCall == nil {
		// Fallback: in-memory state lost (pod restart, failover). Recover
		// the call AND the original confirmation_requested event id from
		// session metadata so the resolved event still links to the
		// request that started the pending state.
		pendingCall, pendingCallConfID = loadPendingToolCall(sessions, sessionID)
	}
	if pendingCall != nil {
		_ = sessions.SetMetadata(sessionID, "pending_tool_call", "")
	}
	toolCallConfirmed := false
	if tc := pendingCall; tc != nil {
		// Parent the resolved event onto the matching confirmation_requested
		// (potentially from a prior turn or even a prior pod). Empty id is
		// the legacy/pre-instrumentation path — leave the turn_start parent.
		resolvedCtx := ctx
		if pendingCallConfID != "" {
			resolvedCtx = emit.WithParent(ctx, pendingCallConfID)
		}
		var decision pipeline.ConfirmationDecision
		// Prefer explicit frontend signal (metadata["confirmation"]) over
		// LLM-based or text-based classification — faster and deterministic.
		switch explicit := actor.ConfirmationDecision(ctx); explicit {
		case "approve":
			decision = pipeline.Approved
		case "reject":
			decision = pipeline.Rejected
		default:
			if o.planner != nil {
				d, classErr := o.planner.ClassifyConfirmation(ctx, userMessage)
				if classErr != nil {
					decision = pipeline.ParseConfirmation(userMessage)
				} else {
					decision = d
				}
			} else {
				decision = pipeline.ParseConfirmation(userMessage)
			}
		}
		if decision == pipeline.Approved {
			// Only record the user's approval in session — rejections are
			// kept out so the LLM doesn't avoid re-calling the tool later.
			_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: userMessage})
			emit.EmitConfirmationResolved(resolvedCtx, o.eventSink, emit.ConfirmationResolvedArgs{
				Choice:     "approve",
				ToolCallID: tc.ID,
			})
			log.Debug("tool call confirmed, executing", "plugin", tc.Plugin, "action", tc.Action)
			result := o.executeCall(ctx, *tc)
			// Record in session history.
			tr := ToolResult{CallID: tc.ID, Content: result.Content, StructuredContent: result.StructuredContent, Error: result.Error}
			if o.supportsNativeTools() {
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role: provider.RoleAssistant,
					ToolCalls: []provider.ToolCall{{
						ID: tc.ID, Name: tc.Plugin + "." + tc.Action, Arguments: tc.Args,
					}},
				})
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role: provider.RoleTool, Content: nativeToolContent(tr), ToolCallID: tc.ID,
				})
			} else {
				_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(*tc)})
				_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(tr)})
			}
			// The tool result is now in session history. Skip preparers, planner,
			// and user-message addition — jump straight to the agent loop so the
			// LLM summarizes the result for the user.
			toolCallConfirmed = true
		} else {
			emit.EmitConfirmationResolved(resolvedCtx, o.eventSink, emit.ConfirmationResolvedArgs{
				Choice:     "reject",
				ToolCallID: tc.ID,
			})
			// Rollback session to before this Run — remove the user message
			// and any tool calls/results that were added during the
			// confirmation attempt. Without this, the LLM sees prior tool
			// results and narrates instead of re-calling the tool.
			if s, _ := sessions.Get(sessionID); s != nil && msgCountAtStart < len(s.Messages) {
				_ = sessions.SetSummary(sessionID, s.Summary, s.Messages[:msgCountAtStart])
			}
			return &RunResult{
				Response: "OK, action cancelled.",
				Metadata: map[string]string{
					"type":   "system",
					"action": "confirmation_rejected",
				},
			}, nil
		}
	}

	content := userMessage
	// When a tool call was just confirmed and executed, the result is already
	// in session history. Skip preparers, planner, and user-message addition
	// — the LLM only needs to summarize the result.
	toolCallSeeded := toolCallConfirmed

	// Transcribe any audio files using STT-flagged preparers before the main preparer loop.
	if timing != nil {
		timing.begin("preparers")
	}
	if !toolCallSeeded {
		content, files = o.runSTTPreparers(ctx, content, files)
	}

	// Run content preparers before the first LLM call (config-driven).
	// relevantTools stays nil when no preparer returns a tools list.
	// An explicit empty slice means "preparer found nothing relevant" →
	// filter to empty (LLM sees no tools except those the plugin always
	// includes, such as ask_knowledge).
	// STT preparers are skipped here — they already ran in runSTTPreparers above.
	//
	// Enrich the content sent to preparers with the last user message from
	// session history so RAG semantic search matches the full intent, not
	// just a bare follow-up like "Item" (which would miss "create-item").
	if !toolCallSeeded {
		// Enrich the search query with recent conversation context so RAG
		// catches follow-ups like "both" after "Which one would you like
		// to delete?". Include both the last user message and last assistant
		// response — the assistant's question often carries the intent that
		// a bare follow-up ("both", "yes", "the first one") needs for
		// semantic search to find the right tools.
		searchText := content
		if sess, _ := sessions.Get(sessionID); sess != nil {
			var parts []string
			if prev := lastUserMessage(sess.Messages); prev != "" && prev != content {
				parts = append(parts, prev)
			}
			if asst := lastAssistantMessage(sess.Messages); asst != "" {
				// Truncate long assistant responses to keep the search query focused.
				if len(asst) > 200 {
					asst = asst[:200]
				}
				parts = append(parts, asst)
			}
			if len(parts) > 0 {
				searchText = strings.Join(parts, " ") + " " + content
			}
		}
		var relevantTools []string
		relevantToolsSet := false
		for _, prep := range o.preparers {
			if prep.STT {
				continue
			}
			guardedContent, blocked, tools, err := o.runSinglePreparerWithSearch(ctx, prep, content, searchText, "preparer", true)
			if err != nil {
				return nil, err
			}
			if blocked != nil {
				return blocked, nil
			}
			content = guardedContent
			// Last preparer that returns relevant_tools wins.
			// Distinguish nil (no tools field) from [] (explicitly empty).
			if tools != nil {
				relevantTools = tools
				relevantToolsSet = true
			}
		}
		// Store relevant tools in context so buildSystemPrompt and planner can filter.
		if relevantToolsSet {
			ctx = withRelevantTools(ctx, relevantTools)
		}
	}

	if timing != nil {
		timing.begin("planner")
	}
	// Block B: Run planner to check if this requires a multi-step pipeline.
	// The planner cost (~3s) is always worth it: even for single-action requests,
	// it enables server-side tool execution which saves ~20s of failed LLM rounds.
	singleStepSeeded := toolCallSeeded
	if o.planner != nil && !toolCallSeeded {
		log.Debug("planner running", "session", sessionID, "message", content)
		rtTools, rtSet := relevantToolsFromContext(ctx)
		plannerCaps := filterCapabilitiesByRelevantTools(o.registry.ListCapabilities(), rtTools, rtSet)
		// Build conversation context from session history so the planner
		// understands follow-up messages (e.g. "Item" after being asked
		// "which type of object?").
		sess, _ := sessions.Get(sessionID)
		convContext := buildPlannerConversationContext(sess)
		// Planner instrumentation: invoked + request fire before the
		// synchronous Plan() call; response fires after with a synthetic
		// summary in raw_content_excerpt because the pipeline.Planner
		// parses and discards the raw LLM response internally (capturing
		// the raw bytes would require a planner-pkg refactor — out of
		// scope here). One planner_step event per Step is emitted only on
		// pipeline plans; direct plans have no steps to enumerate.
		//
		// Reason "agent_loop" marks the main per-turn planning call. The
		// planner's other entry point — ClassifyConfirmation, used by
		// pending-pipeline / pending-tool-call confirmation paths above —
		// is intentionally not instrumented here: those calls are short
		// classifications, not full plans, and would dilute the analytics
		// signal if grouped under the same event_type.
		plannerInvokedID := emit.EmitPlannerInvoked(ctx, o.eventSink, "agent_loop")
		// Scope planner-internal emits — including the LLM call's
		// llm_request/llm_response, which fire from the provider while
		// Plan() runs — under planner_invoked as their parent. The outer
		// ctx (rooted at turn_start) is untouched, so the agent loop
		// below isn't pulled into the planner span.
		plannerCtx := emit.WithParent(ctx, plannerInvokedID)
		emit.EmitPlannerRequest(plannerCtx, o.eventSink, emit.PlannerRequestArgs{
			ModelID:      "", // planner uses provider-default routing; not exposed by pipeline.Planner
			MessageCount: 2,  // planner hardcodes system+user, see pipeline.Planner.Plan
		})
		plannerStart := time.Now()
		planResult, err := o.planner.Plan(plannerCtx, stripKnowledgeContext(content), capabilitiesToPlannerInfo(plannerCaps), convContext)
		plannerLatency := time.Since(plannerStart).Milliseconds()
		var plannerRaw string
		if err != nil {
			plannerRaw = "planner error: " + err.Error()
			log.Warn("planner error, falling through to agent loop", "error", err)
		} else {
			plannerRaw = fmt.Sprintf("planner ok: type=%s steps=%d", planResult.Type, len(planResult.Steps))
			log.Debug("planner result", "type", planResult.Type, "steps", len(planResult.Steps))
		}
		emit.EmitPlannerResponse(plannerCtx, o.eventSink, emit.PlannerResponseArgs{
			RawContent: plannerRaw,
			LatencyMS:  plannerLatency,
		})
		if err == nil && planResult.Type == "pipeline" {
			for i, step := range planResult.Steps {
				note := ""
				if step.Command != nil {
					note = step.Command.Plugin + "." + step.Command.Action
				}
				emit.EmitPlannerStep(plannerCtx, o.eventSink, emit.PlannerStepArgs{
					StepIndex: i,
					StepKind:  "tool",
					Note:      note,
				})
			}
		}
		if err == nil && planResult.Type == "pipeline" && len(planResult.Steps) > 1 {
			confResult := confirmationResult{RequiresConfirmation: true, ConfirmBeforeStep: 0}
			if o.confirmationPlugin != "" && o.confirmationAction != "" {
				confResult = o.checkConfirmationPlugin(ctx, planResult.Steps)
			}
			if !confResult.RequiresConfirmation {
				// All read-only — execute pipeline directly.
				log.Info("pipeline confirmation skipped by plugin", "steps", len(planResult.Steps))
				_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: content})
				p := pipeline.NewPipeline(planResult.Steps, o.pipelineConfig)
				return o.executePipeline(ctx, sessionID, p)
			}
			// Partial execution: run read prefix before the first write step,
			// then pause for confirmation with real data.
			_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: content})
			if confResult.ConfirmBeforeStep > 0 {
				// Execute read steps directly (not through executePipeline which
				// enters the LLM agent loop). Collect their context so the write
				// steps can resolve {{stepN.output}} references.
				readSteps := planResult.Steps[:confResult.ConfirmBeforeStep]
				executor := pipeline.NewExecutor(func(ctx context.Context, pluginName, action string, args map[string]any) pipeline.StepRunResult {
					wireArgs := pipelineArgsToWire(args)
					log.Debug("partial pipeline: executing read step", "plugin", pluginName, "action", action)
					call := ToolCall{
						ID:     fmt.Sprintf("pipeline-%s-%s", pluginName, action),
						Plugin: pluginName,
						Action: action,
						Args:   wireArgs,
					}
					result := o.executeCall(ctx, call)
					// Record in session history so the LLM has context.
					tr := ToolResult{CallID: call.ID, Content: result.Content, Error: result.Error}
					if o.supportsNativeTools() {
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role: provider.RoleAssistant,
							ToolCalls: []provider.ToolCall{{
								ID:        call.ID,
								Name:      call.Plugin + "." + call.Action,
								Arguments: call.Args,
							}},
						})
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role:       provider.RoleTool,
							Content:    nativeToolContent(tr),
							ToolCallID: call.ID,
						})
					} else {
						_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(call)})
						_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(tr)})
					}
					return pipeline.StepRunResult{
						Content:           result.Content,
						StructuredContent: result.StructuredContent,
						Error:             result.Error,
					}
				}, o.pipelineConfig)
				readPipeline := pipeline.NewPipeline(readSteps, o.pipelineConfig)
				_, readErr := executor.Run(ctx, readPipeline)
				if readErr != nil {
					return nil, readErr
				}
				log.Debug("partial pipeline: read prefix executed",
					"read_steps", confResult.ConfirmBeforeStep,
					"remaining_steps", len(planResult.Steps)-confResult.ConfirmBeforeStep)

				// Build write pipeline seeded with read step context.
				// Strip depends_on references to read steps — they already
				// executed and their output is in the context. The executor
				// would skip the write step otherwise because it checks
				// completed[dep] and the read steps aren't in its tracking.
				readStepIDs := make(map[string]bool, len(readSteps))
				for _, rs := range readSteps {
					readStepIDs[rs.ID] = true
				}
				writeSteps := planResult.Steps[confResult.ConfirmBeforeStep:]
				for _, ws := range writeSteps {
					var remaining []string
					for _, dep := range ws.DependsOn {
						if !readStepIDs[dep] {
							remaining = append(remaining, dep)
						}
					}
					ws.DependsOn = remaining
				}
				p := pipeline.NewPipeline(writeSteps, o.pipelineConfig)
				p.Context = readPipeline.Context
				// Narrate only the write steps for confirmation.
				var planText string
				if narrated, narrateErr := o.planner.NarratePlan(ctx, writeSteps, content); narrateErr != nil {
					planText = p.FormatForConfirmation()
				} else {
					planText = narrated
				}
				_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: planText})
				log.Debug("pipeline stored, awaiting confirmation for write steps",
					"pipeline_id", p.ID, "session", sessionID, "write_steps", len(writeSteps))
				confID := emit.EmitConfirmationRequested(ctx, o.eventSink, emit.ConfirmationRequestedArgs{
					Prompt:  planText,
					Choices: []string{"approve", "reject"},
				})
				o.pendingMu.Lock()
				o.pendingPipelines[sessionID] = p
				if confID != "" {
					o.pendingConfirmationIDs[sessionID] = confID
				}
				o.pendingMu.Unlock()
				return &RunResult{
					Response: planText,
					Metadata: map[string]string{
						"type":        "confirmation",
						"prompt_type": "confirmation",
						"pipeline_id": p.ID,
						"options":     "approve,reject",
					},
				}, nil
			}
			// ConfirmBeforeStep == 0: confirm entire pipeline (original behavior).
			p := pipeline.NewPipeline(planResult.Steps, o.pipelineConfig)
			var planText string
			if narrated, narrateErr := o.planner.NarratePlan(ctx, planResult.Steps, content); narrateErr != nil {
				log.Warn("plan narration failed, using fallback", "error", narrateErr)
				planText = p.FormatForConfirmation()
			} else {
				planText = narrated
			}
			_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: planText})
			log.Debug("pipeline stored, awaiting confirmation", "pipeline_id", p.ID, "session", sessionID, "steps", len(p.Steps))
			confID := emit.EmitConfirmationRequested(ctx, o.eventSink, emit.ConfirmationRequestedArgs{
				Prompt:  planText,
				Choices: []string{"approve", "reject"},
			})
			o.pendingMu.Lock()
			o.pendingPipelines[sessionID] = p
			if confID != "" {
				o.pendingConfirmationIDs[sessionID] = confID
			}
			o.pendingMu.Unlock()
			return &RunResult{
				Response: planText,
				Metadata: map[string]string{
					"type":        "confirmation",
					"prompt_type": "confirmation",
					"pipeline_id": p.ID,
					"options":     "approve,reject",
				},
			}, nil
		}
		// Single-step pipeline: execute the tool call server-side and seed the
		// agent loop with the result so the LLM only needs one round to summarize.
		// This avoids the common failure where the LLM narrates instead of calling
		// the tool, saving ~20s on the first attempt.
		if err == nil && planResult != nil && len(planResult.Steps) == 1 {
			step := planResult.Steps[0]
			if step.Command != nil && step.Command.Plugin != "" && step.Command.Action != "" {
				log.Debug("single-step pipeline: executing server-side",
					"plugin", step.Command.Plugin, "action", step.Command.Action)
				call := ToolCall{
					ID:     fmt.Sprintf("planner-%s-%s", step.Command.Plugin, step.Command.Action),
					Plugin: step.Command.Plugin,
					Action: step.Command.Action,
					Args:   pipelineArgsToWire(step.Command.Args),
				}
				toolResult := o.executeCall(ctx, call)
				if toolResult.Error == "" {
					// Seed session: user message, assistant tool call, tool result.
					_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: content})
					if o.supportsNativeTools() {
						// Native format: assistant with tool_calls + tool result with tool_call_id.
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role: provider.RoleAssistant,
							ToolCalls: []provider.ToolCall{{
								ID:        call.ID,
								Name:      call.Plugin + "." + call.Action,
								Arguments: call.Args,
							}},
						})
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role:       provider.RoleTool,
							Content:    nativeToolContent(toolResult),
							ToolCallID: call.ID,
						})
					} else {
						// Text-based format: assistant + user with [plugin_output].
						_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(call)})
						_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(toolResult)})
					}
					singleStepSeeded = true
					log.Debug("single-step pipeline: tool result seeded, entering agent loop for summary")
				} else {
					log.Warn("single-step pipeline: tool call failed, falling through to agent loop",
						"plugin", step.Command.Plugin, "action", step.Command.Action, "error", toolResult.Error)
				}
			}
		}
		// If "direct" or error, fall through to normal agent loop.
		// Store the planner's expected tools so the agent loop can retry if the
		// LLM fails to call them (language-independent tool-call detection).
		// For "direct" with no steps, use a sentinel step — the planner decided
		// this needs a tool call even though it didn't name a specific one.
		// Skip hint storage when we already executed the single step above.
		retryEnabled := o.retryToolCallsEnabled()
		if retryEnabled && planResult != nil && !singleStepSeeded {
			log.Debug("planner hint storage check", "retry_enabled", retryEnabled,
				"plan_type", planResult.Type, "plan_steps", len(planResult.Steps))
			if len(planResult.Steps) > 0 {
				log.Debug("planner expected tools stored in context", "steps", len(planResult.Steps))
				ctx = withExpectedTools(ctx, planResult.Steps)
			} else if planResult.Type == "direct" {
				log.Debug("planner returned direct (tool call expected), storing sentinel")
				ctx = withExpectedTools(ctx, []*pipeline.Step{{ID: "direct"}})
			}
		}
	}

	// Guard: never send empty user content to the LLM (would cause API errors).
	// Channels with media rules should always produce non-empty content, but this
	// catches misconfigured channels or unexpected message types.
	// Skip when single-step pipeline already seeded the session with messages.
	if !singleStepSeeded {
		if content == "" && len(files) == 0 {
			log.Debug("empty content and no files, returning fallback")
			return &RunResult{
				Response: "I received your message but couldn't read its content. Could you try sending it as text?",
				Metadata: map[string]string{
					"type":       "error",
					"error_code": "empty_content",
				},
			}, nil
		}
		if content == "" && len(files) > 0 {
			content = "[The user sent a file attachment.]"
		}

		if err := sessions.AddMessage(sessionID, provider.Message{
			Role:    provider.RoleUser,
			Content: content,
			Files:   files,
		}); err != nil {
			return nil, fmt.Errorf("adding user message: %w", err)
		}
	}
	// Run summarization asynchronously so it doesn't block the user's request.
	go o.maybeSummarizeSession(context.Background(), sessionID)

	result := &RunResult{}

	// Resolve profile model override: strip the provider prefix if present (e.g. "anthropic/claude-3-5" -> "claude-3-5").
	profileModel := ""
	if p := profile.FromContext(ctx); p != nil && p.Model != "" {
		if idx := strings.Index(p.Model, "/"); idx >= 0 {
			profileModel = p.Model[idx+1:]
		} else {
			profileModel = p.Model
		}
	}

	var totalInputTokens, totalOutputTokens, totalToolCalls int
	var modelUsed string
	defer func() {
		if o.usageRecorder != nil {
			if p := profile.FromContext(ctx); p != nil {
				o.usageRecorder.RecordUsage(ctx, p.EntityID, p.Group,
					p.ChannelID, sessionID, modelUsed,
					totalInputTokens, totalOutputTokens, totalToolCalls)
			}
		}
	}()

	// Resolve allowed plugins once per Run call and cache in ctx so that
	// buildSystemPrompt and executeCall share the result without a second DB hit.
	ctx = withAllowedPlugins(ctx, o.resolveAllowedPlugins(ctx))

	// Cache static parts of the LLM request once per Run so every agent-loop
	// round sends a byte-identical prefix. This maximizes provider-side KV
	// cache hits (vLLM automatic prefix caching, OpenAI prompt caching) and
	// avoids rebuilding ~25k tokens of tool definitions on every round.
	var cachedTools []provider.ToolDefinition
	nativeMode := o.supportsNativeTools()
	if nativeMode {
		cachedTools = o.buildToolDefinitions(ctx)
	}
	// Build system prompt variants: the only difference between rounds is the
	// format hint (suppressed until tool results exist). Pre-build both so the
	// prompt text is identical across rounds that share the same variant.
	sysPromptNoHint := o.buildSystemPrompt(withSkipFormatHint(ctx), content, true)
	sysPromptWithHint := o.buildSystemPrompt(ctx, content, true)

	// Emit turn_start once per Run — captures the static request shape
	// (system prompt digest, tool catalogue, model intent) that all agent
	// rounds in this turn share. prepareTurnStart also upserts the
	// underlying prompt bodies into prompt_snapshots when a store is
	// configured, so a consumer reading the event can resolve every
	// sha256 reference to content. Per-round mutations (e.g.
	// reasoning-effort downgrade on summary rounds) are NOT reflected
	// here; they remain on the individual llm_request events. The hinted
	// variant is canonical: the unhinted one only differs by suppressing
	// the format directive on round 1, which doesn't change tool
	// selection.
	//
	// Early-exit paths above (pending pipeline / pending tool-call
	// confirmation) intentionally bypass this emission: the LLM is not
	// driving those decisions, so there's no "turn" to start. They still
	// emit user_message (above) because the user did send input.
	turnStartID := emit.EmitTurnStart(ctx, o.eventSink, o.prepareTurnStart(ctx, sysPromptWithHint, profileModel, cachedTools))
	// Root every subsequent event in this turn under turn_start by
	// stamping it as the default parent on ctx. Specific spans (planner,
	// llm_response, tool_call_extracted) overwrite this slot with their
	// own id before nested emits so the analytics tree has tight scopes
	// — turn_start is the fallback root, not a flat list of children.
	ctx = emit.WithParent(ctx, turnStartID)

	var stripRetries int
	var toolRetries int // retries when planner expected tools but LLM didn't call any
	var transientMessages []provider.Message
	var lastCallSig string // "plugin.action\x00arg1=val1\x00..." for loop detection
	var repeatCount int
	agentRound := 0
	for i := 0; i < maxAgentLoopIterations; i++ {
		agentRound = i + 1
		sess, _ := sessions.Get(sessionID)

		// Pick the cached system prompt variant based on whether tool
		// results exist (format hint is only useful after the first tool round).
		cachedSysPrompt := sysPromptNoHint
		if hasToolResults(sess.Messages) {
			cachedSysPrompt = sysPromptWithHint
		}
		messages := o.buildMessagesWithPrompt(ctx, sess, cachedSysPrompt)
		messages = append(messages, transientMessages...)
		transientMessages = nil
		guardedMessages, blocked, err := o.runGuardPlugins(ctx, messages)
		if err != nil {
			return nil, err
		}
		if blocked != nil {
			return blocked, nil
		}

		// Log system prompt size from the first message (role=system).
		if len(guardedMessages) > 0 && guardedMessages[0].Role == provider.RoleSystem {
			log.Debug("system prompt", "round", i+1, "length", len(guardedMessages[0].Content))
		}
		log.Debug("LLM request", "round", i+1, "messages", len(guardedMessages))
		for j, m := range guardedMessages {
			log.Debug("LLM request message", "index", j+1, "role", m.Role, "content", m.Content)
		}

		req := &provider.CompletionRequest{Messages: guardedMessages}
		if profileModel != "" {
			req.Model = profileModel
		}

		// Native tool calling: reuse cached tool definitions on every round
		// so the LLM can chain multiple tool calls. The identical prefix
		// maximizes provider-side KV cache hits.
		if nativeMode {
			req.Tools = cachedTools
			if i == 0 {
				toolNames := make([]string, len(req.Tools))
				for ti, td := range req.Tools {
					toolNames[ti] = td.Name
				}
				log.Debug("native tools attached", "count", len(req.Tools), "tools", toolNames)
			}
		}
		log.Debug("tool calling mode", "native", nativeMode, "model", req.Model)

		// Enable reasoning only for pure text generation (no tools).
		// When tools are attached, the LLM's job is picking the right
		// tool + args — chain-of-thought adds ~10s overhead without
		// improving tool-calling accuracy.
		if o.supportsReasoning() && len(req.Tools) == 0 {
			req.Reasoning = true
			if p := profile.FromContext(ctx); p != nil && p.BudgetTokens > 0 {
				req.BudgetTokens = p.BudgetTokens
			}
			// Downgrade reasoning effort on summary rounds: when tool results
			// are already in session, the LLM only needs to format a response,
			// not reason deeply about which tools to call. Saves ~10s per round.
			// Check for empty too — applyModelDefaults sets it later, so at this
			// point it's "" which would become "high" from config.
			if hasToolResults(sess.Messages) && (req.ReasoningEffort == "high" || req.ReasoningEffort == "") {
				req.ReasoningEffort = "medium"
				log.Debug("reasoning effort downgraded for summary round", "round", i+1)
			}
			log.Debug("reasoning enabled for LLM request", "round", i+1,
				"budget_tokens", req.BudgetTokens,
				"reasoning_effort", req.ReasoningEffort)
		}

		// Use streaming when a callback is registered and the LLM supports it.
		// Streaming delivers tokens to the channel in real-time while still
		// collecting the full response for tool-call parsing.
		// A per-request callback (from context) takes priority over the global one.
		var resp *provider.CompletionResponse
		if timing != nil {
			// Measure what we're sending to the LLM so operators can see
			// where the 98k tokens come from.
			var systemChars, messagesChars, toolCount int
			for _, m := range guardedMessages {
				if m.Role == "system" {
					systemChars += len(m.Content)
				} else {
					messagesChars += len(m.Content)
				}
			}
			toolCount = len(req.Tools)
			var toolChars int
			for _, td := range req.Tools {
				toolChars += len(td.Name) + len(td.Description)
				if b, err := json.Marshal(td.Parameters); err == nil {
					toolChars += len(b)
				}
			}
			log.Info("llm request size",
				"round", agentRound,
				"system_chars", systemChars,
				"messages_count", len(guardedMessages),
				"messages_chars", messagesChars,
				"tools_count", toolCount,
				"tools_chars", toolChars)
			timing.begin(fmt.Sprintf("llm_round_%d", agentRound))
		}
		llmStart := time.Now()
		streamCB := o.resolveStreamCallback(ctx)
		if streamCB != nil {
			resp, err = o.streamComplete(ctx, req)
		} else {
			resp, err = o.llm.Complete(ctx, req)
		}
		llmDuration := time.Since(llmStart)
		if err != nil {
			return nil, fmt.Errorf("LLM completion: %w", err)
		}
		// Stamp the just-emitted llm_response as parent for the rest of
		// this iteration: tool_call_extracted / tool_call_result / retry
		// / confirmation events all form a tight subtree under the LLM
		// response that produced them. The next iteration's llm_request
		// inherits this parent, which produces a chain of rounds — each
		// round is causally a continuation of the previous, so the chain
		// is the correct semantic. resp.EventID is "" when no sink is
		// configured or the response was a refusal; we leave ctx alone
		// in that case so the existing turn_start parent stays.
		if resp.EventID != "" {
			ctx = emit.WithParent(ctx, resp.EventID)
		}
		log.Info("LLM response", "round", i+1, "duration", llmDuration.Round(time.Millisecond).String(), "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "content", resp.Content)

		if modelUsed == "" {
			modelUsed = resp.Model
		}
		totalInputTokens += resp.Usage.InputTokens
		totalOutputTokens += resp.Usage.OutputTokens

		// Prefer native tool calls from the API response over text-based parsing.
		var calls []ToolCall
		nativeToolCalls := len(resp.ToolCalls) > 0
		if nativeToolCalls {
			for _, tc := range resp.ToolCalls {
				plugin, action, _ := parseToolName(tc.Name)
				calls = append(calls, ToolCall{
					ID:      tc.ID,
					Plugin:  plugin,
					Action:  action,
					Args:    tc.Arguments,
					FromLLM: true,
				})
				log.Debug("native tool call", "id", tc.ID, "name", tc.Name, "args", tc.Arguments)
			}
			log.Info("native tool calls received", "round", i+1, "count", len(calls))
		} else {
			calls = o.parser.Parse(resp.Content)
			// Emit tool_call_parse_failed when the LLM clearly attempted a
			// structured tool call (a [tool_call] / <invoke> /
			// <function_calls> marker is present) but the parser produced
			// no valid call. Detected post-Parse rather than instrumenting
			// the parser to avoid an interface-signature ripple. raw_snippet
			// is the full response content (excerpted + sanitized by the
			// emit helper); per-block error detail is not surfaced today
			// because the default parser silently continues on each
			// individual decode failure — capturing that would require a
			// failures slice on the ToolCallParser interface.
			o.emitParseFailedIfApplicable(ctx, resp.Content, calls)
		}
		if calls == nil {
			// Detect hallucinated results: the LLM fabricated a response with
			// template variables (e.g. "{{plugin_output.pagination.total}}")
			// as if it had called a tool, but it didn't. Retry with a nudge.
			if hasHallucinatedResult(resp.Content) && stripRetries < 3 {
				stripRetries++
				log.Warn("hallucinated result detected, retrying", "round", i+1)
				emit.EmitRetry(ctx, o.eventSink, emit.RetryArgs{
					Phase:     "llm_call",
					Attempt:   stripRetries,
					LastError: "hallucinated tool result",
				})
				// Escape {{ to { { in the hallucinated content before sending it back —
				// vLLM chat templates use Jinja2 which chokes on unescaped {{ in messages.
				escaped := strings.ReplaceAll(resp.Content, "{{", "{ {")
				transientMessages = []provider.Message{
					{Role: provider.RoleAssistant, Content: escaped},
					{Role: provider.RoleUser, Content: "[system] Your previous response contained placeholder variables instead of real data. You MUST call the tool first using [tool_call] format, then answer with the actual results."},
				}
				continue
			}

			stripped := StripInternalBlocks(resp.Content)
			if stripped == "" {
				// Empty response — either the LLM returned nothing, or its output
				// was entirely unparseable tool call blocks that got stripped.
				// Retry once asking for a plain-language answer.
				if stripRetries >= 5 {
					log.Debug("LLM repeatedly produced empty/unparseable response, giving up", "round", i+1)
					result.Response = "(no response)"
					_ = sessions.AddMessage(sessionID, provider.Message{
						Role:    provider.RoleAssistant,
						Content: result.Response,
					})
					o.maybeRecordWorkflow(ctx, result, userMessage)
					return result, nil
				}
				stripRetries++
				emit.EmitRetry(ctx, o.eventSink, emit.RetryArgs{
					Phase:     "llm_call",
					Attempt:   stripRetries,
					LastError: "empty or unparseable response",
				})
				retryHint := "[system] Your previous response was empty or contained only unparseable tool call blocks. Please respond to the user in natural language. If you need to call a tool, use the [tool_call] format shown in your instructions."
				if resp.Content != "" {
					transientMessages = []provider.Message{
						{Role: provider.RoleAssistant, Content: resp.Content},
						{Role: provider.RoleUser, Content: retryHint},
					}
				} else {
					transientMessages = []provider.Message{
						{Role: provider.RoleUser, Content: retryHint},
					}
				}
				log.Debug("LLM produced empty/unparseable response, retrying", "round", i+1, "had_content", resp.Content != "")
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(stripRetries) * time.Second):
				}
				continue
			}
			// Planner-informed retry: the planner expected tool calls but the
			// LLM returned plain text. Retry up to 2 times with a nudge.
			// This is language-independent — no regex needed.
			steps := expectedToolsFromContext(ctx)
			log.Debug("planner retry check", "expected_steps", len(steps), "tool_retries", toolRetries, "will_retry", len(steps) > 0 && toolRetries < 2)
			if len(steps) > 0 && toolRetries < 2 {
				toolRetries++
				emit.EmitRetry(ctx, o.eventSink, emit.RetryArgs{
					Phase:     "llm_call",
					Attempt:   toolRetries,
					LastError: "planner expected tool call but LLM returned plain text",
				})
				log.Warn("planner expected tool calls but LLM returned plain text, retrying",
					"round", i+1, "retry", toolRetries, "expected_steps", len(steps))
				nudge := buildToolCallNudge(steps)
				transientMessages = []provider.Message{
					{Role: provider.RoleAssistant, Content: resp.Content},
					{Role: provider.RoleUser, Content: nudge},
				}
				continue
			}

			// Fallback: retries exhausted but planner has concrete steps.
			// Execute them server-side and feed results to the LLM for
			// summarisation. This rescues weak models that understand the
			// task but cannot produce [tool_call] format output.
			if invokeSteps := plannerStepsToInvoke(steps); len(invokeSteps) > 0 {
				log.Warn("retries exhausted, executing planner steps server-side",
					"round", i+1, "steps", len(invokeSteps))
				invokeResult, invokeErr := o.runInvokeSteps(ctx, invokeSteps)
				if invokeErr != nil {
					return nil, fmt.Errorf("server-side invoke: %w", invokeErr)
				}
				// Record tool calls in session so the LLM sees real data
				// on the next iteration and can produce a natural-language
				// summary.
				for j, call := range invokeResult.ToolCalls {
					result.ToolCalls = append(result.ToolCalls, call)
					if j < len(invokeResult.Results) {
						tr := invokeResult.Results[j]
						result.Results = append(result.Results, tr)
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role:    provider.RoleAssistant,
							Content: formatToolCallMessage(call),
						})
						_ = sessions.AddMessage(sessionID, provider.Message{
							Role:    provider.RoleUser,
							Content: o.guard.WrapContent(tr),
						})
					}
				}
				// Clear expected tools so the next LLM round doesn't
				// retry again — it should just summarise.
				ctx = withExpectedTools(ctx, nil)
				transientMessages = nil
				continue
			}

			result.Response = stripped
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
			_ = sessions.AddMessage(sessionID, provider.Message{
				Role:    provider.RoleAssistant,
				Content: resp.Content,
			})
			o.maybeRecordWorkflow(ctx, result, userMessage)
			o.applyShowToolCalls(result)
			if timing != nil {
				timing.begin("format")
			}
			if fmtErr := o.formatResponse(ctx, result); fmtErr != nil {
				return nil, fmtErr
			}
			if timing != nil {
				timing.end()
			}
			return result, nil
		}

		// The LLM narrated a tool call ("We need to call plugin__action")
		// instead of using [tool_call] format. Retry with an explicit nudge.
		if IsNarratedPlaceholder(calls) {
			log.Debug("narrated tool call detected, retrying with nudge", "round", i+1)
			transientMessages = []provider.Message{
				{Role: provider.RoleAssistant, Content: resp.Content},
				{Role: provider.RoleUser, Content: "[system] Do not describe what you will do. Execute the tool call NOW using the [tool_call] format shown in your instructions."},
			}
			continue
		}

		// Detect repeated identical tool calls — the LLM is stuck in a loop.
		callSig := toolCallSignature(calls)
		if callSig == lastCallSig {
			repeatCount++
		} else {
			lastCallSig = callSig
			repeatCount = 1
		}
		if repeatCount >= 2 {
			log.Warn("repeated identical tool call detected, breaking loop", "round", i+1, "repeat", repeatCount)
			transientMessages = []provider.Message{
				{Role: provider.RoleAssistant, Content: resp.Content},
				{Role: provider.RoleUser, Content: "[system] You are repeating the same tool call. Stop calling tools and answer the user based on the information you already have."},
			}
			continue
		}

		// LLM called tools — planner expectation satisfied, clear the hint
		// so the final answer (no tool calls) is not retried.
		if len(expectedToolsFromContext(ctx)) > 0 {
			log.Debug("planner tool expectation satisfied, clearing hint")
			ctx = withExpectedTools(ctx, nil)
		}

		// Attribute this LLM round's tokens evenly across the tool calls it produced.
		perCallInput := resp.Usage.InputTokens / max(len(calls), 1)
		perCallOutput := resp.Usage.OutputTokens / max(len(calls), 1)

		for i := range calls {
			calls[i].FromLLM = true

			// Tool-level confirmation: if the confirmation plugin says this
			// action needs confirmation, pause the agent loop and return a
			// confirmation prompt to the user. The pending call is stored
			// and executed on the next message if approved.
			if o.confirmationPlugin != "" && o.confirmationAction != "" && !calls[i].ConfirmationBypass {
				confResult := o.checkConfirmationPlugin(ctx, []*pipeline.Step{{
					ID:      calls[i].ID,
					Name:    calls[i].Action,
					Command: &pipeline.PluginCommand{Plugin: calls[i].Plugin, Action: calls[i].Action},
				}})
				if confResult.RequiresConfirmation {
					log.Info("tool call requires confirmation", "plugin", calls[i].Plugin, "action", calls[i].Action)
					pending := calls[i]
					// Build a confirmation message describing what will be done.
					// Prefer LLM narration to hide raw tool names from the user.
					var confirmMsg string
					if o.planner != nil {
						narrated, narrErr := o.planner.NarrateToolCall(ctx, calls[i].Action, calls[i].Args, userMessage)
						if narrErr != nil {
							log.Warn("tool call narration failed, using fallback", "error", narrErr)
						} else {
							confirmMsg = narrated
						}
					}
					if confirmMsg == "" {
						confirmMsg = fmt.Sprintf("I'm about to execute **%s** with the following parameters:\n", calls[i].Action)
						for k, v := range calls[i].Args {
							confirmMsg += fmt.Sprintf("- %s: %s\n", k, v)
						}
						confirmMsg += "\nWould you like me to proceed?"
					}
					// Store the confirmation message so the session has context
					// for the next turn (approval or follow-up questions). On
					// rejection, the rollback (msgCountAtStart) removes it.
					_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: confirmMsg})
					confID := emit.EmitConfirmationRequested(ctx, o.eventSink, emit.ConfirmationRequestedArgs{
						Prompt:     confirmMsg,
						Choices:    []string{"approve", "reject"},
						ToolCallID: calls[i].ID,
					})
					o.pendingMu.Lock()
					o.pendingToolCalls[sessionID] = &pending
					if confID != "" {
						o.pendingConfirmationIDs[sessionID] = confID
					}
					o.pendingMu.Unlock()
					// Also persist to session metadata so pending calls survive
					// restarts and work across multiple instances. The
					// confirmation_requested event id is embedded in the
					// serialized form so the parent_id linkage survives
					// pod restarts and multi-instance failover.
					savePendingToolCall(sessions, sessionID, &pending, confID)
					return &RunResult{
						Response: confirmMsg,
						Metadata: map[string]string{
							"type":        "confirmation",
							"prompt_type": "tool_confirmation",
							"options":     "approve,reject",
						},
					}, nil
				}
			}

			if timing != nil {
				timing.begin(fmt.Sprintf("tool_%s.%s", calls[i].Plugin, calls[i].Action))
			}
			pluginStart := time.Now()
			toolResult := o.executeCall(ctx, calls[i])
			pluginDuration := time.Since(pluginStart)
			call := calls[i]
			result.ToolCalls = append(result.ToolCalls, call)
			result.Results = append(result.Results, toolResult)
			totalToolCalls++

			log.Info("plugin call", "plugin", call.Plugin, "action", call.Action, "duration", pluginDuration.Round(time.Millisecond).String(), "error", toolResult.Error != "")
			if toolResult.Error != "" {
				log.Warn("tool call error", "plugin", call.Plugin, "action", call.Action, "error", toolResult.Error)
			}
			if o.pluginCallObserver != nil {
				o.pluginCallObserver.ObservePluginCall(call.Plugin, call.Action, toolResult.Error != "", perCallInput, perCallOutput)
			}

			if nativeToolCalls {
				// Native tool calling: store assistant message with tool_calls
				// and tool result as role=tool with tool_call_id.
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role:    provider.RoleAssistant,
					Content: resp.Content,
					ToolCalls: []provider.ToolCall{{
						ID:        call.ID,
						Name:      call.Plugin + "." + call.Action,
						Arguments: call.Args,
					}},
				})
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role:       provider.RoleTool,
					Content:    nativeToolContent(toolResult),
					ToolCallID: call.ID,
				})
			} else {
				// Text-based tool calling: store as assistant + user messages.
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role:    provider.RoleAssistant,
					Content: formatToolCallMessage(call),
				})
				_ = sessions.AddMessage(sessionID, provider.Message{
					Role:    provider.RoleUser,
					Content: o.guard.WrapContent(toolResult),
				})
			}
		}
	}

	return nil, fmt.Errorf("agent loop exceeded %d iterations", maxAgentLoopIterations)
}

// resolveStreamCallback returns the streaming callback for the current request.
// A per-request callback (from context, set by the channel handler) takes
// priority over the global one configured in OrchestratorOpts.
// ReasoningProvider is an optional interface that LLM providers implement
// to indicate they support extended thinking / reasoning.
type ReasoningProvider interface {
	SupportsFeature(feature provider.Feature) bool
}

// supportsReasoning returns true if the underlying LLM provider supports reasoning.
func (o *Orchestrator) supportsReasoning() bool {
	rp, ok := o.llm.(ReasoningProvider)
	return ok && rp.SupportsFeature(provider.FeatureReasoning)
}

// supportsNativeTools returns true if the LLM provider supports native function calling
// (structured tools parameter in the request, structured tool_calls in the response).
func (o *Orchestrator) supportsNativeTools() bool {
	rp, ok := o.llm.(ReasoningProvider)
	return ok && rp.SupportsFeature(provider.FeatureTools)
}

// buildToolDefinitions converts the visible plugin actions into provider.ToolDefinition
// for native function calling. Only includes actions visible to the current profile.
func (o *Orchestrator) buildToolDefinitions(ctx context.Context) []provider.ToolDefinition {
	allowedPlugins, _ := ctx.Value(allowedPluginsKey{}).(cachedAllowedPlugins)
	preparerAction := make(map[string]bool)
	for _, prep := range o.preparers {
		preparerAction[prep.Plugin+"."+prep.Action] = true
	}
	for _, g := range o.guards {
		preparerAction[g.Plugin+"."+g.Action] = true
	}

	relevantToolSet := make(map[string]bool)
	rtTools, relevantToolsActive := relevantToolsFromContext(ctx)
	for _, t := range rtTools {
		relevantToolSet[t] = true
	}

	var tools []provider.ToolDefinition
	for _, cap := range o.registry.ListCapabilities() {
		if !o.pluginAllowed(cap, allowedPlugins) {
			slog.Debug("tool registration: plugin excluded", "plugin", cap.Name)
			continue
		}
		for _, action := range cap.Actions {
			fqn := cap.Name + "." + action.Name
			if preparerAction[fqn] || action.UserOnly {
				slog.Debug("tool registration: action skipped (internal/user-only)", "tool", fqn)
				continue
			}
			if relevantToolsActive && !relevantToolSet[fqn] {
				slog.Debug("tool registration: action skipped (RAG filter)", "tool", fqn)
				continue
			}
			// Build JSON Schema for parameters.
			properties := make(map[string]interface{})
			var required []string
			for _, p := range action.Parameters {
				properties[p.Name] = map[string]interface{}{
					"type":        "string",
					"description": p.Description,
				}
				if p.Required {
					required = append(required, p.Name)
				}
			}
			tools = append(tools, provider.ToolDefinition{
				Name:        fqn,
				Description: action.Description,
				Parameters: map[string]interface{}{
					"type":                 "object",
					"properties":           properties,
					"required":             required,
					"additionalProperties": false,
				},
			})
			slog.Debug("tool registration: registered", "tool", fqn, "params", len(action.Parameters))
		}
	}
	return tools
}

// prepareTurnStart assembles the turn_start event payload from the static
// request shape held at agent-loop entry AND, when a PromptSnapshotStore
// is configured, synchronously upserts the underlying prompt bodies into
// prompt_snapshots BEFORE returning — so the emitted sha256 references
// resolve to content the moment the event becomes visible.
//
// Doing both in one place keeps the invariant tight: every (sha256, body)
// pair is durable in the store before the event referencing the sha256
// is emitted to the (potentially async-buffered) sink. A consumer
// reading the event can then look up the snapshot without racing the
// write.
//
// server_instructions are gathered from every registered capability's
// SystemPromptAddition — unfiltered by allowed_plugins on purpose: a
// consumer correlating system_prompt_sha256 to its server_instructions
// list should see the same set of additions that contributed to that
// hash. available_tools mirrors the cachedTools list used by the agent
// loop; when native tool calling is off the slice is empty (text-mode
// tools are described inside the system prompt and are already covered
// by system_prompt_sha256). Both slices are sorted by Name so consumers
// see stable order across runs — ListCapabilities() and cachedTools
// both depend on Go map iteration, which is not deterministic.
//
// snapshot upsert errors are logged and swallowed: a transient DB
// failure must not break the turn. The resulting event still ships;
// the consumer just sees a hash without a resolvable body until the
// snapshot is re-upserted on the next identical turn.
func (o *Orchestrator) prepareTurnStart(ctx context.Context, systemPrompt, modelID string, cachedTools []provider.ToolDefinition) emit.TurnStartArgs {
	args := emit.TurnStartArgs{ModelID: modelID}
	if systemPrompt != "" {
		sum := sha256.Sum256([]byte(systemPrompt))
		args.SystemPromptSHA256 = hex.EncodeToString(sum[:])
		o.upsertSnapshot(ctx, args.SystemPromptSHA256, events.PromptKindSystemPrompt, systemPrompt)
	}
	for _, cap := range o.registry.ListCapabilities() {
		if cap.SystemPromptAddition == "" {
			continue
		}
		sum := sha256.Sum256([]byte(cap.SystemPromptAddition))
		sha := hex.EncodeToString(sum[:])
		args.ServerInstructions = append(args.ServerInstructions, events.ServerInstructionRef{
			Name:   cap.Name,
			SHA256: sha,
		})
		o.upsertSnapshot(ctx, sha, events.PromptKindServerInstructions, cap.SystemPromptAddition)
	}
	sort.Slice(args.ServerInstructions, func(i, j int) bool {
		return args.ServerInstructions[i].Name < args.ServerInstructions[j].Name
	})
	for _, td := range cachedTools {
		sum := sha256.Sum256([]byte(td.Description))
		sha := hex.EncodeToString(sum[:])
		args.AvailableTools = append(args.AvailableTools, events.ToolRef{
			Name:       td.Name,
			DescSHA256: sha,
		})
		o.upsertSnapshot(ctx, sha, events.PromptKindToolDescription, td.Description)
	}
	sort.Slice(args.AvailableTools, func(i, j int) bool {
		return args.AvailableTools[i].Name < args.AvailableTools[j].Name
	})
	return args
}

// upsertSnapshot writes a (sha256, kind, content) tuple to the configured
// PromptSnapshotStore, swallowing nil-store and errors. Errors are
// logged at warn level — the turn continues regardless because the
// alternative (failing a user turn over an analytics snapshot write) is
// worse than a dangling hash reference.
func (o *Orchestrator) upsertSnapshot(ctx context.Context, sha256, kind, content string) {
	if o.snapshotStore == nil {
		return
	}
	if err := o.snapshotStore.UpsertPromptSnapshot(ctx, sha256, kind, content); err != nil {
		slog.WarnContext(ctx, "prompt_snapshot upsert failed",
			"sha256", sha256, "kind", kind, "error", err)
	}
}

// retryToolCallsEnabled returns true if the model opts in to planner-informed
// tool-call retries (for weak models that intermittently skip [tool_call] blocks).
func (o *Orchestrator) retryToolCallsEnabled() bool {
	rp, ok := o.llm.(ReasoningProvider)
	if !ok {
		slog.Debug("retry_tool_calls: LLM does not implement SupportsFeature interface")
		return false
	}
	enabled := rp.SupportsFeature(provider.FeatureRetryToolCalls)
	slog.Debug("retry_tool_calls feature check", "enabled", enabled)
	return enabled
}

func (o *Orchestrator) resolveStreamCallback(ctx context.Context) StreamChunkCallback {
	if sw := pkgchannel.StreamWriterFromContext(ctx); sw != nil {
		return sw.OnChunk
	}
	return o.onStreamChunk
}

// streamComplete attempts a streaming LLM call, forwarding chunks via the
// resolved callback and returning the fully assembled response. If the LLM
// does not support streaming it falls back to a regular Complete call.
func (o *Orchestrator) streamComplete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	// When tools are attached, fall back to non-streaming — the streaming
	// parser doesn't capture tool_calls from SSE deltas, so native tool
	// calls are silently lost and the agent loop sees empty responses.
	if len(req.Tools) > 0 {
		return o.llm.Complete(ctx, req)
	}
	sllm, ok := o.llm.(StreamingLLMClient)
	if !ok {
		return o.llm.Complete(ctx, req)
	}

	cb := o.resolveStreamCallback(ctx)

	stream, err := sllm.Stream(ctx, req)
	if err != nil {
		// If streaming fails, fall back to non-streaming.
		logger.FromContext(ctx).Debug("streaming unavailable, falling back to complete", "error", err)
		return o.llm.Complete(ctx, req)
	}
	// Close is idempotent (see oaiResponseStream.closed guard); the
	// explicit Close after the loop forces emitStreamEnd to fire BEFORE
	// we read EventID below, so the deferred Close becomes a safety net
	// rather than the actual emit trigger.
	defer func() { _ = stream.Close() }()

	var buf strings.Builder
	var usage provider.Usage
	var model string
	showLabels := o.showToolCalls == "friendly" || o.showToolCalls == "raw" || o.showToolCalls == "true"
	filter := newStreamTagFilter(showLabels)
	for {
		chunk, err := stream.Recv()
		if err != nil {
			return nil, fmt.Errorf("stream recv: %w", err)
		}
		if chunk.Content != "" {
			buf.WriteString(chunk.Content)
			if cb != nil {
				// Filter out [tool_call]...[/tool_call] blocks so users
				// never see raw protocol tags in streaming updates.
				if visible := filter.Feed(chunk.Content); visible != "" {
					cb(ctx, visible, false)
				}
			}
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 {
			usage = chunk.Usage
		}
		if chunk.Done {
			if cb != nil {
				// Flush any buffered text that wasn't part of a tag.
				if trailing := filter.Flush(); trailing != "" {
					cb(ctx, trailing, false)
				}
				cb(ctx, "", true)
			}
			break
		}
	}

	// Fire the end-of-stream emit now so EventID is populated before we
	// build the response. Any stream implementation that doesn't surface
	// an event id (test fakes, future providers) simply returns "".
	_ = stream.Close()
	var eventID string
	if exposer, ok := stream.(interface{ EventID() string }); ok {
		eventID = exposer.EventID()
	}

	return &provider.CompletionResponse{
		Content: buf.String(),
		Model:   model,
		Usage:   usage,
		EventID: eventID,
	}, nil
}

// streamTagFilter suppresses [tool_call]...[/tool_call] blocks from streamed
// output so channel users never see raw protocol tags. When a complete block
// is detected, the filter emits a short human-friendly placeholder like
// "⏳ Calling plugin.action…\n" instead of the raw protocol content.
//
// Because tags may arrive split across multiple chunks (e.g. "[tool_" then
// "call]"), the filter buffers partial matches against the opening/closing tag
// and only emits or drops once the match is confirmed or refuted.
type streamTagFilter struct {
	inside     bool   // true while between [tool_call] and [/tool_call]
	pending    string // buffered text that might be the start of a tag
	blockBody  string // accumulated body inside a [tool_call] block
	showLabels bool   // when true, emit friendly "_plugin → action…_" labels

	// Narration holdback: buffer outgoing text and suppress it if it
	// turns out to be a narrated tool call like "We will call list-containers…"
	narrationBuf string // text held back while checking for narration
}

func newStreamTagFilter(showLabels bool) *streamTagFilter {
	return &streamTagFilter{showLabels: showLabels}
}

const (
	streamOpenTag  = "[tool_call]"
	streamCloseTag = "[/tool_call]"
)

// Feed processes new text and returns the portion that should be forwarded to
// the channel callback. It may return "" if all text is inside a tool_call
// block or is being buffered for a partial tag match.
func (f *streamTagFilter) Feed(text string) string {
	f.pending += text
	var out strings.Builder
	for f.pending != "" {
		if f.inside {
			// Look for closing tag.
			idx := strings.Index(f.pending, streamCloseTag)
			if idx >= 0 {
				// Accumulate the body and optionally emit a friendly placeholder.
				f.blockBody += f.pending[:idx]
				f.pending = f.pending[idx+len(streamCloseTag):]
				f.inside = false
				if f.showLabels {
					if friendly := toolCallFriendlyLabel(f.blockBody); friendly != "" {
						out.WriteString(friendly)
					}
				}
				f.blockBody = ""
				continue
			}
			// Check if pending ends with a prefix of the closing tag.
			if partialTagSuffix(f.pending, streamCloseTag) > 0 {
				break // keep buffering
			}
			// No match — accumulate all buffered content as block body.
			f.blockBody += f.pending
			f.pending = ""
		} else {
			// Look for opening tag.
			idx := strings.Index(f.pending, streamOpenTag)
			if idx >= 0 {
				// Emit everything before the tag.
				out.WriteString(f.pending[:idx])
				f.pending = f.pending[idx+len(streamOpenTag):]
				f.inside = true
				f.blockBody = ""
				continue
			}
			// Check if pending ends with a prefix of the opening tag.
			if n := partialTagSuffix(f.pending, streamOpenTag); n > 0 {
				// Emit everything except the partial match at the end.
				out.WriteString(f.pending[:len(f.pending)-n])
				f.pending = f.pending[len(f.pending)-n:]
				break
			}
			// No tag at all — emit everything.
			out.WriteString(f.pending)
			f.pending = ""
		}
	}
	return f.filterNarration(out.String())
}

// filterNarration buffers outgoing text and suppresses it if it matches a
// narrated tool call pattern (e.g. "We will call list-containers…"). Text is
// held back until a sentence boundary (period, newline) confirms or refutes
// the narration. If the sentence is not a narration, the held-back text is
// flushed to the caller.
func (f *streamTagFilter) filterNarration(text string) string {
	if text == "" {
		return ""
	}
	f.narrationBuf += text

	var out strings.Builder

	// Process sentence by sentence. A "sentence" ends at '.' or '\n'.
	for {
		idx := strings.IndexAny(f.narrationBuf, ".\n")
		if idx < 0 {
			// No sentence boundary yet — keep buffering if the text so far
			// could still be the start of a narration.
			if couldBeNarration(f.narrationBuf) {
				break
			}
			// Not a narration prefix — release everything.
			out.WriteString(f.narrationBuf)
			f.narrationBuf = ""
			break
		}

		sentence := f.narrationBuf[:idx+1]
		f.narrationBuf = f.narrationBuf[idx+1:]

		if hasNarratedToolCall(sentence) || hasNarratedIntent(sentence) {
			// Suppress the narrated sentence; continue processing remainder.
			continue
		}
		out.WriteString(sentence)
	}

	return out.String()
}

// couldBeNarration returns true if s could be the beginning of a narrated
// tool call sentence. Matches common LLM narration prefixes.
func couldBeNarration(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	prefixes := []string{
		"we will", "we'll", "we need to", "we should",
		"i will", "i'll", "i need to", "i'm going to",
		"let me", "let's",
		"now ", "next ",
		"i'll fetch", "we'll fetch", "i'll check", "we'll check",
		"i'll get", "we'll get", "i'll look", "we'll look",
		"i'll search", "we'll search", "i'll query", "we'll query",
		"i'll find", "we'll find", "i'll list", "we'll list",
		"i'll count", "we'll count", "i'll retrieve", "we'll retrieve",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// Flush returns any buffered text that was held back for partial tag matching.
// Called when the stream ends.
func (f *streamTagFilter) Flush() string {
	s := f.pending
	f.pending = ""
	f.blockBody = ""
	if f.inside {
		// Unclosed block — drop the buffered content.
		f.inside = false
		s = ""
	}
	// Release any narration buffer — fail-open: if the stream ends
	// mid-sentence, show the text rather than swallowing it.
	if f.narrationBuf != "" {
		held := f.narrationBuf
		f.narrationBuf = ""
		// Even on flush, suppress confirmed narrations.
		if hasNarratedToolCall(held) || hasNarratedIntent(held) {
			return s
		}
		return s + held
	}
	return s
}

// toolCallFriendlyLabel extracts the tool name from a [tool_call] block body
// and returns a short human-readable string. Returns "" if no name is found.
func toolCallFriendlyLabel(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	var toolName string

	// Format A: {"tool": "plugin.action", ...}
	var block toolCallJSON
	if json.Unmarshal([]byte(body), &block) == nil && block.Tool != "" {
		toolName = block.Tool
	} else {
		// Format B/C: first line is "plugin.action" or "plugin.action(...)"
		firstLine := body
		if nl := strings.IndexByte(body, '\n'); nl >= 0 {
			firstLine = body[:nl]
		}
		firstLine = strings.TrimSpace(firstLine)
		if paren := strings.IndexByte(firstLine, '('); paren > 0 {
			firstLine = firstLine[:paren]
		}
		// Validate it looks like a tool name (has a dot or double underscore).
		if strings.Contains(firstLine, ".") || strings.Contains(firstLine, "__") {
			toolName = firstLine
		}
	}

	if toolName == "" {
		return ""
	}

	// Normalise double-underscore to dot for display.
	if !strings.Contains(toolName, ".") {
		if dunder := strings.Index(toolName, "__"); dunder > 0 {
			toolName = toolName[:dunder] + "." + toolName[dunder+2:]
		}
	}

	// Split into plugin and action for a friendlier display.
	if dot := strings.LastIndex(toolName, "."); dot > 0 && dot < len(toolName)-1 {
		plugin := toolName[:dot]
		action := toolName[dot+1:]
		return fmt.Sprintf("_%s → %s…_\n", plugin, action)
	}
	return fmt.Sprintf("_Calling %s…_\n", toolName)
}

// partialTagSuffix returns the length of the longest suffix of s that is a
// prefix of tag. For example, partialTagSuffix("abc[tool", "[tool_call]")
// returns 5 ("[tool").
func partialTagSuffix(s, tag string) int {
	maxLen := len(tag) - 1
	if maxLen > len(s) {
		maxLen = len(s)
	}
	for n := maxLen; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
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
		nextContent, blocked, _, err := o.runSinglePreparer(ctx, g, content, "guard", false)
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

// confirmationResult holds the parsed response from the confirmation plugin.
type confirmationResult struct {
	RequiresConfirmation bool `json:"requires_confirmation"`
	ConfirmBeforeStep    int  `json:"confirm_before_step"` // index of first write step; -1 if none
}

// checkConfirmationPlugin asks the configured confirmation plugin whether a
// pipeline requires user confirmation. Returns confirm=true on any error (fail-safe).
func (o *Orchestrator) checkConfirmationPlugin(ctx context.Context, steps []*pipeline.Step) confirmationResult {
	log := logger.FromContext(ctx)
	failSafe := confirmationResult{RequiresConfirmation: true, ConfirmBeforeStep: 0}

	type stepInfo struct {
		Plugin string `json:"plugin"`
		Action string `json:"action"`
		Name   string `json:"name"`
	}
	infos := make([]stepInfo, len(steps))
	for i, s := range steps {
		infos[i] = stepInfo{Plugin: s.Command.Plugin, Action: s.Command.Action, Name: s.Name}
	}
	stepsJSON, err := json.Marshal(infos)
	if err != nil {
		log.Warn("confirmation plugin: marshal error, requiring confirmation", "error", err)
		return failSafe
	}
	call := ToolCall{
		ID:     "confirmation-check",
		Plugin: o.confirmationPlugin,
		Action: o.confirmationAction,
		Args:   map[string]string{"steps": string(stepsJSON)},
	}
	result := o.executeCall(ctx, call)
	if result.Error != "" {
		log.Warn("confirmation plugin error, requiring confirmation", "error", result.Error)
		return failSafe
	}
	var resp confirmationResult
	content := result.StructuredContent
	if content == "" {
		content = result.Content
	}
	if err := json.Unmarshal([]byte(content), &resp); err != nil {
		log.Warn("confirmation plugin: invalid response, requiring confirmation", "error", err, "content", content)
		return failSafe
	}
	log.Debug("confirmation plugin result",
		"requires_confirmation", resp.RequiresConfirmation,
		"confirm_before_step", resp.ConfirmBeforeStep)
	return resp
}

func (o *Orchestrator) executePipeline(ctx context.Context, sessionID string, p *pipeline.Pipeline) (*RunResult, error) {
	log := logger.FromContext(ctx)
	runner := func(ctx context.Context, pluginName, action string, args map[string]any) pipeline.StepRunResult {
		// Stringify typed args at the wire boundary; ToolCall.Args is
		// map[string]string by gRPC contract. The downstream plugin re-
		// coerces per its declared input schema.
		wireArgs := pipelineArgsToWire(args)
		log.Debug("pipeline executing step", "plugin", pluginName, "action", action, "args", wireArgs)
		call := ToolCall{
			ID:     fmt.Sprintf("pipeline-%s-%s", pluginName, action),
			Plugin: pluginName,
			Action: action,
			Args:   wireArgs,
		}
		result := o.executeCall(ctx, call)
		if result.Error != "" {
			log.Warn("pipeline step failed", "plugin", pluginName, "action", action, "error", result.Error)
		} else {
			preview := result.Content
			if len(preview) > 500 {
				preview = preview[:500] + "... [truncated]"
			}
			log.Debug("pipeline step succeeded", "plugin", pluginName, "action", action, "result", preview)
		}
		return pipeline.StepRunResult{
			Content:           result.Content,
			StructuredContent: result.StructuredContent,
			Error:             result.Error,
		}
	}
	executor := pipeline.NewExecutor(runner, p.Config)
	execResult, err := executor.Run(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("pipeline execution: %w", err)
	}

	log.Debug("pipeline execution done", "success", execResult.Success, "steps_executed", len(execResult.Steps))

	// Record step results in session history. Args are stringified at the
	// recording boundary so the trace mirrors what hit the wire.
	var toolCalls []ToolCall
	var toolResults []ToolResult
	for _, es := range execResult.Steps {
		tc := ToolCall{
			ID:     fmt.Sprintf("pipeline-%s-%s", es.Plugin, es.Action),
			Plugin: es.Plugin,
			Action: es.Action,
			Args:   pipelineArgsToWire(es.Args),
		}
		tr := ToolResult{CallID: tc.ID, Content: es.Content, Error: es.Error}
		toolCalls = append(toolCalls, tc)
		toolResults = append(toolResults, tr)
		if o.supportsNativeTools() {
			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role: provider.RoleAssistant,
				ToolCalls: []provider.ToolCall{{
					ID:        tc.ID,
					Name:      tc.Plugin + "." + tc.Action,
					Arguments: tc.Args,
				}},
			})
			_ = o.sessions.AddMessage(sessionID, provider.Message{
				Role:       provider.RoleTool,
				Content:    nativeToolContent(tr),
				ToolCallID: tc.ID,
			})
		} else {
			_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: formatToolCallMessage(tc)})
			_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleUser, Content: o.guard.WrapContent(tr)})
		}
	}
	_ = o.sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleAssistant, Content: execResult.Summary})

	return &RunResult{
		Response:  execResult.Summary,
		ToolCalls: toolCalls,
		Results:   toolResults,
	}, nil
}

// sanitizeHistory removes poisoned assistant messages from session history.
// An assistant message is "legitimate" only if it contains a [tool_call] block
// OR is immediately preceded by a [plugin_output] (meaning it's a summary of
// real tool results). Everything else is the LLM talking without acting —
// hallucinated numbers, narrated intent, placeholder text — and teaches the
// model to repeat the same bad pattern.
func sanitizeHistory(msgs []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == provider.RoleAssistant {
			// Keep assistant messages that contain VALID tool calls — they drove real actions.
			// Drop broken tool calls like "[tool_call] ." or "[tool_call] " that were
			// malformed by the LLM; keeping them in history teaches bad patterns.
			if strings.Contains(m.Content, "[tool_call]") && !isBrokenToolCall(m.Content) {
				out = append(out, m)
				continue
			}
			// Keep assistant messages with native tool calls (ToolCalls field set).
			if len(m.ToolCalls) > 0 {
				out = append(out, m)
				continue
			}
			// Keep assistant messages that follow a tool result — they summarize
			// real data (the normal round-2 response after a tool call).
			if i > 0 && (strings.Contains(msgs[i-1].Content, "[plugin_output]") || msgs[i-1].Role == provider.RoleTool) {
				out = append(out, m)
				continue
			}
			// Everything else is the LLM answering without calling a tool.
			// Drop it and its orphaned preceding user message.
			if len(out) > 0 && out[len(out)-1].Role == provider.RoleUser {
				out = out[:len(out)-1]
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// isBrokenToolCall returns true if the message contains a [tool_call] block
// that is malformed — e.g. "[tool_call] ." or "[tool_call] " without a valid
// JSON body or plugin.action identifier. These are LLM mistakes that should
// be stripped from history to avoid teaching bad patterns.
func isBrokenToolCall(content string) bool {
	idx := strings.Index(content, "[tool_call]")
	if idx < 0 {
		return false
	}
	after := strings.TrimSpace(content[idx+len("[tool_call]"):])
	// A valid tool call must contain either JSON ({) or a plugin.action identifier (at least "a.b").
	// Broken calls are typically just ".", empty, or a single word.
	if after == "" || after == "." {
		return true
	}
	// Check for the compact format: "[tool_call] plugin.action(args)" — valid if it has a dot before any paren.
	// Check for JSON format: "[tool_call]\n{...}\n[/tool_call]" — valid if it has a brace.
	if strings.ContainsAny(after, "{") {
		return false // has JSON body
	}
	if strings.Contains(after, ".") && len(after) > 3 {
		return false // has plugin.action
	}
	return true
}

// hasToolResults returns true if the session history contains at least one
// message with a [plugin_output] block, indicating a prior tool-call round.
func hasToolResults(msgs []provider.Message) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, "[plugin_output]") {
			return true
		}
		// Native tool calling: role=tool messages are tool results.
		if m.Role == provider.RoleTool {
			return true
		}
	}
	return false
}

func (o *Orchestrator) buildMessages(ctx context.Context, sess *state.Session, userMessage string, includeServerInstructions ...bool) []provider.Message {
	messages := make([]provider.Message, 0, len(sess.Messages)+4)

	inclSI := len(includeServerInstructions) == 0 || includeServerInstructions[0]
	// Suppress the OUTPUT FORMAT section in the system prompt until tool
	// results exist. The HTML format hint prevents weak models from
	// generating [tool_call] blocks on the first round.
	buildCtx := ctx
	hasResults := hasToolResults(sess.Messages)
	if !hasResults {
		buildCtx = withSkipFormatHint(ctx)
	}
	slog.Debug("format hint decision", "has_tool_results", hasResults, "skip_format_hint", !hasResults)
	systemPrompt := o.buildSystemPrompt(buildCtx, userMessage, inclSI)
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
	// When context_messages is set, keep only the last N messages to avoid
	// blowing the context window. This is a simple sliding window that
	// preserves tool results (unlike LLM summarization which loses them).
	convMessages := sess.Messages
	if o.contextMessages > 0 && len(convMessages) > o.contextMessages {
		convMessages = convMessages[len(convMessages)-o.contextMessages:]
	}
	// Strip [knowledge_context] from ALL user messages including the current turn.
	// Server instructions are already in the system prompt via SystemPromptAddition,
	// and the preparer's knowledge_context duplicates them. Stripping all turns
	// prevents both per-turn duplication and redundancy with the system prompt.
	//
	// Also sanitize poisoned assistant messages from session history: remove
	// hallucinated template variables and narrated tool calls that were saved
	// before detection was in place. These teach the LLM bad patterns.
	messages = appendStrippingAllKC(messages, sanitizeHistory(convMessages))

	// For weaker / OSS models that tend to ignore system-prompt formatting
	// instructions, repeat the channel format hint as a trailing system
	// reminder. Only inject it after at least one tool-result exchange so it
	// doesn't compete with the tool-calling instruction on the very first
	// round — the OUTPUT FORMAT section in the system prompt is sufficient
	// there. Once tool results are present the model switches to answer
	// mode and benefits from the nudge.
	if hint := channelFormatHint(ctx); hint != "" && hasToolResults(convMessages) {
		messages = append(messages, provider.Message{
			Role:    provider.RoleSystem,
			Content: "[IMPORTANT — output format reminder] " + hint,
		})
	}

	// Only inject the "don't repeat" reminder when there's actual prior
	// conversation (at least one assistant reply). On the first turn there's
	// nothing to repeat.
	if len(convMessages) > 2 {
		messages = append(messages, provider.Message{
			Role:    provider.RoleSystem,
			Content: "[IMPORTANT] Answer ONLY the user's last message above. Do NOT repeat or summarize any earlier answers from this conversation. Be concise.",
		})
	}

	if o.contextWindow > 0 {
		messages = trimToContextWindow(ctx, messages, o.contextWindow)
	}

	return messages
}

// buildMessagesWithPrompt assembles the LLM message list using a pre-built
// system prompt. This avoids rebuilding the prompt on every agent-loop round,
// keeping the prefix byte-identical for provider-side KV cache hits.
func (o *Orchestrator) buildMessagesWithPrompt(ctx context.Context, sess *state.Session, systemPrompt string) []provider.Message {
	messages := make([]provider.Message, 0, len(sess.Messages)+4)
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
	convMessages := sess.Messages
	if o.contextMessages > 0 && len(convMessages) > o.contextMessages {
		convMessages = convMessages[len(convMessages)-o.contextMessages:]
	}
	messages = appendStrippingAllKC(messages, sanitizeHistory(convMessages))

	if hint := channelFormatHint(ctx); hint != "" && hasToolResults(convMessages) {
		messages = append(messages, provider.Message{
			Role:    provider.RoleSystem,
			Content: "[IMPORTANT — output format reminder] " + hint,
		})
	}

	// Trailing instruction placed close to the generation point where
	// the model is most likely to follow it. The same instruction in the
	// preamble (far away) gets buried under 50-100k tokens.
	// Only inject the "don't repeat" reminder when there's actual prior
	// conversation (at least one assistant reply). On the first turn there's
	// nothing to repeat.
	if len(convMessages) > 2 {
		messages = append(messages, provider.Message{
			Role:    provider.RoleSystem,
			Content: "[IMPORTANT] Answer ONLY the user's last message above. Do NOT repeat or summarize any earlier answers from this conversation. Be concise.",
		})
	}

	if o.contextWindow > 0 {
		messages = trimToContextWindow(ctx, messages, o.contextWindow)
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
func trimToContextWindow(ctx context.Context, messages []provider.Message, contextWindow int) []provider.Message {
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
		logger.FromContext(ctx).Info("context trimming", "dropped", len(messages)-len(trimmed), "tokens", total, "limit", maxInputTokens)
	}

	return trimmed
}

func (o *Orchestrator) buildSystemPrompt(ctx context.Context, userMessage string, includeServerInstructions bool) string {
	var sb strings.Builder
	// When the provider supports native tool calling, use a preamble that
	// omits the text-based [tool_call] format instructions. Sending both
	// the text format and native tools confuses weaker models — they
	// narrate instead of calling tools.
	if o.supportsNativeTools() {
		sb.WriteString(prompts.OrchestratorPreambleNative)
	} else {
		sb.WriteString(prompts.OrchestratorPreamble)
	}

	sb.WriteString(o.rules.BuildPromptSection())

	if o.runtimePromptPath != "" {
		if data, err := os.ReadFile(o.runtimePromptPath); err == nil {
			sb.WriteString("\n## Additional instructions (editable from chat)\n")
			sb.WriteString(string(data))
			sb.WriteString("\n\n")
		}
	}

	// Expose the caller's channel and conversation id so the LLM knows what
	// "the current channel" means when a tool parameter says it defaults to
	// it. Without this, small models (e.g. Haiku) fail to invoke tools like
	// scheduler.create_job on non-Slack channels because they pattern-match
	// the Slack-shaped channel ids in other tool schemas and conclude they
	// don't have a valid one on Telegram/Discord/etc.
	if session := sessionDescriptor(ctx); session != "" {
		sb.WriteString("## Current session\n")
		sb.WriteString(session)
		sb.WriteString("\nWhen a tool parameter's description says it defaults to the current channel or conversation, OMIT it — the host injects these values automatically. Do not try to invent or ask for an id.\n\n")
	}

	// Don't list content-preparer or guard actions as tools; they run automatically before LLM calls.
	preparerAction := make(map[string]bool)
	for _, prep := range o.preparers {
		preparerAction[prep.Plugin+"."+prep.Action] = true
	}
	for _, g := range o.guards {
		preparerAction[g.Plugin+"."+g.Action] = true
	}

	// Resolve the set of plugins allowed for the current profile group (if any).
	allowedPlugins := o.resolveAllowedPlugins(ctx)

	// Build a set of relevant tools from the preparer (RAG) for filtering.
	// When a preparer ran (relevantToolsActive=true), only tools in the set
	// are shown — an empty set means "preparer found nothing relevant".
	// When no preparer ran (relevantToolsActive=false), all tools are shown.
	relevantToolSet := make(map[string]bool)
	rtTools, relevantToolsActive := relevantToolsFromContext(ctx)
	for _, t := range rtTools {
		relevantToolSet[t] = true
	}

	caps := o.registry.ListCapabilities()

	// When relevantToolsActive, collect plugin names that have actions but none
	// matched — these go into a compact "discoverable plugins" section so the
	// LLM knows they exist and can use ask_knowledge to find their actions.
	var discoverablePlugins []string

	for _, cap := range caps {
		if !o.pluginAllowed(cap, allowedPlugins) {
			slog.Debug("plugin excluded from system prompt",
				"plugin", cap.Name,
				"strict", allowedPlugins.strict,
				"allowed_groups", cap.AllowedGroups,
				"in_allowlist", allowedPlugins.m[cap.Name],
			)
			continue
		}
		slog.Debug("plugin included in system prompt", "plugin", cap.Name,
			"system_prompt_addition_len", len(cap.SystemPromptAddition))

		// Filter actions: exclude preparer/guard actions, user-only, and
		// (when RAG is active) actions not in the relevant set.
		var visibleActions []Action
		hasNonInternalActions := false
		for _, action := range cap.Actions {
			if preparerAction[cap.Name+"."+action.Name] || action.UserOnly {
				continue
			}
			hasNonInternalActions = true
			if relevantToolsActive && !relevantToolSet[cap.Name+"."+action.Name] {
				continue
			}
			visibleActions = append(visibleActions, action)
		}

		// When RAG is active and no actions matched, add to discoverable list
		// so the LLM knows the plugin exists and can query ask_knowledge.
		// Preserve server instructions even when actions are filtered out —
		// they are plugin-level guidance the LLM needs regardless of which
		// specific actions are visible.
		if relevantToolsActive && len(visibleActions) == 0 && hasNonInternalActions {
			discoverablePlugins = append(discoverablePlugins, cap.Name)
			if includeServerInstructions && cap.SystemPromptAddition != "" {
				fmt.Fprintf(&sb, "--- plugin: %s ---\n%s\n--- end plugin: %s ---\n\n",
					cap.Name, cap.SystemPromptAddition, cap.Name)
			}
			continue
		}

		// Skip plugin header entirely if no actions are visible after filtering.
		if len(visibleActions) == 0 && (!includeServerInstructions || cap.SystemPromptAddition == "") {
			continue
		}

		fmt.Fprintf(&sb, "## %s\n%s\n", cap.Name, cap.Description)
		// Server instructions first — domain context the LLM needs before
		// reading tool definitions (e.g. entity relationships, counting
		// patterns, field semantics).
		if includeServerInstructions && cap.SystemPromptAddition != "" {
			fmt.Fprintf(&sb, "--- plugin: %s ---\n%s\n--- end plugin: %s ---\n", cap.Name, cap.SystemPromptAddition, cap.Name)
		}
		// In native tool mode, tool definitions are sent via req.Tools —
		// don't duplicate them in the system prompt text. This saves ~50k
		// tokens on accounts with many tools (e.g. 71 MCP tools).
		if !o.supportsNativeTools() {
			for _, action := range visibleActions {
				fmt.Fprintf(&sb, "- %s.%s: %s\n", cap.Name, action.Name, action.Description)
				for _, p := range action.Parameters {
					req := ""
					if p.Required {
						req = " (required)"
					}
					fmt.Fprintf(&sb, "  - %s: %s%s\n", p.Name, p.Description, req)
				}
			}
		}
		sb.WriteString("\n")
	}

	// When RAG filtering is active, list plugins that have actions but none
	// matched the current query. The LLM can use ask_knowledge to discover
	// their actions on demand. Use alias names (e.g. "jira", "tickets") when
	// available, since those are the names the LLM should use.
	if len(discoverablePlugins) > 0 {
		sb.WriteString("## Other available plugins\n")
		sb.WriteString("These plugins have tools but none matched your current request. Use weaviate.ask_knowledge to discover their actions.\n")
		for _, name := range discoverablePlugins {
			if aliases := o.registry.AliasesFor(name); len(aliases) > 0 {
				for _, alias := range aliases {
					fmt.Fprintf(&sb, "- %s\n", alias)
				}
			} else {
				fmt.Fprintf(&sb, "- %s\n", name)
			}
		}
		sb.WriteString("\n")
	}

	if o.subprocessConfig.Enabled {
		sb.WriteString(prompts.OrchestratorSubprocess)
	}

	workflowMemories, _ := o.memory.MemoriesForContext(ctx, "workflow")
	if len(workflowMemories) > 0 {
		sb.WriteString("## Relevant past workflows\n")
		const maxWorkflows = 5
		count := 0
		for _, m := range workflowMemories {
			if count >= maxWorkflows {
				break
			}
			// Skip garbage workflow entries with very short triggers
			// (e.g. "trigger: ?", "trigger: ,") that waste tokens.
			if idx := strings.Index(m.Content, "trigger: "); idx >= 0 {
				trigger := m.Content[idx+9:]
				if nl := strings.Index(trigger, "\n"); nl >= 0 {
					trigger = trigger[:nl]
				}
				if len(strings.TrimSpace(trigger)) < 5 {
					continue
				}
			}
			sb.WriteString(m.Content)
			sb.WriteString("\n")
			count++
		}
		sb.WriteString("\n")
	}

	if hint := channelFormatHint(ctx); hint != "" && !skipFormatHintFromContext(ctx) {
		sb.WriteString("## OUTPUT FORMAT\n")
		sb.WriteString(hint)
		sb.WriteString("\n")
	}

	if p := profile.FromContext(ctx); p != nil && p.Language != "" {
		sb.WriteString("## Language\n")
		fmt.Fprintf(&sb, "IMPORTANT: You MUST respond in %s. All your replies, explanations, and summaries must be written in %s.\n\n", p.Language, p.Language)
	}

	return sb.String()
}

// sessionDescriptor returns a 1-2 line string describing the caller's current
// channel and conversation for injection into the system prompt. Returns an
// empty string when neither is known (e.g. in unit tests) so the block can
// be safely omitted rather than rendering an empty "## Current session".
func sessionDescriptor(ctx context.Context) string {
	channelID := ""
	if p := profile.FromContext(ctx); p != nil && p.ChannelID != "" {
		channelID = p.ChannelID
	} else if a := actor.Actor(ctx); a != "" {
		// Classic mode: actor is "channel:sender". Take the channel prefix.
		if i := strings.IndexByte(a, ':'); i > 0 {
			channelID = a[:i]
		}
	}
	conversationID := actor.ConversationID(ctx)
	if channelID == "" && conversationID == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You are currently on")
	if channelID != "" {
		fmt.Fprintf(&sb, " channel `%s`", channelID)
	}
	if conversationID != "" {
		if channelID != "" {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, " conversation `%s`", conversationID)
	}
	sb.WriteString(".\n")
	return sb.String()
}

// channelFormatHint returns a formatting instruction string for the LLM based
// on the ResponseFormat declared in the channel capabilities. If the channel
// provides a custom ResponseFormatPrompt it takes precedence over the built-in
// hints. Returns empty string when no format is configured.
func channelFormatHint(ctx context.Context) string {
	caps := pkgchannel.CapabilitiesFromContext(ctx)
	if caps.ResponseFormatPrompt != "" {
		return caps.ResponseFormatPrompt
	}
	switch caps.ResponseFormat {
	case pkgchannel.FormatSlack:
		return prompts.FormatSlack
	case pkgchannel.FormatMarkdown:
		return prompts.FormatMarkdown
	case pkgchannel.FormatHTML:
		return prompts.FormatHTML
	case pkgchannel.FormatTelegram:
		return prompts.FormatTelegram
	case pkgchannel.FormatTeams:
		return prompts.FormatTeams
	case pkgchannel.FormatWhatsApp:
		return prompts.FormatWhatsApp
	case pkgchannel.FormatDiscord:
		return prompts.FormatDiscord
	case pkgchannel.FormatText:
		return prompts.FormatText
	default:
		return ""
	}
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
			slog.Warn("invoke step missing plugin or action", "step", i+1)
			continue
		}
		if !o.registry.HasAction(step.Plugin, step.Action) {
			slog.Warn("invoke step unknown action", "step", i+1, "plugin", step.Plugin, "action", step.Action)
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
				Metadata: map[string]string{
					"type":   "system",
					"action": step.Action,
				},
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
		Metadata: map[string]string{
			"type":   "system",
			"action": steps[len(steps)-1].Action,
		},
	}, nil
}

// rejectUnknownArgs returns a non-nil error listing args in call.Args that
// are not declared as Parameters on the action. The error message also lists
// the allowed parameter names so the LLM can self-correct on the next turn.
func rejectUnknownArgs(call ToolCall, action *Action) error {
	allowed := make(map[string]struct{}, len(action.Parameters))
	for _, p := range action.Parameters {
		allowed[p.Name] = struct{}{}
	}
	var unknown []string
	for k := range call.Args {
		if _, ok := allowed[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	allowedList := make([]string, 0, len(allowed))
	for k := range allowed {
		allowedList = append(allowedList, k)
	}
	sort.Strings(allowedList)
	allowedStr := "(none)"
	if len(allowedList) > 0 {
		allowedStr = strings.Join(allowedList, ", ")
	}
	return fmt.Errorf("unknown argument(s) for %s.%s: %s; allowed: %s",
		call.Plugin, call.Action, strings.Join(unknown, ", "), allowedStr)
}

func (o *Orchestrator) executeCall(ctx context.Context, call ToolCall) ToolResult {
	// Emit tool_call_extracted at the very top for LLM-originated calls
	// so the raw decoded view is captured before any validation,
	// normalization, policy gating, or context-arg injection mutates it
	// (raw-capture rule). Internal calls (preparers, guards, pipelines)
	// are host-orchestrated, not part of the LLM reasoning trace, and are
	// not emitted. CallID joins the matching tool_call_result emitted
	// further down; tool_call_extracted's event id is also stamped onto
	// ctx via WithParent so the eventual tool_call_result and any
	// refusal/not-found emits in between link back via parent_id.
	//
	// dispatchStart is initialized unconditionally so emitRefusalResult
	// can compute a meaningful LatencyMS regardless of where the policy
	// gate fires. Even non-FromLLM calls pay one time.Now() — cheap and
	// keeps the contract simple.
	dispatchStart := time.Now()
	if call.FromLLM {
		mode := emit.ToolCallModeText
		if o.supportsNativeTools() {
			mode = emit.ToolCallModeNative
		}
		// Arguments captured as the raw map ref; send() in the emit
		// package marshals payload synchronously, so the JSON snapshot is
		// frozen here BEFORE the context-arg injection below mutates
		// call.Args. If this emit ever becomes async, the call.Args map
		// must be copied first to preserve the raw-capture invariant.
		extractedID := emit.EmitToolCallExtracted(ctx, o.eventSink, emit.ToolCallExtractedArgs{
			CallID:    call.ID,
			Plugin:    call.Plugin,
			Action:    call.Action,
			Arguments: call.Args,
			Mode:      mode,
		})
		if extractedID != "" {
			ctx = emit.WithParent(ctx, extractedID)
		}
	}

	if call.Plugin == "" {
		o.emitToolCallNotFound(ctx, call)
		return ToolResult{
			CallID: call.ID,
			Error:  `tool call format not recognized. You MUST use this exact format: [tool_call] {"tool": "plugin.action", "args": {"key": "value"}} [/tool_call]. Do NOT use XML or any other format. Retry the same action now using the correct format.`,
		}
	}
	exec, ok := o.registry.GetExecutor(call.Plugin)
	if !ok {
		o.emitToolCallNotFound(ctx, call)
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("plugin %q not found", call.Plugin),
		}
	}
	if !o.registry.HasAction(call.Plugin, call.Action) {
		// LLMs frequently mangle action names in two ways:
		// 1. Underscores instead of hyphens: "list_persons" → "list-persons"
		// 2. Dropping plugin prefix: "list-items" → "plugin__list-items"
		// Try each normalization and their combination before giving up.
		resolved := false
		candidates := []string{
			strings.ReplaceAll(call.Action, "_", "-"),
			call.Plugin + "__" + call.Action,
			call.Plugin + "__" + strings.ReplaceAll(call.Action, "_", "-"),
			strings.ReplaceAll(call.Action, "-", "_"),
			call.Plugin + "__" + strings.ReplaceAll(call.Action, "-", "_"),
		}
		for _, candidate := range candidates {
			if candidate != call.Action && o.registry.HasAction(call.Plugin, candidate) {
				call.Action = candidate
				resolved = true
				break
			}
		}
		if !resolved {
			o.emitToolCallNotFound(ctx, call)
			return ToolResult{
				CallID: call.ID,
				Error:  fmt.Sprintf("action %q not found in plugin %q", call.Action, call.Plugin),
			}
		}
	}

	actorID := actor.Actor(ctx)
	// permission_plugin gates LLM-originated plugin actions (including e.g.
	// install_skill); configure it for team deployments.  Internal calls
	// (guards, preparers, formatters, pipelines) are exempt — they are
	// constructed programmatically by the host, not by the LLM.
	if call.FromLLM && actorID != "" && o.permissionChecker != nil && call.Plugin != o.permissionPluginName {
		allowed, err := o.permissionChecker.Allowed(ctx, actorID, call.Plugin)
		if err != nil {
			slog.Warn("permission check failed", "actor", actorID, "plugin", call.Plugin, "error", err)
			return o.emitRefusalResult(ctx, call, "permission denied", dispatchStart)
		}
		if !allowed {
			return o.emitRefusalResult(ctx, call, "permission denied", dispatchStart)
		}
	}

	// Single lookup: get capability and find the action for context injection, audit logging, and UserOnly enforcement.
	var action *Action
	var capForCheck PluginCapability
	if cap, ok := o.registry.GetCapability(call.Plugin); ok {
		capForCheck = cap
		for i := range cap.Actions {
			if cap.Actions[i].Name == call.Action {
				action = &cap.Actions[i]
				break
			}
		}
	}

	// Defense-in-depth: block plugins that are not allowed for this profile.
	// In strict mode (WhoAmI plugins list) every plugin is checked; in non-strict
	// mode only capabilities with AllowedGroups set are gated (pluginAllowed handles both).
	// This mirrors buildSystemPrompt and protects against crafted tool calls.
	// Internal calls (guards, preparers, pipelines) are exempt: they are constructed
	// programmatically by the host, not by the LLM, so they are trusted. This matches
	// the exemptions already in place for UserOnly and rejectUnknownArgs below.
	if call.FromLLM {
		if allowed := o.resolveAllowedPlugins(ctx); !o.pluginAllowed(capForCheck, allowed) {
			slog.Warn("BLOCKED tool call for restricted plugin",
				"plugin", call.Plugin,
				"action", call.Action,
				"strict", allowed.strict,
				"allowed_groups", capForCheck.AllowedGroups,
				"in_allowlist", allowed.m[capForCheck.Name],
				"allowlist_keys", mapKeys(allowed.m),
			)
			return o.emitRefusalResult(ctx, call,
				fmt.Sprintf("plugin %q is not available for this profile", call.Plugin),
				dispatchStart)
		} else {
			slog.Debug("tool call allowed",
				"plugin", call.Plugin,
				"action", call.Action,
				"strict", allowed.strict,
				"in_allowlist", allowed.m[capForCheck.Name],
			)
		}
	}

	if call.FromLLM && action != nil && action.UserOnly {
		slog.Warn("BLOCKED LLM attempt to invoke user_only action", "actor", actorID, "plugin", call.Plugin, "action", call.Action, "args", call.Args)
		return o.emitRefusalResult(ctx, call,
			fmt.Sprintf("action %q can only be invoked by the user, not the LLM", call.Action),
			dispatchStart)
	}
	// Reject unknown args from LLM-originated calls. Without this, stray keys
	// (e.g. Haiku emitting `message=` at top level instead of inside `args`)
	// are silently dropped; the call reaches the plugin with empty args and
	// fails later with an error the LLM can't trace back to its own mistake.
	// Internal callers (pipelines, permission checks, context preparers) are
	// exempt — they construct calls programmatically and may legitimately use
	// names outside the declared Parameters set. InjectContextArgs are not
	// accepted from the LLM; the host injects them below.
	// rejectUnknownArgs is pre-dispatch validation: emit
	// tool_call_args_invalid (its own typed failure event) and return
	// early. Unlike the policy refusals further up, no tool_call_result
	// fires here — the dispatcher never ran, so there is no "result" to
	// record. Consumer-side analytics count
	//   extracted == not_found + args_invalid + result
	// for orthogonality.
	if call.FromLLM && action != nil && len(call.Args) > 0 {
		if err := rejectUnknownArgs(call, action); err != nil {
			slog.Warn("BLOCKED LLM call with unknown args", "plugin", call.Plugin, "action", call.Action, "error", err.Error())
			emit.EmitToolCallArgsInvalid(ctx, o.eventSink, emit.ToolCallArgsInvalidArgs{
				CallID:          call.ID,
				Plugin:          call.Plugin,
				Action:          call.Action,
				ValidationError: err.Error(),
			})
			return ToolResult{CallID: call.ID, Error: err.Error()}
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
			slog.Info("audit", "actor", actorID, "plugin", call.Plugin, "action", call.Action, "args", call.Args)
		}
	}

	result := o.guard.ExecuteWithTimeout(ctx, exec, call)
	result = o.guard.ValidateResult(call, result)
	result = o.guard.Sanitize(result)
	if call.FromLLM {
		status := "ok"
		respBody := result.Content
		if result.Error != "" {
			status = "error"
			respBody = result.Error
		}
		emit.EmitToolCallResult(ctx, o.eventSink, emit.ToolCallResultArgs{
			CallID:    call.ID,
			Status:    status,
			Response:  respBody,
			LatencyMS: time.Since(dispatchStart).Milliseconds(),
		})
	}
	return result
}

// emitToolCallNotFound emits a tool_call_not_found event for an LLM-originated
// call whose plugin or action could not be resolved. The requested-name format
// matches the LLM's own "plugin.action" naming convention so analytics can
// group mis-spellings without re-parsing.
func (o *Orchestrator) emitToolCallNotFound(ctx context.Context, call ToolCall) {
	if !call.FromLLM {
		return
	}
	emit.EmitToolCallNotFound(ctx, o.eventSink, call.Plugin+"."+call.Action)
}

// emitParseFailedIfApplicable emits one tool_call_parse_failed event when
// the response contains a structured tool-call marker but the parser
// produced no valid call. Narrated-placeholder is excluded — that path
// signals "LLM never tried the format", not a decode failure. An
// empty-plugin placeholder in calls indicates "parser saw a block but
// could not decode its body" (default parser's sawBlock fallback +
// bare-JSON-without-tool-key fallback both produce this signal). Mixed
// success (some valid calls + a discarded malformed block) does NOT
// trigger this — len(calls) > 0 with no empty-plugin entry is treated as
// success, accepting the v1 limitation noted at the call site.
func (o *Orchestrator) emitParseFailedIfApplicable(ctx context.Context, response string, calls []ToolCall) {
	if !containsToolCallMarker(response) {
		return
	}
	if IsNarratedPlaceholder(calls) {
		return
	}
	if calls != nil {
		hasEmptyPlugin := false
		for _, c := range calls {
			if c.Plugin == "" {
				hasEmptyPlugin = true
				break
			}
		}
		if !hasEmptyPlugin {
			return
		}
	}
	emit.EmitToolCallParseFailed(ctx, o.eventSink, emit.ToolCallParseFailedArgs{
		RawSnippet: response,
		ParserUsed: "default",
		ParseError: "tool-call marker present but no valid tool call could be decoded",
	})
}

// emitRefusalResult emits a tool_call_result with status="error" for policy
// refusals (permission denied, restricted plugin, user-only action) and
// returns the matching ToolResult. The call was extracted and identified
// before the refusal, so tool_call_result — not tool_call_not_found — is
// the right shape; the error message is captured verbatim in response_excerpt
// for downstream attribution.
func (o *Orchestrator) emitRefusalResult(ctx context.Context, call ToolCall, errMsg string, dispatchStart time.Time) ToolResult {
	if call.FromLLM {
		emit.EmitToolCallResult(ctx, o.eventSink, emit.ToolCallResultArgs{
			CallID:    call.ID,
			Status:    "error",
			Response:  errMsg,
			LatencyMS: time.Since(dispatchStart).Milliseconds(),
		})
	}
	return ToolResult{CallID: call.ID, Error: errMsg}
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
	// This fires from a background goroutine started in Run with
	// context.Background(), so actor.SessionID(ctx) would resolve to "" and
	// session_events writes would fail validation. Wrap the session id back
	// onto ctx here so the emit helpers can read it via the standard slot.
	ctx = actor.WithSessionID(ctx, sessionID)
	keep := o.maxMessagesAfterSummary
	if keep > len(sess.Messages) {
		keep = len(sess.Messages)
	}
	toSummarize := sess.Messages[:len(sess.Messages)-keep]
	keepMessages := sess.Messages[len(sess.Messages)-keep:]
	emit.EmitSummarizationTriggered(ctx, o.eventSink, emit.SummarizationTriggeredArgs{
		MessageCount: len(sess.Messages),
		Reason:       "threshold_reached",
	})

	var sysPrompt, userContent string
	if sess.Summary != "" {
		sysPrompt = o.summarizeUpdatePrompt
		if sysPrompt == "" {
			sysPrompt = prompts.SummarizeUpdate
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
			sysPrompt = prompts.SummarizeDefault
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
	summarizeStart := time.Now()
	resp, err := o.llm.Complete(ctx, req)
	if err != nil {
		slog.Warn("session summarization failed", "error", err)
		// No completed event on LLM failure: triggered-without-completed
		// is the analytics signal for "summarization started but did not
		// finish". Same pattern as other failure-mode events in this file.
		return
	}
	newSummary := strings.TrimSpace(resp.Content)
	if newSummary == "" {
		return
	}
	if err := o.sessions.SetSummary(sessionID, newSummary, keepMessages); err != nil {
		// Same "triggered without completed" failure signal as the LLM
		// error path above. SessionStore.SetSummary only errors on
		// "session not found", which is already excluded by the earlier
		// Get() check; reaching this branch would require the session
		// being deleted concurrently between Get and SetSummary. Treated
		// as a defensive return for that race, no dedicated test.
		slog.Warn("set session summary failed", "error", err)
		return
	}
	emit.EmitSummarizationCompleted(ctx, o.eventSink, emit.SummarizationCompletedArgs{
		Summary:      newSummary,
		KeptMessages: len(keepMessages),
		LatencyMS:    time.Since(summarizeStart).Milliseconds(),
	})
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

// nativeToolContent returns the content string for a role=tool message in
// the native tool-calling format.  When the plugin returned an error the
// error text is used so the LLM can read and react to it instead of seeing
// an empty string and hallucinating a response.  When structured content is
// available it is appended so the LLM has the authoritative payload.
func nativeToolContent(r ToolResult) string {
	if r.Error != "" {
		return "error: " + r.Error
	}
	if r.StructuredContent != "" {
		return r.Content + "\n\n[structured]\n" + r.StructuredContent + "\n[/structured]"
	}
	return r.Content
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

// pendingToolCallMeta is the JSON-serializable form of a pending tool call
// stored in session metadata so it survives restarts.
//
// ConfirmationRequestedID carries the session-event id of the
// confirmation_requested event that started this pending state, so the
// matching confirmation_resolved event can be parented back to it even
// across pod restarts or multi-instance failover where the in-memory
// pendingConfirmationIDs map is empty.
type pendingToolCallMeta struct {
	ID                      string            `json:"id"`
	Plugin                  string            `json:"plugin"`
	Action                  string            `json:"action"`
	Args                    map[string]string `json:"args,omitempty"`
	ConfirmationRequestedID string            `json:"confirmation_requested_id,omitempty"`
}

func savePendingToolCall(sessions SessionStoreInterface, sessionID string, tc *ToolCall, confirmationRequestedID string) {
	meta := pendingToolCallMeta{
		ID:                      tc.ID,
		Plugin:                  tc.Plugin,
		Action:                  tc.Action,
		Args:                    tc.Args,
		ConfirmationRequestedID: confirmationRequestedID,
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return
	}
	_ = sessions.SetMetadata(sessionID, "pending_tool_call", string(data))
}

// loadPendingToolCall restores the pending tool call from session metadata.
// Returns the ToolCall and the confirmation_requested event id (empty when
// the persisted row predates the field or no event was captured).
func loadPendingToolCall(sessions SessionStoreInterface, sessionID string) (*ToolCall, string) {
	sess, err := sessions.Get(sessionID)
	if err != nil || sess == nil {
		return nil, ""
	}
	raw := sess.Metadata["pending_tool_call"]
	if raw == "" {
		return nil, ""
	}
	var meta pendingToolCallMeta
	if json.Unmarshal([]byte(raw), &meta) != nil {
		return nil, ""
	}
	return &ToolCall{ID: meta.ID, Plugin: meta.Plugin, Action: meta.Action, Args: meta.Args}, meta.ConfirmationRequestedID
}

// toolCallSignature builds a deterministic string from a set of tool calls
// for loop detection. Two rounds with the same signature are considered repeats.
func toolCallSignature(calls []ToolCall) string {
	var sb strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&sb, "%s.%s", c.Plugin, c.Action)
		// Sort keys for deterministic comparison (map iteration is random).
		keys := make([]string, 0, len(c.Args))
		for k := range c.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, "\x00%s=%s", k, c.Args[k])
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func formatToolResultMessage(result ToolResult) string {
	if result.Error != "" {
		return fmt.Sprintf("[tool_result] error: %s", result.Error)
	}
	return fmt.Sprintf("[tool_result] %s", result.Content)
}

// relevantToolsKey is the context key for tool filtering from preparer responses.
type relevantToolsKey struct{}

// relevantToolsResult wraps the tool list so we can distinguish
// "preparer not called" (nil wrapper) from "preparer returned empty" (non-nil, empty list).
type relevantToolsResult struct {
	tools []string
}

// withRelevantTools stores the relevant tool list from a preparer response in ctx.
// An empty slice means "preparer found nothing relevant" — the filter will exclude
// all tools. Pass nil to indicate no preparer ran (show all tools).
func withRelevantTools(ctx context.Context, tools []string) context.Context {
	return context.WithValue(ctx, relevantToolsKey{}, &relevantToolsResult{tools: tools})
}

// relevantToolsFromContext returns (tools, true) when a preparer set the list
// (even if empty), or (nil, false) when no preparer ran.
func relevantToolsFromContext(ctx context.Context) ([]string, bool) {
	r, ok := ctx.Value(relevantToolsKey{}).(*relevantToolsResult)
	if !ok || r == nil {
		return nil, false
	}
	return r.tools, true
}

type searchQueryKey struct{}

// withSearchQuery stores an enriched search query in the context so the RAG
// preparer can use it for semantic search while the original user message
// stays intact for the LLM.
func withSearchQuery(ctx context.Context, query string) context.Context {
	return context.WithValue(ctx, searchQueryKey{}, query)
}

// SearchQueryFromContext returns the enriched search query, or empty string if none.
// Exported so channel plugins (weaviate preparer) can access it.
func SearchQueryFromContext(ctx context.Context) string {
	v, _ := ctx.Value(searchQueryKey{}).(string)
	return v
}

// filterCapabilitiesByRelevantTools returns only the capabilities and actions
// that match the relevant tools list (format "plugin.action"). When set is
// false (no preparer ran), all capabilities are returned unchanged. When set
// is true but the list is empty, no actions pass the filter.
func filterCapabilitiesByRelevantTools(caps []PluginCapability, relevantTools []string, isSet bool) []PluginCapability {
	if !isSet {
		return caps
	}
	set := make(map[string]bool, len(relevantTools))
	for _, t := range relevantTools {
		set[t] = true
	}
	var filtered []PluginCapability
	for _, cap := range caps {
		var actions []Action
		for _, a := range cap.Actions {
			if set[cap.Name+"."+a.Name] {
				actions = append(actions, a)
			}
		}
		if len(actions) > 0 || cap.SystemPromptAddition != "" {
			fc := cap
			fc.Actions = actions
			filtered = append(filtered, fc)
		}
	}
	return filtered
}

// allowedPluginsKey is the context key for the per-Run allowed-plugin cache.
type allowedPluginsKey struct{}

// cachedAllowedPlugins wraps the plugin allowlist for a single Run call.
// m == nil means "unrestricted" (no profile, no lookup configured, or a DB
// failure that fails open). strict is true when the list came directly from
// Profile.Plugins (WhoAmI per-request allowlist); in that mode every plugin
// is gated, not just those with AllowedGroups on their capability.
//
// The zero value (m == nil, strict == false) deliberately doubles as both
// "cache miss" and "no restrictions" — the ctx type-assertion in
// resolveAllowedPlugins uses the ok bool to distinguish a real cache hit from
// the zero value, so the two meanings never collide in practice.
type cachedAllowedPlugins struct {
	m      map[string]bool
	strict bool
}

func withAllowedPlugins(ctx context.Context, c cachedAllowedPlugins) context.Context {
	return context.WithValue(ctx, allowedPluginsKey{}, c)
}

// skipFormatHintKey suppresses the OUTPUT FORMAT section in the system prompt
// so the HTML format hint doesn't interfere with tool-call generation.
type skipFormatHintKey struct{}

func withSkipFormatHint(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipFormatHintKey{}, true)
}

func skipFormatHintFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(skipFormatHintKey{}).(bool)
	return v
}

// expectedToolsKey stores the planner's expected tool steps in context so the
// agent loop can detect when the LLM fails to call tools that were expected.
type expectedToolsKey struct{}

func withExpectedTools(ctx context.Context, steps []*pipeline.Step) context.Context {
	return context.WithValue(ctx, expectedToolsKey{}, steps)
}

func expectedToolsFromContext(ctx context.Context) []*pipeline.Step {
	steps, _ := ctx.Value(expectedToolsKey{}).([]*pipeline.Step)
	return steps
}

// buildToolCallNudge creates a retry nudge message that includes a concrete
// tool call example derived from the planner's expected steps. Weak LLMs that
// ignore the generic "use [tool_call] format" instruction are much more likely
// to comply when they see the exact JSON they need to produce.
func buildToolCallNudge(steps []*pipeline.Step) string {
	// Find the first step with a concrete command to use as the example.
	for _, s := range steps {
		if s.Command == nil || s.Command.Plugin == "" || s.Command.Action == "" {
			continue
		}
		tool := s.Command.Plugin + "." + s.Command.Action
		// Marshal typed args directly — JSON preserves number/bool/array
		// shape exactly as the LLM should re-emit them in the [tool_call]
		// block. Stringification (used for the gRPC wire) would force the
		// LLM to read "2037838" and decide whether to quote it; better to
		// hand the JSON form straight back.
		args := s.Command.Args
		if args == nil {
			args = map[string]any{}
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			break
		}
		return "[system] You answered without calling a tool — your answer may be fabricated. " +
			"You MUST call the tool first, then answer with real data. " +
			"Output ONLY a [tool_call] block like this:\n\n" +
			"[tool_call]\n{\"tool\": \"" + tool + "\", \"args\": " + string(argsJSON) + "}\n[/tool_call]"
	}
	// Fallback: no concrete step available (sentinel "direct" step).
	return "[system] Do not describe what you will do. Execute the tool call NOW using the [tool_call] format shown in your instructions."
}

// plannerStepsToInvoke converts planner pipeline steps to InvokeSteps for
// server-side execution. Steps without a concrete command (e.g. sentinel
// "direct" steps) are skipped. Typed args are stringified at this boundary
// — InvokeStep is a wire-shaped value passed to the websocket / hint layer.
func plannerStepsToInvoke(steps []*pipeline.Step) []InvokeStep {
	var out []InvokeStep
	for _, s := range steps {
		if s.Command == nil || s.Command.Plugin == "" || s.Command.Action == "" {
			continue
		}
		out = append(out, InvokeStep{
			Plugin: s.Command.Plugin,
			Action: s.Command.Action,
			Args:   pipelineArgsToWire(s.Command.Args),
		})
	}
	return out
}

// resolveAllowedPlugins returns the set of plugin IDs allowed for the current profile.
// Returns a zero cachedAllowedPlugins (m == nil, strict == false) when no profile is set
// or no group plugin lookup is configured (= no restrictions).
// The result is cached in ctx by Run; within a single Run call this never hits the DB twice.
//
// Priority:
//  1. Profile.Plugins non-nil → strict allowlist from WhoAmI (per-request, every plugin gated)
//  2. Group DB lookup → non-strict (only capabilities with AllowedGroups are gated)
func (o *Orchestrator) resolveAllowedPlugins(ctx context.Context) cachedAllowedPlugins {
	if cached, ok := ctx.Value(allowedPluginsKey{}).(cachedAllowedPlugins); ok {
		return cached
	}
	p := profile.FromContext(ctx)
	if p == nil {
		return cachedAllowedPlugins{}
	}
	// WhoAmI returned an explicit plugin list — use it as a strict per-request allowlist.
	if p.Plugins != nil {
		m := make(map[string]bool, len(p.Plugins))
		for _, id := range p.Plugins {
			m[id] = true
		}
		return cachedAllowedPlugins{m: m, strict: true}
	}
	// Fall back to group-based DB lookup (non-strict: only AllowedGroups-gated capabilities are affected).
	if o.groupPluginLookup == nil {
		return cachedAllowedPlugins{}
	}
	if p.Group == "" {
		// Profile is present but has no group: deny all group-restricted plugins.
		return cachedAllowedPlugins{m: map[string]bool{}}
	}
	ids, err := o.groupPluginLookup.PluginsForGroup(ctx, p.Group)
	if err != nil {
		slog.Warn("group plugin lookup failed", "group", p.Group, "error", err)
		// Fail open: return unrestricted (m == nil) so the bot stays usable during
		// DB outages. This means a lookup failure silently grants access to all
		// AllowedGroups-gated plugins for this request. Strict mode (WhoAmI) never
		// reaches here, so that path is unaffected. Chosen deliberately over
		// fail-closed because bricking the bot during incidents is worse than a
		// temporary permission widening on group-gated plugins.
		return cachedAllowedPlugins{}
	}
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return cachedAllowedPlugins{m: m}
}

// pluginAllowed reports whether a capability is accessible for the current profile.
//
// Strict mode (Profile.Plugins from WhoAmI): every plugin is checked against the
// allowlist — a plugin absent from the list is blocked regardless of AllowedGroups.
//
// Non-strict mode (group DB lookup): only capabilities that have AllowedGroups set
// are gated; capabilities without AllowedGroups remain publicly visible.
func (o *Orchestrator) pluginAllowed(cap PluginCapability, allowed cachedAllowedPlugins) bool {
	if allowed.m == nil {
		// No profile / no lookup configured — unrestricted.
		return true
	}
	if !allowed.strict && len(cap.AllowedGroups) == 0 {
		// Non-strict mode: capability has no group restriction — always visible.
		return true
	}
	if allowed.m[cap.Name] {
		return true
	}
	// Check aliases: if the capability is an alias target (e.g. "mcp") and any
	// of its aliases (e.g. "jira", "appsignal") are in the allowlist, allow it.
	// This shouldn't normally fire because ListCapabilities already replaces
	// alias targets with per-alias entries, but it's defense-in-depth.
	for _, alias := range o.registry.AliasesFor(cap.Name) {
		if allowed.m[alias] {
			return true
		}
	}
	return false
}

// mapKeys returns the keys of a map as a sorted slice (for debug logging).
func mapKeys(m map[string]bool) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
// appendStrippingAllKC appends session messages to dst, stripping
// [knowledge_context] blocks from every user message. Server instructions
// are already in the system prompt via SystemPromptAddition, so the
// preparer-injected knowledge_context is redundant. Stripping all turns
// prevents both historical accumulation and current-turn duplication.
//
// Also deduplicates knowledge article plugin outputs: when ask_knowledge
// returns the same MCP server instructions block repeatedly (~5KB each),
// subsequent occurrences are replaced with a short stub to save context.
func appendStrippingAllKC(dst []provider.Message, msgs []provider.Message) []provider.Message {
	seenKnowledge := make(map[uint64]bool)
	seenToolResults := make(map[uint64]bool)
	for _, m := range msgs {
		if m.Role == provider.RoleUser {
			stripped := stripKnowledgeContext(m.Content)
			if stripped != m.Content {
				m = provider.Message{Role: m.Role, Content: stripped, Files: m.Files}
			}
			// Deduplicate knowledge article plugin outputs.
			m = deduplicateKnowledgeOutput(m, seenKnowledge)
			// Deduplicate large repeated tool results (>1KB).
			m = deduplicateLargeToolResult(m, seenToolResults)
		}
		dst = append(dst, m)
	}
	return dst
}

// deduplicateLargeToolResult replaces repeated large plugin output blocks
// (>1KB) with a stub. This catches duplicate tool results when the same
// tool is called multiple times with the same result (e.g. list-items
// called twice in a conversation).
func deduplicateLargeToolResult(m provider.Message, seen map[uint64]bool) provider.Message {
	const openTag = "[plugin_output]"
	const closeTag = "[/plugin_output]"
	const minSize = 1024 // only deduplicate blocks >1KB
	if !strings.Contains(m.Content, openTag) {
		return m
	}
	start := strings.Index(m.Content, openTag)
	end := strings.LastIndex(m.Content, closeTag)
	if start < 0 || end < 0 || end <= start {
		return m
	}
	body := m.Content[start+len(openTag) : end]
	if len(body) < minSize {
		return m // too small to bother deduplicating
	}
	h := fnv64a(body)
	if !seen[h] {
		seen[h] = true
		return m
	}
	replaced := m.Content[:start] +
		"[plugin_output]\n(Same tool result as a previous call — see above.)\n[/plugin_output]" +
		m.Content[end+len(closeTag):]
	return provider.Message{Role: m.Role, Content: strings.TrimSpace(replaced), Files: m.Files}
}

// deduplicateKnowledgeOutput replaces repeated knowledge article plugin
// outputs with a short stub. Each unique knowledge block is kept on first
// occurrence; subsequent identical blocks are replaced to save context.
func deduplicateKnowledgeOutput(m provider.Message, seen map[uint64]bool) provider.Message {
	const marker = "## Knowledge Articles"
	const openTag = "[plugin_output]"
	const closeTag = "[/plugin_output]"
	if !strings.Contains(m.Content, marker) {
		return m
	}
	// Extract the knowledge section between [plugin_output] tags.
	start := strings.Index(m.Content, openTag)
	end := strings.LastIndex(m.Content, closeTag)
	if start < 0 || end < 0 || end <= start {
		return m
	}
	body := m.Content[start+len(openTag) : end]
	h := fnv64a(body)
	if !seen[h] {
		seen[h] = true
		return m
	}
	// Replace duplicate knowledge output with a short stub.
	replaced := m.Content[:start] +
		"[plugin_output]\n(Same knowledge articles as a previous lookup — see above.)\n[/plugin_output]" +
		m.Content[end+len(closeTag):]
	return provider.Message{Role: m.Role, Content: strings.TrimSpace(replaced), Files: m.Files}
}

// lastUserMessage returns the most recent user message from the session,
// stripping knowledge context. Used to enrich short follow-up messages
// (e.g. "Item") with the prior intent ("Create some arbitrary test object")
// so RAG semantic search matches the right tools.
func lastUserMessage(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleUser {
			text := stripKnowledgeContext(messages[i].Content)
			if text != "" {
				return text
			}
		}
	}
	return ""
}

func lastAssistantMessage(messages []provider.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == provider.RoleAssistant && messages[i].Content != "" {
			return messages[i].Content
		}
	}
	return ""
}

// buildPlannerConversationContext extracts a compact summary of recent
// conversation turns so the planner can interpret follow-up messages.
// Only includes user and assistant text messages (no tool results or system
// messages) and caps at the last 6 turns to keep the planner prompt small.
func buildPlannerConversationContext(sess *state.Session) string {
	if sess == nil || len(sess.Messages) == 0 {
		return ""
	}
	var parts []string
	// Walk backwards to find the last few user/assistant exchanges.
	for i := len(sess.Messages) - 1; i >= 0 && len(parts) < 6; i-- {
		m := sess.Messages[i]
		if m.Role != provider.RoleUser && m.Role != provider.RoleAssistant {
			continue
		}
		text := m.Content
		if text == "" {
			continue
		}
		// Strip knowledge context blocks — they're huge and not useful for the planner.
		text = stripKnowledgeContext(text)
		if text == "" {
			continue
		}
		// Truncate long messages to keep context compact.
		if len(text) > 300 {
			text = text[:300] + "..."
		}
		prefix := "User"
		if m.Role == provider.RoleAssistant {
			prefix = "Assistant"
		}
		parts = append(parts, prefix+": "+text)
	}
	if len(parts) == 0 {
		return ""
	}
	// Reverse to chronological order.
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return "Previous conversation:\n" + strings.Join(parts, "\n")
}

// fnv64a computes a fast FNV-1a hash for deduplication.
func fnv64a(s string) uint64 {
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// stripKnowledgeContext removes [knowledge_context]...[/knowledge_context] blocks
// from a message. Used to strip historical knowledge context and planner input.
func stripKnowledgeContext(s string) string {
	const open, close = "[knowledge_context]", "[/knowledge_context]"
	start := strings.Index(s, open)
	if start < 0 {
		return s
	}
	end := strings.Index(s, close)
	if end < 0 {
		return s
	}
	return strings.TrimSpace(s[:start] + s[end+len(close):])
}

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
		result[i] = pipeline.CapabilityInfo{Name: cap.Name, Description: cap.Description, Actions: actions, SystemPromptAddition: cap.SystemPromptAddition}
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

// syncActionEntry mirrors the weaviate-plugin's expected JSON shape for one action.
type syncActionEntry struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  []Parameter `json:"parameters"`
}

// syncKnowledgeArticleEntry mirrors the weaviate-plugin's expected JSON shape
// for one knowledge article. ID, Title and Content are required; Tags is
// optional and will be merged with the plugin-augmented tag set on storage.
type syncKnowledgeArticleEntry struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

// syncActionsPayload is the JSON shape expected by the sync_actions action.
//
// ServerInstructions carries the plugin's SystemPromptAddition (e.g. an MCP
// server's `initialize.instructions` text bridged through by mcp-plugin) so the
// vector store can index it for semantic retrieval alongside actions.
//
// KeepPlugins, when populated, is the authoritative list of plugin names that
// should remain in the vector store. Sync-plugin implementations are expected
// to delete any indexed records whose plugin is NOT in this list (orphan prune).
// It's set during full startup syncs (SyncActions) and omitted during late-load
// single-plugin syncs (SyncPluginActions). The prune is idempotent across the
// per-plugin calls of one full sync, so plugins can apply it on every call.
// Older sync-plugin versions that don't recognize the field MUST ignore it —
// the upsert path is unaffected.
type syncActionsPayload struct {
	PluginName         string            `json:"plugin_name"`
	Actions            []syncActionEntry `json:"actions"`
	ServerInstructions string            `json:"server_instructions,omitempty"`
	// KnowledgeArticles ships per-section knowledge contributed by the plugin
	// (e.g. an MCP server's `initialize._meta.knowledge_articles`). The sync
	// plugin stores each as a KnowledgeArticles record with source
	// "mcp-knowledge:<plugin>:<id>" so the prepare-path RAG can pull just the
	// relevant section into [knowledge_context], instead of the full prose
	// being injected via SystemPromptAddition into every system prompt.
	// Optional; older sync plugins that don't recognize the field MUST ignore
	// it — the upsert path is unaffected.
	KnowledgeArticles []syncKnowledgeArticleEntry `json:"knowledge_articles,omitempty"`
	KeepPlugins       []string                    `json:"keep_plugins,omitempty"`
	// Hash is a SHA-256 digest of the plugin's actions + server_instructions
	// + knowledge_articles. When the sync plugin receives the same hash on
	// consecutive calls for a given plugin_name, it can skip the upsert (the
	// data hasn't changed). Computed by the orchestrator; older sync plugins
	// ignore unknown fields.
	Hash string `json:"hash,omitempty"`
	// IsContinuationBatch tells the sync plugin to skip the per-plugin
	// pre-delete on this call. Set on batches 1..N of a chunked plugin sync
	// so each subsequent batch only inserts and does not wipe what batch 0
	// (and earlier continuation batches) already persisted. Older sync-plugin
	// versions that don't recognize the field MUST ignore it — they will
	// continue to pre-delete on every batch and exhibit the legacy
	// last-batch-wins truncation behaviour.
	IsContinuationBatch bool `json:"is_continuation_batch,omitempty"`
}

// SyncActions iterates all registered plugin capabilities and calls the configured
// sync_actions plugin to upsert them into the vector store. This enables retrieval-based
// tool filtering by the RAG preparer. Safe to call at startup; errors are logged, not returned.
func (o *Orchestrator) SyncActions(ctx context.Context) {
	if o.syncActionsPlugin == "" || o.syncActionsAction == "" {
		return
	}
	if !o.registry.HasAction(o.syncActionsPlugin, o.syncActionsAction) {
		slog.Warn("sync_actions target not found", "plugin", o.syncActionsPlugin, "action", o.syncActionsAction)
		return
	}

	slog.Info("sync_actions starting", "component", "orchestrator",
		"sync_plugin", o.syncActionsPlugin, "sync_action", o.syncActionsAction)

	caps := o.registry.ListCapabilities()
	// Build the authoritative live-plugin list once, excluding the sync plugin
	// itself. Each per-plugin sync_actions call ships this list as keep_plugins
	// so the sync plugin can prune records for plugins that have been removed
	// since the last startup. Older sync-plugin versions ignore the field —
	// upserts still work, orphans just linger (the previous behavior).
	keep := make([]string, 0, len(caps))
	for _, cap := range caps {
		if cap.Name == o.syncActionsPlugin {
			continue
		}
		keep = append(keep, cap.Name)
	}

	var synced, failed int
	for _, cap := range caps {
		if err := o.syncPluginCapability(ctx, cap, keep); err != nil {
			slog.Warn("sync_actions failed", "component", "orchestrator", "plugin", cap.Name, "error", err)
			failed++
		} else {
			synced++
		}
	}

	slog.Info("sync_actions completed", "component", "orchestrator",
		"plugins_synced", synced, "plugins_failed", failed)
}

// SyncPluginActions syncs a single plugin's capabilities to the vector store.
// Intended for use when a plugin comes online after initial startup (e.g. via retry).
func (o *Orchestrator) SyncPluginActions(ctx context.Context, pluginName string) {
	if o.syncActionsPlugin == "" || o.syncActionsAction == "" {
		return
	}
	cap, ok := o.registry.GetCapability(pluginName)
	if !ok {
		slog.Warn("sync_actions: plugin not found in registry", "component", "orchestrator", "plugin", pluginName)
		return
	}
	slog.Info("sync_actions: syncing late-loaded plugin", "component", "orchestrator", "plugin", pluginName)
	// Late-load: omit keep_plugins so the sync plugin only upserts this one
	// capability and does not interpret the call as a full-snapshot prune.
	if err := o.syncPluginCapability(ctx, cap, nil); err != nil {
		slog.Warn("sync_actions failed for late-loaded plugin", "component", "orchestrator", "plugin", pluginName, "error", err)
	}
}

// syncPluginCapability syncs a single plugin's actions to the vector store.
// keepPlugins is the authoritative live-plugin set during a full startup sync,
// or nil during late-load single-plugin re-sync.
func (o *Orchestrator) syncPluginCapability(ctx context.Context, cap PluginCapability, keepPlugins []string) error {
	// Skip all actions from the sync plugin so it doesn't index its own
	// internal actions (sync_actions, ingest, etc.) into the vector store.
	if cap.Name == o.syncActionsPlugin {
		return nil
	}
	entries := make([]syncActionEntry, 0, len(cap.Actions))
	for _, a := range cap.Actions {
		if a.UserOnly {
			continue
		}
		entries = append(entries, syncActionEntry{
			Name:        a.Name,
			Description: a.Description,
			Parameters:  a.Parameters,
		})
	}
	knowledge := make([]syncKnowledgeArticleEntry, 0, len(cap.KnowledgeArticles))
	for _, ka := range cap.KnowledgeArticles {
		if ka.ID == "" || ka.Title == "" || ka.Content == "" {
			continue
		}
		knowledge = append(knowledge, syncKnowledgeArticleEntry(ka))
	}

	if len(entries) == 0 && cap.SystemPromptAddition == "" && len(knowledge) == 0 {
		return nil
	}

	exec, ok := o.registry.GetExecutor(o.syncActionsPlugin)
	if !ok {
		return fmt.Errorf("executor disappeared")
	}

	// Compute a deterministic hash over the full set of actions + server
	// instructions + knowledge articles. The sync plugin stores this per
	// plugin and skips the upsert when unchanged — avoids redundant Weaviate
	// writes on restart.
	hash := capabilityHash(entries, cap.SystemPromptAddition, knowledge)

	// Batch actions into chunks to avoid oversized payloads that time out
	// on the vector store side. Instructions and keep_plugins go on the
	// first batch only; subsequent batches are pure action upserts.
	const batchSize = 10
	batches := chunkActions(entries, batchSize)
	if len(batches) == 0 {
		// No actions but we have server instructions — send a single empty batch.
		batches = [][]syncActionEntry{{}}
	}

	for i, batch := range batches {
		payload := syncActionsPayload{
			PluginName:          cap.Name,
			Actions:             batch,
			Hash:                hash,
			IsContinuationBatch: i > 0,
		}
		if i == 0 {
			// Persist server instructions (e.g. MCP initialize.instructions) to
			// the vector store so they survive plugin restarts and are searchable
			// via search_instructions. The prepare action excludes mcp:-sourced
			// articles from [knowledge_context] to avoid duplication with the
			// system prompt's SystemPromptAddition.
			//
			// Knowledge articles take a different route: stored under
			// "mcp-knowledge:<plugin>:<id>" sources which are not filtered by
			// the prepare path, so each section can be retrieved on-demand
			// instead of injected into every system prompt.
			payload.ServerInstructions = cap.SystemPromptAddition
			payload.KnowledgeArticles = knowledge
			payload.KeepPlugins = keepPlugins
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal batch %d: %w", i, err)
		}
		call := ToolCall{
			ID:     fmt.Sprintf("sync-%s-%d", cap.Name, i),
			Plugin: o.syncActionsPlugin,
			Action: o.syncActionsAction,
			Args:   map[string]string{"payload": string(b)},
		}
		result := o.guard.ExecuteWithDeadline(ctx, exec, call, 2*time.Minute)
		if result.Error != "" {
			return fmt.Errorf("batch %d/%d: %s", i+1, len(batches), result.Error)
		}
	}
	slog.Info("sync_actions done", "component", "orchestrator", "plugin", cap.Name, "actions", len(entries), "batches", len(batches))
	return nil
}

// capabilityHash computes a SHA-256 digest over a plugin's actions, server
// instructions and knowledge articles. The sync plugin stores this hash and
// skips re-syncing when it matches — avoiding redundant Weaviate writes on
// restart.
func capabilityHash(entries []syncActionEntry, serverInstructions string, knowledge []syncKnowledgeArticleEntry) string {
	h := sha256.New()
	// Deterministic: entries are built in stable order from cap.Actions.
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s\n%s\n", e.Name, e.Description)
		if len(e.Parameters) > 0 {
			b, _ := json.Marshal(e.Parameters)
			h.Write(b)
		}
		h.Write([]byte{'\n'})
	}
	_, _ = fmt.Fprintf(h, "instructions:%s\n", serverInstructions)
	// Knowledge articles arrive in stable order from cap.KnowledgeArticles.
	for _, ka := range knowledge {
		_, _ = fmt.Fprintf(h, "knowledge:%s\n%s\n%s\n", ka.ID, ka.Title, ka.Content)
		if len(ka.Tags) > 0 {
			b, _ := json.Marshal(ka.Tags)
			h.Write(b)
		}
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// chunkActions splits a slice of syncActionEntry into batches of at most size n.
func chunkActions(entries []syncActionEntry, n int) [][]syncActionEntry {
	if n <= 0 {
		return [][]syncActionEntry{entries}
	}
	var batches [][]syncActionEntry
	for i := 0; i < len(entries); i += n {
		end := i + n
		if end > len(entries) {
			end = len(entries)
		}
		batches = append(batches, entries[i:end])
	}
	return batches
}

// ---------------------------------------------------------------------------
// Glossary sync
// ---------------------------------------------------------------------------

// syncGlossaryPayload is the JSON shape expected by the sync_glossary action.
type syncGlossaryPayload struct {
	GlossaryHash        string              `json:"glossary_hash"`
	Entries             []syncGlossaryEntry `json:"entries"`
	IsContinuationBatch bool                `json:"is_continuation_batch,omitempty"`
}

type syncGlossaryEntry struct {
	Term       string   `json:"term"`
	Definition string   `json:"definition"`
	Category   string   `json:"category,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Synonyms   []string `json:"synonyms,omitempty"`
}

// glossaryHash computes a SHA-256 digest over the full set of glossary entries.
func glossaryHash(entries []syncGlossaryEntry) string {
	h := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%s\n%s\n%s\n", e.Term, e.Definition, e.Category)
		for _, t := range e.Tags {
			_, _ = fmt.Fprintf(h, "tag:%s\n", t)
		}
		for _, s := range e.Synonyms {
			_, _ = fmt.Fprintf(h, "syn:%s\n", s)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SyncGlossary collects glossary entries from all registered plugins and syncs
// them to the vector store via the sync_glossary action. Entries arrive from MCP
// servers through the plugin capability proto. Safe to call at startup; errors
// are logged, not returned.
func (o *Orchestrator) SyncGlossary(ctx context.Context) {
	if o.syncActionsPlugin == "" || o.syncGlossaryAction == "" {
		return
	}
	if !o.registry.HasAction(o.syncActionsPlugin, o.syncGlossaryAction) {
		slog.Warn("sync_glossary target not found", "plugin", o.syncActionsPlugin, "action", o.syncGlossaryAction)
		return
	}

	// Collect glossary entries from all plugins.
	caps := o.registry.ListCapabilities()
	var allEntries []syncGlossaryEntry
	for _, cap := range caps {
		for _, g := range cap.Glossary {
			if g.Term == "" || g.Definition == "" {
				continue
			}
			allEntries = append(allEntries, syncGlossaryEntry(g))
		}
	}

	if len(allEntries) == 0 {
		slog.Info("sync_glossary: no entries from any plugin, skipping", "component", "orchestrator")
		return
	}

	slog.Info("sync_glossary starting", "component", "orchestrator",
		"entries", len(allEntries), "sync_plugin", o.syncActionsPlugin, "sync_action", o.syncGlossaryAction)

	exec, ok := o.registry.GetExecutor(o.syncActionsPlugin)
	if !ok {
		slog.Warn("sync_glossary: executor not found", "plugin", o.syncActionsPlugin)
		return
	}

	hash := glossaryHash(allEntries)

	// Batch glossary entries like actions to avoid oversized payloads.
	const batchSize = 50
	for i := 0; i < len(allEntries); i += batchSize {
		end := i + batchSize
		if end > len(allEntries) {
			end = len(allEntries)
		}
		payload := syncGlossaryPayload{
			GlossaryHash:        hash,
			Entries:             allEntries[i:end],
			IsContinuationBatch: i > 0,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			slog.Warn("sync_glossary: marshal failed", "error", err)
			return
		}
		call := ToolCall{
			ID:     fmt.Sprintf("sync-glossary-%d", i/batchSize),
			Plugin: o.syncActionsPlugin,
			Action: o.syncGlossaryAction,
			Args:   map[string]string{"payload": string(b)},
		}
		result := o.guard.ExecuteWithDeadline(ctx, exec, call, 2*time.Minute)
		if result.Error != "" {
			slog.Warn("sync_glossary: batch failed", "batch", i/batchSize, "error", result.Error)
			return
		}
	}

	slog.Info("sync_glossary completed", "component", "orchestrator", "entries", len(allEntries))
}

// IngestKnowledgeDir recursively scans the configured knowledge directory for .md
// files and ingests each one via the configured plugin action. The directory is
// optional — knowledge articles can also be submitted via the plugin's HTTP API.
// Whether re-runs produce duplicates depends on the ingest action (weaviate upserts
// by title, so re-runs are safe). Errors are logged, not returned.
func (o *Orchestrator) IngestKnowledgeDir(ctx context.Context) {
	k := o.knowledge
	if k.Plugin == "" || k.Action == "" {
		return
	}
	// Dir is optional — articles may arrive exclusively via HTTP API.
	if k.Dir == "" {
		return
	}
	if !o.registry.HasAction(k.Plugin, k.Action) {
		slog.Warn("knowledge ingest target not found", "plugin", k.Plugin, "action", k.Action)
		return
	}

	exec, ok := o.registry.GetExecutor(k.Plugin)
	if !ok {
		slog.Warn("knowledge ingest executor not found", "plugin", k.Plugin)
		return
	}

	const maxFileSize int64 = 10 << 20 // 10 MB

	var count int
	err := filepath.WalkDir(k.Dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			slog.Warn("knowledge file stat failed", "path", path, "error", err)
			return nil
		}
		if info.Size() > maxFileSize {
			slog.Warn("knowledge file too large, skipping", "path", path, "size_bytes", info.Size(), "max_bytes", maxFileSize)
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("knowledge file read failed", "path", path, "error", err)
			return nil
		}
		title := strings.TrimSuffix(d.Name(), ".md")
		call := ToolCall{
			ID:     fmt.Sprintf("knowledge-ingest-%s", title),
			Plugin: k.Plugin,
			Action: k.Action,
			Args: map[string]string{
				"title":   title,
				"content": string(data),
				"source":  "knowledge_dir",
			},
		}
		result := o.guard.ExecuteWithTimeout(ctx, exec, call)
		if result.Error != "" {
			slog.Warn("knowledge ingest failed", "file", d.Name(), "error", result.Error)
		} else {
			count++
		}
		return nil
	})
	if err != nil {
		slog.Warn("knowledge dir walk failed", "dir", k.Dir, "error", err)
	}
	if count > 0 {
		slog.Info("knowledge dir ingested", "dir", k.Dir, "files", count)
	}
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
