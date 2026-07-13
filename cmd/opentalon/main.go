package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/bootstrap"
	"github.com/opentalon/opentalon/internal/bundle"
	"github.com/opentalon/opentalon/internal/channel"
	"github.com/opentalon/opentalon/internal/commands"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/dedup"
	"github.com/opentalon/opentalon/internal/eventwebhook"
	"github.com/opentalon/opentalon/internal/health"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/metrics"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/plugin"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/redisclient"
	"github.com/opentalon/opentalon/internal/reminder"
	"github.com/opentalon/opentalon/internal/requestpkg"
	"github.com/opentalon/opentalon/internal/scheduler"
	"github.com/opentalon/opentalon/internal/sessionlock"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	"github.com/opentalon/opentalon/internal/synclock"
	"github.com/opentalon/opentalon/internal/version"
	chanpkg "github.com/opentalon/opentalon/pkg/channel"
)

// Compile-time assertion that the DB-backed SessionStore satisfies the
// orchestrator's optional InjectionStateStore interface. Lives here
// because neither the store package nor the orchestrator package can
// reference the other without a cycle — main.go is the wiring point
// where both are already imported, so it's the right place to pin the
// contract. RFC #249 Phase 3.
var _ orchestrator.InjectionStateStore = (*store.SessionStore)(nil)

// capabilityRefresher, capabilityRegistry and corpusSyncer are the slices of the
// plugin manager, tool registry and orchestrator that the capability-refresh
// poll needs. Declaring them as interfaces keeps refreshAllCapabilities unit
// testable with fakes.
type capabilityRefresher interface {
	List() []string
	RefreshCapabilities(ctx context.Context, name string) (orchestrator.PluginCapability, error)
}

type capabilityRegistry interface {
	UpdateCapability(name string, cap orchestrator.PluginCapability)
}

type corpusSyncer interface {
	SyncPluginActions(ctx context.Context, name string)
}

// refreshPluginTimeout bounds a single plugin's live capability re-fetch so one
// hung upstream can't starve the rest of a poll cycle. It caps only the refresh
// RPC; the corpus sync keeps the parent context (it has its own per-batch
// deadline) so a large but healthy re-vectorize is never cut short.
const refreshPluginTimeout = 90 * time.Second

// refreshAllCapabilities runs one capability-refresh cycle over every loaded
// plugin. See refreshOnePlugin for the per-plugin behaviour.
func refreshAllCapabilities(ctx context.Context, pm capabilityRefresher, reg capabilityRegistry, syncer corpusSyncer, locker synclock.Locker) {
	names := pm.List()
	slog.Info("refresh poll: cycle start", "component", "refresh", "plugins", len(names))
	for _, name := range names {
		refreshOnePlugin(ctx, name, pm, reg, syncer, locker)
	}
}

// refreshOnePlugin re-fetches one plugin's capabilities, updates the executable
// registry, and (leader-gated) re-syncs its corpus, so a changed tool
// description / server instruction / knowledge article on an upstream MCP server
// propagates without a pod restart. Plugins that don't support refresh report
// gRPC Unimplemented and are skipped.
//
// The cheap re-fetch + registry update runs on every pod (each keeps a fresh
// executable view); only the corpus write is leader-gated via TryAcquirePlugin
// so a cluster doesn't double-write.
func refreshOnePlugin(ctx context.Context, name string, pm capabilityRefresher, reg capabilityRegistry, syncer corpusSyncer, locker synclock.Locker) {
	// Bound the refresh RPC so one hung upstream can't stall the whole cycle.
	rctx, cancel := context.WithTimeout(ctx, refreshPluginTimeout)
	fresh, err := pm.RefreshCapabilities(rctx, name)
	cancel()
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			slog.Debug("refresh poll: plugin does not support refresh, skipping", "component", "refresh", "plugin", name)
			return
		}
		slog.Warn("refresh poll: refresh failed", "component", "refresh", "plugin", name, "error", err)
		return
	}
	reg.UpdateCapability(name, fresh)
	slog.Info("refresh poll: capabilities refreshed", "component", "refresh", "plugin", name,
		"actions", len(fresh.Actions), "knowledge", len(fresh.KnowledgeArticles))

	// Leader-gate the corpus write so a cluster doesn't double-write.
	acquired, lockErr := locker.TryAcquirePlugin(ctx, name)
	switch {
	case lockErr != nil:
		// Redis blip: proceed best-effort (the per-doc sync is idempotent), but we
		// do NOT own the lock — so we must not release it, or we'd delete the key
		// the actual holder owns.
		slog.Warn("refresh poll: sync lock errored, proceeding without it", "component", "refresh", "plugin", name, "error", lockErr)
	case !acquired:
		slog.Debug("refresh poll: corpus sync skipped (another pod is syncing)", "component", "refresh", "plugin", name)
		return
	default:
		// Release only the lock we actually acquired, even if the sync panics.
		defer locker.ReleasePlugin(ctx, name)
	}
	syncer.SyncPluginActions(ctx, name)
}

func main() {
	fmt.Fprintln(os.Stderr, "OpenTalon starting...")
	configPath := flag.String("config", "", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	cleanFlag := flag.String("clean", "", "clear cached bundles and exit (all, plugins, channels, skills, lua_plugins); requires -config")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get())
		os.Exit(0)
	}

	if *cleanFlag != "" {
		runClean(*configPath, *cleanFlag)
		return
	}

	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "Usage: opentalon -config <path>")
		fmt.Fprintln(os.Stderr, "  Run OpenTalon with the given config. Use config.example.yaml as a template.")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	absConfigPath, err := filepath.Abs(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving config path: %v\n", err)
		os.Exit(1)
	}
	cfg.State.DataDir = config.ResolveStateDataDir(cfg, absConfigPath)

	// Configure structured logging (stdout/stderr, level-filtered).
	logLevel := cfg.Log.Level
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		logLevel = env
	}
	logger.Setup(logLevel)

	// Start gRPC health probe server (always on).
	healthSrv := health.New(cfg.Health.Addr)
	go func() {
		if err := healthSrv.ListenAndServe(); err != nil {
			slog.Error("health server error", "error", err)
		}
	}()

	// Start Prometheus metrics server if enabled.
	var metricsCollector *metrics.Collector
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled {
		metricsCollector = metrics.New()
		addr := cfg.Metrics.Addr
		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsCollector.Handler())
		metricsSrv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			slog.Info("metrics server listening", "addr", addr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server error", "error", err)
			}
		}()
	}

	// Fetch remote bootstrap config (if configured) and merge into static config.
	// Remote entries are additive — static config wins on key conflicts.
	var bootstrapGroupPlugins map[string][]string
	if bsProvider := bootstrap.New(cfg.Bootstrap); bsProvider != nil {
		bsCtx, bsCancel := context.WithTimeout(context.Background(), 60*time.Second)
		remoteResp, err := bsProvider.Fetch(bsCtx)
		bsCancel()
		if err != nil {
			if cfg.Bootstrap.Required {
				fmt.Fprintf(os.Stderr, "Error fetching bootstrap config (required=true): %v\n", err)
				os.Exit(1)
			}
			slog.Warn("bootstrap fetch failed, proceeding with static config only", "error", err)
		} else {
			cfg = bootstrap.Merge(cfg, remoteResp)
			bootstrapGroupPlugins = remoteResp.GroupPlugins
			slog.Info("bootstrap config merged",
				"url", cfg.Bootstrap.URL,
				"channels", len(remoteResp.Channels),
				"plugins", len(remoteResp.Plugins),
				"group_plugins", len(remoteResp.GroupPlugins),
			)
		}
	}

	// State store is opened first because the LLM provider needs the
	// per-session debug-capture sink (an async writer over the state DB)
	// at construction time. Sessions and other state pieces are wired the
	// same way as before — just earlier in the bootstrap.
	dataDir := cfg.State.DataDir
	var memory orchestrator.MemoryStoreInterface
	var sessions orchestrator.SessionStoreInterface
	var groupPluginStore *store.GroupPluginStore
	var usageStore *store.UsageStore
	var entityStore *store.EntityStore
	var debugStore *store.DebugEventStore
	var debugWriter *store.DebugEventWriter
	var debugRetentionCancel context.CancelFunc
	var sessionEventStore *store.SessionEventStore
	var sessionEventWriter *store.SessionEventWriter
	var sessionEventsRetentionCancel context.CancelFunc
	var eventWebhookSink *eventwebhook.Sink
	// injectionStateStore persists load_tools sticky promotion (the
	// per-session KnownTools set) across turns — the DB-backed SessionStore
	// satisfies it, the in-memory fallback does not. Stays nil when the
	// state DB is unavailable; load_tools promotion is then best-effort and
	// does not survive past the request.
	var injectionStateStore orchestrator.InjectionStateStore
	if dataDir != "" || cfg.State.DB.Driver == "postgres" {
		db, err := store.Open(cfg.State.DB, dataDir)
		if err != nil {
			slog.Warn("state store open failed, using in-memory state", "error", err)
			memory, sessions = newInMemoryState()
		} else {
			defer func() { _ = db.Close() }()
			memory = store.NewMemoryStore(db)
			sessStore := store.NewSessionStore(db, cfg.State.Session.MaxMessages, cfg.State.Session.MaxIdleDays)
			if err := sessStore.PruneIdleSessions(); err != nil {
				slog.Warn("session prune failed", "error", err)
			}
			sessions = sessStore
			injectionStateStore = sessStore
			groupPluginStore = store.NewGroupPluginStore(db)
			usageStore = store.NewUsageStore(db)
			entityStore = store.NewEntityStore(db)
			debugStore = store.NewDebugEventStore(db)
			debugWriter = store.NewDebugEventWriter(debugStore)
			debugWriter.Start(context.Background())
			// Retention: explicit RetentionDisabled overrides the days field;
			// RetentionDays==0 means "use the default 30", not "disable" —
			// see DebugConfig docstring for the reasoning. retentionCtx is
			// cancelled in the shutdown sequence at the bottom of main(),
			// alongside dispatcher and writer teardown.
			var retention time.Duration
			if !cfg.State.Debug.RetentionDisabled {
				days := cfg.State.Debug.RetentionDays
				if days <= 0 {
					days = 30
				}
				retention = time.Duration(days) * 24 * time.Hour
			}
			var retentionCtx context.Context
			retentionCtx, debugRetentionCancel = context.WithCancel(context.Background())
			go store.RunDebugRetention(retentionCtx, debugStore, retention)

			// Structured session_events log: always-on (orchestrator emits
			// every turn / tool call / failure mode), separate retention
			// horizon from /debug because the analytics use case wants
			// longer history than the raw-HTTP-replay use case.
			sessionEventStore = store.NewSessionEventStore(db)
			sessionEventWriter = store.NewSessionEventWriter(sessionEventStore)
			sessionEventWriter.Start(context.Background())
			var sessionEventsRetention time.Duration
			if !cfg.State.SessionEvents.RetentionDisabled {
				days := cfg.State.SessionEvents.RetentionDays
				if days <= 0 {
					days = 90
				}
				sessionEventsRetention = time.Duration(days) * 24 * time.Hour
			}
			var sessionEventsRetentionCtx context.Context
			sessionEventsRetentionCtx, sessionEventsRetentionCancel = context.WithCancel(context.Background())
			go store.RunSessionEventsRetention(sessionEventsRetentionCtx, sessionEventStore, sessionEventsRetention)
			// Seed static group→plugin assignments from config (source="config"; does not overwrite whoami/admin).
			seedGroupPlugins(context.Background(), groupPluginStore, cfg.Profiles.Groups)
			// Seed group→plugin assignments from remote bootstrap response (source="bootstrap"; lower priority than "config", does not overwrite whoami/admin).
			seedBootstrapGroupPlugins(context.Background(), groupPluginStore, bootstrapGroupPlugins)
		}
	} else {
		memory, sessions = newInMemoryState()
	}

	// Build the resolver for raw-HTTP capture: trace_id is derived from the
	// session key and stamped onto ctx by orchestrator.Run; logger.IsSession-
	// Debug reflects whether the session has metadata["debug"]=true. When
	// debugStore is nil (no state DB configured) we still build a sink
	// adapter for completeness, but the resolver short-circuits anyway.
	// AlwaysCapture promotes capture from per-session opt-in to every session,
	// so the raw request/response going to the LLM endpoint is always on record.
	var debugSink provider.DebugEventSink
	var debugResolver provider.DebugContextResolver
	if debugWriter != nil {
		alwaysCapture := cfg.State.Debug.AlwaysCapture
		debugSink = &providerDebugSink{writer: debugWriter}
		debugResolver = func(ctx context.Context) (string, string, bool) {
			if !alwaysCapture && !logger.IsSessionDebug(ctx) {
				return "", "", false
			}
			return actor.SessionID(ctx), logger.TraceID(ctx), true
		}
	}

	// Build the structured session-event sink. Unlike debugSink (opt-in
	// via /debug), this one is always-on whenever the state store is
	// available. Wired into the LLM provider via buildProvider; future
	// PRs route it into the orchestrator subsystems too.
	var sessionSink emit.Sink = emit.NoOpSink{}
	if sessionEventWriter != nil {
		sessionSink = &sessionSinkAdapter{writer: sessionEventWriter}
	}

	// Optional out-of-process event webhook: tee the same event stream to a
	// configured HTTP consumer (a best-effort, low-latency push alongside the
	// durable writer + the api-plugin's since_seq pull). Built here, before
	// sessionSink is handed to both the provider and the orchestrator below,
	// so every producer delivers through the tee. A misconfigured webhook
	// (bad URL, unknown event type) fails the boot loudly rather than
	// silently forwarding nothing — same fail-fast as the provider build.
	if cfg.EventWebhook != nil {
		ws, werr := eventwebhook.New(eventwebhook.Options{
			URL:        cfg.EventWebhook.URL,
			EventTypes: cfg.EventWebhook.EventTypes,
			Headers:    cfg.EventWebhook.Headers,
			Timeout:    time.Duration(cfg.EventWebhook.TimeoutMS) * time.Millisecond,
			BufferSize: cfg.EventWebhook.BufferSize,
			MaxRetries: cfg.EventWebhook.MaxRetries,
		})
		if werr != nil {
			fmt.Fprintf(os.Stderr, "Error building event webhook: %v\n", werr)
			os.Exit(1) //nolint:gocritic // matches the other main()-level fatal config paths
		}
		eventWebhookSink = ws
		// context.Background so a graceful Stop can still flush in-flight
		// events after the application context is cancelled (mirrors the
		// session-event writer's Start).
		eventWebhookSink.Start(context.Background())
		sessionSink = emit.MultiSink{sessionSink, eventWebhookSink}
		// Expose the sink's delivery counters on the existing /metrics
		// endpoint: a webhook that silently stops delivering (worker death,
		// endpoint failures, buffer overflow) is visible as a stalled
		// delivered_total / climbing failed_total instead of requiring log
		// archaeology across pods.
		if metricsCollector != nil {
			metricsCollector.MustRegister(
				prometheus.NewCounterFunc(prometheus.CounterOpts{
					Name: "opentalon_event_webhook_delivered_total",
					Help: "Events successfully POSTed to the configured event webhook.",
				}, func() float64 { return float64(ws.Delivered()) }),
				prometheus.NewCounterFunc(prometheus.CounterOpts{
					Name: "opentalon_event_webhook_failed_total",
					Help: "Events whose webhook delivery was given up on (retries exhausted, non-retryable status, or marshal failure).",
				}, func() float64 { return float64(ws.Failed()) }),
				prometheus.NewCounterFunc(prometheus.CounterOpts{
					Name: "opentalon_event_webhook_dropped_total",
					Help: "Events dropped because the webhook delivery buffer was full.",
				}, func() float64 { return float64(ws.Dropped()) }),
			)
		}
	}

	// Build LLM provider and default model from config. The debug sink
	// + resolver pair feeds per-session /debug capture (either nil
	// disables it); sessionSink captures the structured event stream
	// for every LLM call.
	prov, defaultModel, err := buildProvider(cfg, debugSink, debugResolver, sessionSink)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building provider: %v\n", err)
		os.Exit(1) //nolint:gocritic // matches the other main()-level fatal paths; the deferred db.Close is best-effort, the OS reclaims handles on exit
	}

	// Build model lookup map for defaultModelClient.
	modelMap := make(map[string]provider.ModelInfo)
	for _, m := range prov.Models() {
		modelMap[m.ID] = m
	}

	// Surface a repair.model misconfiguration at startup instead of at the
	// first repairable failure: the corrector side-call goes through the
	// same single provider as the session model, so its id must be a bare
	// model id that provider serves — the "provider/model" routing form is
	// sent to the API verbatim and 4xxes on every corrector call, leaving
	// the repair phase enabled but permanently dead. Warn rather than fail:
	// endpoints may serve models beyond the configured list (though such
	// models record usage at zero cost, since cost lookup misses).
	if cfg.Orchestrator.Repair.Enabled && cfg.Orchestrator.Repair.Model != "" {
		if _, ok := modelMap[cfg.Orchestrator.Repair.Model]; !ok {
			configured := make([]string, 0, len(modelMap))
			for id := range modelMap {
				configured = append(configured, id)
			}
			sort.Strings(configured)
			slog.Warn("repair.model is not in the configured models list; corrector calls will fail unless the primary provider serves it (use a bare model id, not the provider/model routing form)",
				"model", cfg.Orchestrator.Repair.Model,
				"configured_models", strings.Join(configured, ", "))
		}
	}

	// LLM client that sets default model when orchestrator doesn't
	llm := &defaultModelClient{provider: prov, model: defaultModel, models: modelMap}

	// Look up context window and output budget for the default model. The
	// output budget (max_tokens) is reserved from the window on every trim so
	// prompt + completion cannot exceed the model's context length.
	var contextWindow, maxOutputTokens int
	if m, ok := modelMap[defaultModel]; ok {
		contextWindow = m.ContextWindow
		maxOutputTokens = m.MaxTokens
	}

	// Sessions created on first message per channel (session key from channel ID)

	toolRegistry := orchestrator.NewToolRegistry()

	// Load tool plugins (path from config or from github+ref via plugins.lock)
	ctx := context.Background()
	pluginEntries := make([]plugin.PluginEntry, 0, len(cfg.Plugins))
	for name, p := range cfg.Plugins {
		path := p.Plugin
		if p.GitHub != "" && p.Ref != "" {
			resolvedPath, err := bundle.EnsurePlugin(ctx, dataDir, name, p.GitHub, p.Ref, p.Cache)
			if err != nil {
				slog.Warn("bundle plugin failed", "plugin", name, "error", err)
				continue
			}
			path = resolvedPath
		}
		if path == "" {
			slog.Warn("plugin has no plugin ref and no github/ref", "plugin", name)
			continue
		}
		pluginCfg := p.Config
		if pluginCfg == nil {
			pluginCfg = make(map[string]interface{})
		}
		// Inject DB connection info only when the plugin opts in via db_access: true.
		if p.DBAccess {
			if _, ok := pluginCfg["__db_driver"]; !ok {
				driver := cfg.State.DB.Driver
				if driver == "" {
					driver = "sqlite"
				}
				pluginCfg["__db_driver"] = driver
				if driver == "postgres" {
					pluginCfg["__db_dsn"] = cfg.State.DB.DSN
				} else if dataDir != "" {
					pluginCfg["__db_dsn"] = filepath.Join(dataDir, "state.db")
				}
			}
		}
		entry := plugin.PluginEntry{
			Name: name, Plugin: path, Enabled: p.Enabled, Config: pluginCfg, ExposeHTTP: p.ExposeHTTP,
		}
		if p.DialTimeout != "" {
			if d, err := time.ParseDuration(p.DialTimeout); err == nil {
				entry.DialTimeout = d
			} else {
				slog.Warn("invalid dial_timeout for plugin, using default", "plugin", name, "value", p.DialTimeout)
			}
		}
		pluginEntries = append(pluginEntries, entry)
	}
	// Request packages (skill-style): loaded before plugins so MCP server configs
	// can be injected into the MCP plugin binary's environment at launch.
	var requestSets []requestpkg.Set
	if cfg.RequestPackages.Path != "" {
		dirSets, err := requestpkg.LoadDir(cfg.RequestPackages.Path)
		if err != nil {
			slog.Warn("request_packages path failed", "path", cfg.RequestPackages.Path, "error", err)
		} else {
			requestSets = append(requestSets, dirSets...)
		}
	}
	if cfg.RequestPackages.SkillsPath != "" {
		skillSets, err := requestpkg.LoadSkillsDir(cfg.RequestPackages.SkillsPath)
		if err != nil {
			slog.Warn("request_packages skills_path failed", "path", cfg.RequestPackages.SkillsPath, "error", err)
		} else {
			requestSets = append(requestSets, skillSets...)
		}
	}
	// Download skills by name (from default repo or per-skill github/ref)
	var defaultRepoPath string
	if cfg.RequestPackages.DefaultSkillGitHub != "" && cfg.RequestPackages.DefaultSkillRef != "" {
		p, err := bundle.EnsureSkillsRepo(ctx, dataDir, cfg.RequestPackages.DefaultSkillGitHub, cfg.RequestPackages.DefaultSkillRef)
		if err != nil {
			slog.Warn("skills repo failed", "repo", cfg.RequestPackages.DefaultSkillGitHub, "error", err)
		} else {
			defaultRepoPath = p
		}
	}
	for _, skill := range cfg.RequestPackages.Skills {
		if skill.Name == "" {
			continue
		}
		var skillDir string
		switch {
		case skill.GitHub != "" && skill.Ref != "":
			path, err := bundle.EnsureSkillDir(ctx, dataDir, skill.Name, skill.GitHub, skill.Ref)
			if err != nil {
				slog.Warn("skill bundle failed", "skill", skill.Name, "error", err)
				continue
			}
			skillDir = path
		case defaultRepoPath != "":
			skillDir = filepath.Join(defaultRepoPath, skill.Name)
		default:
			slog.Warn("skill has no github/ref and no default_skill_github/ref", "skill", skill.Name)
			continue
		}
		set, err := requestpkg.LoadSkillDir(skillDir)
		if err != nil {
			slog.Warn("load skill failed", "skill", skill.Name, "dir", skillDir, "error", err)
			continue
		}
		requestSets = append(requestSets, set)
	}
	// Merge installed skills (persisted from /install skill) so they survive restart
	if installed, err := config.LoadInstalledSkills(dataDir); err == nil {
		for _, skill := range installed {
			if skill.Name == "" || skill.GitHub == "" {
				continue
			}
			ref := skill.Ref
			if ref == "" {
				ref = "main"
			}
			path, err := bundle.EnsureSkillDir(ctx, dataDir, skill.Name, skill.GitHub, ref)
			if err != nil {
				slog.Warn("installed skill bundle failed", "skill", skill.Name, "error", err)
				continue
			}
			set, err := requestpkg.LoadSkillDir(path)
			if err != nil {
				slog.Warn("load installed skill failed", "skill", skill.Name, "error", err)
				continue
			}
			requestSets = append(requestSets, set)
		}
	}
	for _, inl := range cfg.RequestPackages.Inline {
		set := requestpkg.Set{PluginName: inl.Plugin, Description: inl.Description, AllowedGroups: inl.AllowedGroups}
		set.MCP = mcpConfigFromInline(inl.MCP)
		for _, p := range inl.Packages {
			params := make([]requestpkg.ParamDefinition, len(p.Parameters))
			for i, q := range p.Parameters {
				params[i] = requestpkg.ParamDefinition{Name: q.Name, Description: q.Description, Required: q.Required}
			}
			set.Packages = append(set.Packages, requestpkg.Package{
				Action: p.Action, Description: p.Description, Method: p.Method, URL: p.URL,
				Body: p.Body, Headers: p.Headers, RequiredEnv: p.RequiredEnv, Parameters: params,
			})
		}
		requestSets = append(requestSets, set)
	}

	injectMCPServers(pluginEntries, requestpkg.CollectMCPServers(requestSets), dataDir)

	for _, e := range pluginEntries {
		if e.Enabled && dataDir != "" {
			if err := store.RunPluginMigrations(dataDir, e.Name, filepath.Dir(e.Plugin)); err != nil {
				slog.Warn("plugin migrations failed", "plugin", e.Name, "error", err)
			}
		}
	}
	pluginManager := plugin.NewManager(toolRegistry)
	retryCtx, retryCancel := context.WithCancel(ctx)
	defer retryCancel()
	slog.Info("loading plugins", "component", "startup", "count", len(pluginEntries))
	if err := pluginManager.LoadAll(ctx, pluginEntries); err != nil {
		slog.Warn("some plugins failed to load", "error", err)
	}
	slog.Info("plugin loading complete", "component", "startup", "loaded", len(pluginManager.List()))
	pluginManager.StartRetryLoop(retryCtx, 10*time.Second)

	if err := requestpkg.Register(toolRegistry, requestSets); err != nil {
		slog.Warn("request_packages registration failed", "error", err)
	}

	// Register built-in opentalon plugin (install_skill, show_config, list_commands, set_prompt, clear_session, reload_mcp)
	runtimePromptPath := ""
	if dataDir != "" {
		runtimePromptPath = filepath.Join(dataDir, "custom_prompt.txt")
	}
	mcpCacheDir := ""
	if dataDir != "" {
		mcpCacheDir = filepath.Join(dataDir, "mcp-cache")
	}
	// When a knowledge plugin is configured, automatically refresh it on /clear.
	var onClearActions []commands.OnClearAction
	if cfg.Orchestrator.Knowledge.Plugin != "" {
		onClearActions = append(onClearActions, commands.OnClearAction{
			Plugin: cfg.Orchestrator.Knowledge.Plugin,
			Action: "refresh",
		})
	}
	cmdExecutor := commands.NewExecutor(toolRegistry, sessions, dataDir, cfg, runtimePromptPath).
		WithMCPReload(pluginManager, mcpCacheDir).
		WithProfileStore(groupPluginStore)
	if debugStore != nil {
		cmdExecutor.WithDebugEventCounter(debugStore)
	}
	if err := toolRegistry.Register(commands.Capability(), cmdExecutor); err != nil {
		slog.Warn("register opentalon commands failed", "error", err)
	}

	contentPreparers := make([]orchestrator.ContentPreparerEntry, 0, len(cfg.Orchestrator.ContentPreparers))
	for _, p := range cfg.Orchestrator.ContentPreparers {
		entry := orchestrator.ContentPreparerEntry{
			Plugin:   p.Plugin,
			Action:   p.Action,
			ArgKey:   p.ArgKey,
			Guard:    p.Guard,
			FailOpen: p.FailOpen,
			STT:      p.STT,
			Insecure: true, // default: cannot run invoke
		}
		if !strings.HasPrefix(p.Plugin, "lua:") {
			if plug, ok := cfg.Plugins[p.Plugin]; ok && plug.Insecure != nil && !*plug.Insecure {
				entry.Insecure = false // trusted: can invoke
			}
		}
		contentPreparers = append(contentPreparers, entry)
	}
	responseFormatters := make([]orchestrator.ResponseFormatterEntry, 0, len(cfg.Orchestrator.ResponseFormatters))
	for _, f := range cfg.Orchestrator.ResponseFormatters {
		failOpen := true
		if f.FailOpen != nil {
			failOpen = *f.FailOpen
		}
		responseFormatters = append(responseFormatters, orchestrator.ResponseFormatterEntry{
			Plugin:   f.Plugin,
			Action:   f.Action,
			FailOpen: failOpen,
		})
	}
	luaScriptPaths := buildLuaScriptPaths(ctx, dataDir, cfg)
	var permChecker orchestrator.PermissionChecker
	permPluginName := cfg.Orchestrator.PermissionPlugin
	if permPluginName != "" {
		permChecker = orchestrator.NewPermissionChecker(toolRegistry, orchestrator.NewGuard(), permPluginName)
	}
	pipelineCfg := pipeline.DefaultConfig()
	if cfg.Orchestrator.Pipeline.MaxStepRetries > 0 {
		pipelineCfg.MaxStepRetries = cfg.Orchestrator.Pipeline.MaxStepRetries
	}
	if cfg.Orchestrator.Pipeline.StepTimeout != "" {
		if d, err := time.ParseDuration(cfg.Orchestrator.Pipeline.StepTimeout); err == nil {
			pipelineCfg.StepTimeout = d
		}
	}
	var planTimeout time.Duration
	if cfg.Orchestrator.Pipeline.PlanTimeout != "" {
		if d, err := time.ParseDuration(cfg.Orchestrator.Pipeline.PlanTimeout); err == nil {
			planTimeout = d
		}
	}
	// Build profile verifier (nil when profiles.who_am_i.url is not configured).
	var profileVerifier channel.ProfileVerifier
	if cfg.Profiles.WhoAmI.URL != "" {
		vcfg := profile.VerifierConfig{
			URL:               cfg.Profiles.WhoAmI.URL,
			Method:            cfg.Profiles.WhoAmI.Method,
			TokenHeader:       cfg.Profiles.WhoAmI.TokenHeader,
			TokenPrefix:       cfg.Profiles.WhoAmI.TokenPrefix,
			EntityIDField:     cfg.Profiles.WhoAmI.EntityIDField,
			GroupField:        cfg.Profiles.WhoAmI.GroupField,
			PluginsField:      cfg.Profiles.WhoAmI.PluginsField,
			ModelField:        cfg.Profiles.WhoAmI.ModelField,
			ChannelTypeField:  cfg.Profiles.WhoAmI.ChannelTypeField,
			ChannelTypeHeader: cfg.Profiles.WhoAmI.ChannelTypeHeader,
			LimitField:        cfg.Profiles.WhoAmI.LimitField,
			LimitTimeField:    cfg.Profiles.WhoAmI.LimitTimeField,
			NameField:         cfg.Profiles.WhoAmI.NameField,
			ExtraHeaders:      cfg.Profiles.WhoAmI.ExtraHeaders,
			MetadataHeaders:   cfg.Profiles.WhoAmI.MetadataHeaders,
		}
		if d, err := time.ParseDuration(cfg.Profiles.WhoAmI.Timeout); err == nil {
			vcfg.Timeout = d
		}
		if d, err := time.ParseDuration(cfg.Profiles.WhoAmI.CacheTTL); err == nil {
			vcfg.CacheTTL = d
		}
		profileVerifier = profile.NewVerifier(vcfg, groupPluginStore, entityStore)
		slog.Info("profile verification enabled", "url", cfg.Profiles.WhoAmI.URL)
	}

	// Build orchestrator usage recorder adapter (nil when usageStore is nil and metrics disabled).
	var usageRecorder orchestrator.UsageRecorder
	if usageStore != nil || metricsCollector != nil {
		usageRecorder = &usageRecorderAdapter{store: usageStore, provider: prov, collector: metricsCollector}
	}
	// Avoid non-nil interface wrapping a nil pointer.
	var pluginObserver orchestrator.PluginCallObserver
	if metricsCollector != nil {
		pluginObserver = metricsCollector
	}

	// channelNotifier carries a late-bound *channel.Registry pointer; both
	// the scheduler (job notifications) and the orchestrator (server-
	// initiated session.title frames) take method values whose receiver
	// captures this struct, so the registry can be wired in later without
	// rebuilding either consumer. Created here to break the
	// orch ← ChannelSender ← notifier ← reg ← handler ← orch cycle.
	notifier := &channelNotifier{reg: nil}

	// Build a single shared Redis client when cluster dedup, plugin exec, or both need it.
	// Sharing one pool halves connection count compared to opening two clients to the same instance.
	// Hoisted before the orchestrator so the session-turn lease can use it
	// (and before sync so the sync lock can, too).
	needsRedis := cfg.Cluster.Enabled || cfg.PluginExec.Enabled
	var sharedRedis redis.UniversalClient
	if needsRedis && (cfg.Redis.RedisURL != "" || len(cfg.Redis.Sentinels) > 0) {
		var err error
		sharedRedis, err = redisclient.New(
			cfg.Redis.RedisURL,
			cfg.Redis.MasterName,
			cfg.Redis.Sentinels,
			cfg.Redis.Password,
			cfg.Redis.SentinelPassword,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error connecting to Redis: %v\n", err)
			os.Exit(1) //nolint:gocritic
		}
		defer func() { _ = sharedRedis.Close() }()
	}
	if cfg.Cluster.Enabled && sharedRedis == nil {
		fmt.Fprintf(os.Stderr, "cluster.enabled requires redis.redis_url or redis.sentinels to be configured\n")
		os.Exit(1) //nolint:gocritic
	}

	// Cross-pod session-turn lease: in cluster mode, turns for the same
	// session are serialized across pods via Redis; single-instance mode
	// relies on the orchestrator's in-memory per-session mutex alone.
	var sessionLocker sessionlock.Locker
	if cfg.Cluster.Enabled && sharedRedis != nil {
		sessionLocker = sessionlock.NewRedis(sharedRedis)
	} else {
		sessionLocker = sessionlock.Noop()
	}

	// Pending pipeline plans (multi-step plans awaiting the user's approval)
	// are held in pod memory only. That is fine on one pod, but with several
	// pods a confirmation reply routed to a different pod cannot resume the
	// plan. Surface the combination loudly instead of failing silently.
	if cfg.Cluster.Enabled && cfg.Orchestrator.Pipeline.Enabled {
		slog.Warn("cluster mode with orchestrator.pipeline.enabled: pending pipeline plans are per-pod in-memory state and are NOT multi-pod safe; a confirmation reply landing on another pod cannot resume the plan")
	}

	// Apply built-in prompt overrides from config before the orchestrator is
	// built (NewWithRules reads the default/scheduling rules). Unknown keys are
	// surfaced as a warning rather than a hard failure so a typo doesn't block
	// startup. This only swaps the embedded prompt defaults; plugin/MCP server
	// instructions are still appended to the system prompt as before.
	if len(cfg.Orchestrator.PromptOverrides) > 0 {
		applied, unknown := prompts.ApplyOverrides(cfg.Orchestrator.PromptOverrides)
		if len(applied) > 0 {
			slog.Info("prompt overrides applied", "prompts", applied)
		}
		if len(unknown) > 0 {
			slog.Warn("ignoring unknown prompt override keys", "keys", unknown, "valid", prompts.OverridableNames())
		}
	}

	orch := orchestrator.NewWithRules(llm, orchestrator.DefaultParser, toolRegistry, memory, sessions, orchestrator.OrchestratorOpts{
		CustomRules:                   cfg.Orchestrator.Rules,
		ContentPreparers:              contentPreparers,
		ResponseFormatters:            responseFormatters,
		LuaScriptPaths:                luaScriptPaths,
		PermissionChecker:             permChecker,
		PermissionPluginName:          permPluginName,
		RuntimePromptPath:             runtimePromptPath,
		ContextMessages:               cfg.State.Session.ContextMessages, // 0 = all messages; >0 = send only last N messages to LLM
		SummarizeAfterMessages:        cfg.State.Session.SummarizeAfter,  // 0 (default) = off; set to e.g. 10 to enable LLM summarization
		MaxMessagesAfterSummary:       defaultInt(cfg.State.Session.MaxMessagesAfterSummary, 5),
		SummarizePrompt:               cfg.State.Session.SummarizePrompt,
		SummarizeUpdatePrompt:         cfg.State.Session.SummarizeUpdatePrompt,
		SessionTitlePrompt:            cfg.State.Session.SessionTitlePrompt,
		PipelineEnabled:               cfg.Orchestrator.Pipeline.Enabled,
		PlanTimeout:                   planTimeout,
		PipelineConfig:                pipelineCfg,
		ConfirmationPlugin:            cfg.Orchestrator.Pipeline.ConfirmationPlugin,
		ConfirmationAction:            cfg.Orchestrator.Pipeline.ConfirmationAction,
		ConfirmationClassifierEnabled: cfg.Orchestrator.Pipeline.ConfirmationClassifierEnabled,
		ContextWindow:                 contextWindow,
		MaxOutputTokens:               maxOutputTokens,
		MaxConcurrentSessions:         cfg.Orchestrator.MaxConcurrentSessions,
		GroupPluginLookup:             groupPluginStore,
		UsageRecorder:                 usageRecorder,
		PluginCallObserver:            pluginObserver,
		EventSink:                     sessionSink,       // async-buffered via SessionEventWriter
		PromptSnapshotStore:           sessionEventStore, // direct/sync store; intentionally not async-buffered so a consumer reading a turn_start event can resolve its sha256 references without racing the writer. nil when state DB is not configured
		SyncActionsPlugin:             cfg.Orchestrator.Knowledge.SyncPlugin,
		SyncActionsAction:             cfg.Orchestrator.Knowledge.SyncAction,
		Knowledge: orchestrator.KnowledgeConfig{
			Plugin: cfg.Orchestrator.Knowledge.Plugin,
			Action: cfg.Orchestrator.Knowledge.Action,
			Dir:    cfg.Orchestrator.Knowledge.Dir,
		},
		ShowToolCalls: cfg.Orchestrator.ShowToolCalls,
		// The DB-backed SessionStore satisfies InjectionStateStore via
		// its GetInjectionState / UpdateInjectionState methods
		// (migration 010). When state DB is not configured the variable
		// stays nil and load_tools sticky promotions don't persist
		// across turns.
		InjectionStateStore: injectionStateStore,
		ToolErrorHandling: orchestrator.ToolErrorHandlingConfig{
			LoopCapPerTurn:          cfg.Orchestrator.Preparer.ToolErrorHandling.LoopCapPerTurn,
			StickyDemotionThreshold: cfg.Orchestrator.Preparer.ToolErrorHandling.StickyDemotionThreshold,
		},
		ChannelSender:        notifier.SendToSession,
		SessionTitlesEnabled: true,
		Repair: orchestrator.RepairConfig{
			Enabled:     cfg.Orchestrator.Repair.Enabled,
			Model:       cfg.Orchestrator.Repair.Model,
			Prompt:      cfg.Orchestrator.Repair.Prompt,
			MaxAttempts: cfg.Orchestrator.Repair.MaxAttempts,
			Timeout:     parseDurationOrZero(cfg.Orchestrator.Repair.Timeout),
		},
		Subprocess: orchestrator.SubprocessConfig{
			Enabled:       cfg.Orchestrator.Subprocess.Enabled,
			MaxDepth:      cfg.Orchestrator.Subprocess.MaxDepth,
			MaxIterations: cfg.Orchestrator.Subprocess.MaxIterations,
			DefaultTimeout: func() time.Duration {
				if cfg.Orchestrator.Subprocess.DefaultTimeout != "" {
					if d, err := time.ParseDuration(cfg.Orchestrator.Subprocess.DefaultTimeout); err == nil {
						return d
					}
				}
				return 60 * time.Second
			}(),
		},
		SessionLocker: sessionLocker,
	})

	// Wire on-clear actions now that the orchestrator is available.
	cmdExecutor.WithOnClear(onClearActions, orch.RunAction)

	// Build sync locker: cluster mode uses Redis so only one pod runs
	// SyncActions/IngestKnowledgeDir; single-instance uses noop.
	var slocker synclock.Locker
	if cfg.Cluster.Enabled && sharedRedis != nil {
		slocker = synclock.NewRedis(sharedRedis)
	} else {
		slocker = synclock.Noop()
	}

	// Sync plugin capabilities to the vector store and ingest knowledge articles
	// from the configured directory. Runs synchronously so the orchestrator is
	// fully ready before accepting traffic.
	//
	// These calls use guard.ExecuteWithTimeout directly (not executeCall) because
	// there is no actor/session at startup. This intentionally skips permission
	// checks, audit logging, arg validation, and plugin-allowed filtering.
	// If the sync or ingest plugin ever declares AuditLog=true, those calls
	// will not be logged — acceptable for host-initiated startup work.
	slog.Info("startup: syncing plugin actions and knowledge to vector store", "component", "startup")
	acquired, err := slocker.AcquireOrWait(ctx)
	if err != nil {
		slog.Error("startup: sync lock failed, proceeding with local sync", "component", "startup", "error", err)
		acquired = true
	}
	if acquired {
		orch.SyncActions(ctx)
		orch.IngestKnowledgeDir(ctx)
		slocker.ReleaseDone(ctx)
		slog.Info("startup: sync complete (leader), orchestrator ready", "component", "startup")
	} else {
		slog.Info("startup: sync skipped (follower), orchestrator ready", "component", "startup")
	}

	// Forward-declare channelManager so the OnPluginLoaded closure can
	// reference it for readiness checks (channelManager is created below).
	var channelManager *channel.Manager

	// When a plugin comes online later (e.g. via the retry loop), sync its
	// actions to the vector store automatically. In cluster mode, the lock
	// ensures only one pod performs the sync for a given plugin.
	pluginManager.OnPluginLoaded(func(name string) {
		go func() {
			ok, lockErr := slocker.TryAcquirePlugin(ctx, name)
			if lockErr != nil {
				slog.Warn("plugin sync lock failed, proceeding", "plugin", name, "error", lockErr)
				ok = true
			}
			if ok {
				defer slocker.ReleasePlugin(ctx, name)
				orch.SyncPluginActions(ctx, name)
			} else {
				slog.Debug("plugin sync skipped (another pod is syncing)", "plugin", name)
			}
		}()
		// Update readiness when a late-loaded plugin comes online.
		if channelManager != nil && pluginManager.Ready() && channelManager.Ready() {
			healthSrv.SetReady("opentalon", true)
		}
	})

	// Capability refresh poll: every refreshInterval, re-fetch each loaded
	// plugin's upstream capabilities, update the executable registry, and
	// re-sync the corpus (leader-gated), so changed tool descriptions, server
	// instructions or knowledge articles propagate without a pod restart.
	refreshInterval := 15 * time.Minute
	if raw := cfg.Orchestrator.Knowledge.RefreshInterval; raw != "" {
		if d, err := time.ParseDuration(raw); err != nil {
			slog.Warn("invalid knowledge.refresh_interval, using default",
				"component", "refresh", "value", raw, "default", refreshInterval.String(), "error", err)
		} else {
			refreshInterval = d
		}
	}
	if refreshInterval > 0 {
		slog.Info("startup: capability refresh poll enabled", "component", "refresh", "interval", refreshInterval.String())
		go func() {
			ticker := time.NewTicker(refreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-retryCtx.Done():
					return
				case <-ticker.C:
					refreshAllCapabilities(retryCtx, pluginManager, toolRegistry, orch, slocker)
				}
			}
		}()
	} else {
		slog.Info("startup: capability refresh poll disabled", "component", "refresh")
	}

	// Scheduler: wired after orchestrator so it can route job actions through orch.
	// Personal reminders bypass the approver policy via AddPersonalJob.
	// Reuses the channelNotifier built above the orchestrator (shared
	// late-bound *channel.Registry pointer); the title-push path and the
	// scheduler's job-result path therefore go through the same adapter.
	// Scheduler/reminder jobs persist to pod-local disk (dataDir), so in
	// cluster mode a job created on one pod is delivered and visible only on
	// that pod (and lost if the pod is replaced) — see docs/cluster.md.
	if cfg.Cluster.Enabled {
		slog.Warn("cluster mode: scheduler and reminder jobs persist to pod-local disk; each job is visible and delivered only on the pod that created it and does not survive pod replacement")
	}
	sched := scheduler.NewWithPolicy(orch, notifier, dataDir, cfg.Scheduler.Approvers, cfg.Scheduler.MaxJobsPerUser)
	staticJobs := make([]scheduler.Job, 0, len(cfg.Scheduler.Jobs))
	for _, jc := range cfg.Scheduler.Jobs {
		if jc.Enabled != nil && !*jc.Enabled {
			continue
		}
		staticJobs = append(staticJobs, scheduler.Job{
			Name:          jc.Name,
			Interval:      jc.Interval,
			Cron:          jc.Cron,
			At:            jc.At,
			Action:        jc.Action,
			Args:          jc.Args,
			NotifyChannel: jc.NotifyChannel,
		})
	}
	if err := sched.Start(staticJobs); err != nil {
		slog.Warn("scheduler start failed", "error", err)
	}
	defer sched.Stop()
	schedTool := scheduler.NewSchedulerTool(sched)
	if err := toolRegistry.Register(schedTool.Capability(), schedTool); err != nil {
		slog.Warn("register scheduler tool failed", "error", err)
	}
	if err := toolRegistry.Register(reminder.Capability(), reminder.NewTool()); err != nil {
		slog.Warn("register reminder tool failed", "error", err)
	}

	// Strict resume: surface "not found" up to the handler so it can emit
	// session_expired to the client rather than silently auto-creating
	// (the pre-refactor footgun that let UI and server drift apart). The
	// underlying SessionStore.Get already wraps state.ErrSessionNotFound on
	// genuine misses and a distinct infra error otherwise; the handler
	// discriminates via errors.Is.
	resumeSession := func(sessionKey string) error {
		_, err := sessions.Get(sessionKey)
		return err
	}
	// Fresh mint: delegates to the underlying store, which is idempotent
	// in both the DB-backed (INSERT-on-conflict returns existing row) and
	// the in-memory variant (Create returns existing pointer when present).
	createSession := func(sessionKey, entityID, groupID, kind string) {
		sessions.Create(sessionKey, entityID, groupID, kind)
	}
	runner := &channelRunner{orch: orch}
	handler := channel.NewMessageHandler(channel.HandlerConfig{
		ResumeSession: resumeSession,
		CreateSession: createSession,
		Runner:        runner,
		RunAction:     orch.RunAction,
		HasAction:     toolRegistry.HasAction,
		Verifier:      profileVerifier,
		LimitChecker:  usageStore,
		// Read-only re-emit of a still-pending tool confirmation on a resume
		// handshake, so a reconnected client redraws its Approve/Reject buttons.
		PendingConfirmation: orch.PendingConfirmationFrame,
	})

	reg := channel.NewRegistry(handler)
	notifier.reg = reg

	if dw := cfg.Orchestrator.DebounceWindow; dw != "" {
		if d, err := time.ParseDuration(dw); err == nil && d > 0 {
			reg.SetDebounceWindow(d)
			slog.Info("message debounce enabled", "window", d)
		} else if err != nil {
			slog.Warn("invalid orchestrator.debounce_window, debounce disabled", "value", dw, "error", err)
		}
	}

	if cfg.Cluster.Enabled {
		dedupTTL := 5 * time.Minute
		if cfg.Cluster.DedupTTL != "" {
			if d, err := time.ParseDuration(cfg.Cluster.DedupTTL); err == nil {
				dedupTTL = d
			} else {
				slog.Warn("invalid cluster.dedup_ttl, using default 5m", "value", cfg.Cluster.DedupTTL)
			}
		}
		reg.SetDeduplicator(dedup.NewFromClient(sharedRedis), dedupTTL)
		slog.Info("cluster deduplication enabled", "ttl", dedupTTL, "sentinel", len(cfg.Redis.Sentinels) > 0)
	}
	channelManager = channel.NewManager(reg, toolRegistry)
	// Wire the inbound-enrichment cache. Redis-backed when the deployment
	// already runs Redis (cache is shared across pods, survives restarts);
	// in-memory fallback otherwise so single-pod and dev setups keep
	// working without Redis. Channels without an inbound.enrich block
	// ignore the cache entirely.
	channelManager.SetEnrichCache(channel.NewEnrichCache(sharedRedis))
	channelEntries := make([]channel.ChannelEntry, 0, len(cfg.Channels))
	for name, ch := range cfg.Channels {
		pathRef := ch.Plugin
		if ch.GitHub != "" && ch.Ref != "" {
			resolvedPath, err := bundle.EnsureChannel(ctx, dataDir, name, ch.GitHub, ch.Ref, ch.Cache)
			if err != nil {
				slog.Warn("bundle channel failed", "channel", name, "error", err)
				continue
			}
			pathRef = resolvedPath
		}
		if pathRef == "" {
			slog.Warn("channel has no plugin ref and no github/ref", "channel", name)
			continue
		}
		channelEntries = append(channelEntries, channel.ChannelEntry{
			Name: name, Plugin: pathRef, Enabled: ch.Enabled, Config: ch.Config,
		})
	}
	if err := channelManager.LoadAll(ctx, channelEntries); err != nil {
		slog.Warn("some channels failed to load", "error", err)
	} else {
		slog.Info("channels loaded")
	}

	// Mark readiness once all plugins and channels are loaded.
	if pluginManager.Ready() && channelManager.Ready() {
		healthSrv.SetReady("opentalon", true)
	}

	// Start plugin exec dispatcher (allows trusted plugins to execute ToolRegistry actions via Redis).
	// Requires plugin_exec.enabled: true and redis.redis_url (or sentinels) to be set.
	var dispatcher *plugin.ExecDispatcher
	var dispatchCancel context.CancelFunc
	if cfg.PluginExec.Enabled {
		if sharedRedis == nil {
			fmt.Fprintf(os.Stderr, "plugin_exec.enabled requires redis.redis_url or redis.sentinels to be configured\n")
			os.Exit(1) //nolint:gocritic
		} else {
			var actionTimeout time.Duration
			if cfg.PluginExec.ActionTimeout != "" {
				if d, err := time.ParseDuration(cfg.PluginExec.ActionTimeout); err == nil {
					actionTimeout = d
				} else {
					slog.Warn("plugin_exec.action_timeout invalid, using default", "value", cfg.PluginExec.ActionTimeout)
				}
			}
			dispatcher = plugin.NewExecDispatcher(sharedRedis, orch, actionTimeout)
			var dispatchCtx context.Context
			dispatchCtx, dispatchCancel = context.WithCancel(ctx)
			go dispatcher.Start(dispatchCtx)
		}
	}

	sigCh := make(chan os.Signal, 1)
	// SIGTERM is what Kubernetes sends to terminate a pod; without it the
	// graceful teardown below never runs and the process is hard-killed,
	// severing every open WebSocket chat session on every rollout.
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	// Stop advertising readiness as the first shutdown step so Kubernetes
	// removes this pod from the Service endpoints before we close connections.
	// The bounded writer flushes below double as the endpoint-drain window.
	healthSrv.SetReady("opentalon", false)

	// Flush debug-event writer first so any /debug rows captured up to the
	// signal are persisted. Bounded wait — never block shutdown forever.
	if debugWriter != nil {
		debugWriter.Stop(5 * time.Second)
	}
	if debugRetentionCancel != nil {
		debugRetentionCancel()
	}
	// Cancel the structured-event retention loop now; the writer itself is
	// stopped further below — after the producers — so a turn's final events
	// still land instead of being dropped (or racing the buffer close).
	if sessionEventsRetentionCancel != nil {
		sessionEventsRetentionCancel()
	}

	// Stop health server first so K8s stops routing traffic during teardown.
	healthSrv.Shutdown()

	// Stop the scheduler before channels so in-flight reminders can still
	// be delivered via the channel registry. (deferred sched.Stop() above
	// runs late — we want explicit ordering here.)
	sched.Stop()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(context.Background())
	}
	channelManager.StopAll()
	pluginManager.StopAll()

	// Cancel the dispatcher and wait for all in-flight handlers to finish draining
	// before the deferred sharedRedis.Close() runs. Without this wait, handlers
	// mid-flight when the signal arrives would hit XAck/XAdd on a closed client.
	if dispatcher != nil && dispatchCancel != nil {
		dispatchCancel()
		dispatcher.Wait()
	}

	// Event sinks (structured-event writer + event webhook) are stopped LAST —
	// after every producer that can Emit has finished: channelManager.StopAll
	// joins the in-flight WebSocket turns (registry wg.Wait), dispatcher.Wait
	// joins the redis-queued Runs. A turn draining in this window fires its
	// deferred turn_finished; stopping the sinks any earlier would drop that
	// final event and risk an Emit racing the channel close. Bounded flush so
	// shutdown never blocks forever.
	if sessionEventWriter != nil {
		sessionEventWriter.Stop(5 * time.Second)
	}
	if eventWebhookSink != nil {
		eventWebhookSink.Stop(5 * time.Second)
	}
}

// providerDebugSink adapts the state-store async writer to the
// provider.DebugEventSink interface. The provider package can't import
// store directly without dragging the SQL drivers into every test build,
// so this thin shim keeps the dependency direction one-way: main.go
// composes both packages.
type providerDebugSink struct {
	writer *store.DebugEventWriter
}

func (s *providerDebugSink) Submit(_ context.Context, e provider.DebugEvent) {
	s.writer.Submit(store.DebugEvent{
		SessionID: e.SessionID,
		TraceID:   e.TraceID,
		Direction: e.Direction,
		Status:    e.Status,
		URL:       e.URL,
		Body:      e.Body,
		Timestamp: e.Timestamp,
	})
}

// sessionSinkAdapter adapts the state-store async session-event writer
// to the emit.Sink interface. Stays in main.go for the same dependency-
// direction reason as providerDebugSink: the emit package cannot import
// store without inviting an import cycle through provider, so this
// composition layer is the natural home for the conversion.
type sessionSinkAdapter struct {
	writer *store.SessionEventWriter
}

func (s *sessionSinkAdapter) Emit(_ context.Context, e emit.Event) {
	s.writer.Submit(store.SessionEvent{
		ID:         e.ID,
		SessionID:  e.SessionID,
		EventType:  e.EventType,
		ParentID:   e.ParentID,
		DurationMS: e.DurationMS,
		Payload:    e.Payload,
	})
}

// seedGroupPlugins seeds the static group→plugin config baseline to the DB.
// Rows with source="config" are inserted only when no row exists yet for that group+plugin pair.
func seedGroupPlugins(ctx context.Context, gps *store.GroupPluginStore, groups map[string]config.GroupConfig) {
	if gps == nil || len(groups) == 0 {
		return
	}
	for groupID, gc := range groups {
		if len(gc.Plugins) == 0 {
			continue
		}
		if err := gps.UpsertGroupPlugins(ctx, groupID, gc.Plugins, "config"); err != nil {
			slog.Warn("seed group plugins failed", "group", groupID, "error", err)
		}
	}
}

// seedBootstrapGroupPlugins seeds group→plugin assignments fetched from the remote bootstrap
// endpoint. Uses source="bootstrap" which has the same priority as "config" in the DB.
func seedBootstrapGroupPlugins(ctx context.Context, gps *store.GroupPluginStore, groups map[string][]string) {
	if gps == nil || len(groups) == 0 {
		return
	}
	for groupID, plugins := range groups {
		if len(plugins) == 0 {
			continue
		}
		if err := gps.UpsertGroupPlugins(ctx, groupID, plugins, "bootstrap"); err != nil {
			slog.Warn("seed bootstrap group plugins failed", "group", groupID, "error", err)
		}
	}
}

// usageRecorderAdapter adapts store.UsageStore to orchestrator.UsageRecorder.
type usageRecorderAdapter struct {
	store     *store.UsageStore
	provider  provider.Provider
	collector *metrics.Collector
}

func (a *usageRecorderAdapter) RecordUsage(ctx context.Context, entityID, groupID, channelID, sessionID, modelID, interactionKind, systemSource string, inputTokens, outputTokens, toolCalls int) {
	if a.store == nil && a.collector == nil {
		return
	}
	var inputCostUSD, outputCostUSD float64
	if modelID != "" && a.provider != nil {
		for _, m := range a.provider.Models() {
			if m.ID == modelID {
				// Cost is configured per million tokens.
				inputCostUSD = float64(inputTokens) * m.Cost.Input / 1_000_000
				outputCostUSD = float64(outputTokens) * m.Cost.Output / 1_000_000
				break
			}
		}
	}
	if a.store != nil {
		if err := a.store.Record(ctx, store.UsageRecord{
			EntityID:        entityID,
			GroupID:         groupID,
			ChannelID:       channelID,
			SessionID:       sessionID,
			ModelID:         modelID,
			InteractionKind: interactionKind,
			SystemSource:    systemSource,
			InputTokens:     inputTokens,
			OutputTokens:    outputTokens,
			ToolCalls:       toolCalls,
			InputCost:       inputCostUSD,
			OutputCost:      outputCostUSD,
		}); err != nil {
			slog.Warn("usage record failed", "entity", entityID, "error", err)
		}
	}
	if a.collector != nil {
		a.collector.RecordUsage(ctx, entityID, groupID, channelID, sessionID, modelID,
			inputTokens, outputTokens, toolCalls, inputCostUSD, outputCostUSD)
	}
}

// newInMemoryState returns in-memory memory and session stores (used when data_dir is unset or DB open fails).
func newInMemoryState() (orchestrator.MemoryStoreInterface, orchestrator.SessionStoreInterface) {
	mem := state.NewMemoryStore("")
	_ = mem.Load()
	return mem, state.NewSessionStore("")
}

// channelNotifier adapts channel.Registry to scheduler.Notifier so the
// scheduler can deliver job results back to the channel where they were
// scheduled. The registry pointer is set after construction because the
// scheduler must be built before the orchestrator is, but the registry
// depends on the orchestrator.
type channelNotifier struct {
	reg *channel.Registry
}

func (n *channelNotifier) Notify(ctx context.Context, channelID, conversationID, content string) error {
	if n.reg == nil {
		return fmt.Errorf("channel registry not yet initialized")
	}
	return n.reg.Send(ctx, channelID, chanpkg.OutboundMessage{
		ConversationID: conversationID,
		Content:        content,
	})
}

// SendToSession satisfies orchestrator.ChannelSender. The orchestrator
// only knows a session key; the inverse split lives here at the
// integration boundary so the orchestrator stays free of channel-
// topology knowledge. Used by maybeGenerateTitle to broadcast the live
// session.title frame; same late-bound reg pointer as Notify.
//
// Key shape: pkg.SessionKey emits "<channelID>:<conversationID>" (or
// "...:threadID" when set), but the handler in internal/channel
// prepends "<entityID>:" once a profile is resolved (see handler.go:
// `sessionKey = p.EntityID + ":" + sessionKey`). The shipped channels
// (websocket, console) never set threadID, so in practice keys are
// 2-part (anonymous) or 3-part (profile-resolved) and the right-most
// two segments are always channelID + conversationID. Split-from-right
// covers both without an entityID lookup; if a future channel ever
// sets threadID alongside a profile prefix, the registry-match
// fallback below catches it.
func (n *channelNotifier) SendToSession(ctx context.Context, sessionID string, msg chanpkg.OutboundMessage) error {
	if n.reg == nil {
		return fmt.Errorf("channel registry not yet initialized")
	}
	parts := strings.Split(sessionID, ":")
	if len(parts) < 2 {
		return fmt.Errorf("invalid session key %q", sessionID)
	}
	channelID := parts[len(parts)-2]
	conversationID := parts[len(parts)-1]
	if _, ok := n.reg.Get(channelID); ok {
		// From-right-2 named a registered channel, so a 3+-part key has the
		// profile-scoped shape "<entity>:<channel>:<conversation>" and
		// parts[0] is the owner entity — stamp it (contract:
		// chanpkg.OwnerEntityMetadataKey). The lookup gate matters: an
		// anonymous threaded key "<channel>:<conversation>:<thread>" is also
		// 3-part, but its parts[0] is a channel id, not an owner.
		if len(parts) >= 3 {
			msg.Metadata = chanpkg.StampOwnerEntity(msg.Metadata, parts[0])
		}
	} else {
		// Fallback: from-right-2 doesn't name a registered channel (a
		// thread-capable channel shipped a non-empty threadID). Scan
		// registered channels and match by `:<channelID>:` substring so we
		// send to the right one instead of a fake channel named after the
		// conversationID. The search string gets a leading ":" so an
		// anonymous threaded key, whose channel id sits at position 0,
		// matches too. No owner stamp on this path: the key's leading
		// segment is not reliably an entity here.
		for _, ch := range n.reg.List() {
			needle := ":" + ch.ID() + ":"
			if idx := strings.Index(":"+sessionID, needle); idx >= 0 {
				channelID = ch.ID()
				rest := sessionID[idx+len(needle)-1:]
				if i := strings.Index(rest, ":"); i >= 0 {
					conversationID = rest[:i]
					if msg.ThreadID == "" {
						msg.ThreadID = rest[i+1:]
					}
				} else {
					conversationID = rest
				}
				break
			}
		}
	}
	if msg.ConversationID == "" {
		msg.ConversationID = conversationID
	}
	return n.reg.Send(ctx, channelID, msg)
}

// channelRunner adapts the orchestrator to channel.Runner.
type channelRunner struct {
	orch *orchestrator.Orchestrator
}

func (r *channelRunner) Run(ctx context.Context, sessionKey, content string, files ...chanpkg.FileAttachment) (string, string, map[string]string, error) {
	providerFiles := make([]provider.MessageFile, len(files))
	for i, f := range files {
		providerFiles[i] = provider.MessageFile{MimeType: f.MimeType, Data: f.Data}
	}
	result, err := r.orch.Run(ctx, sessionKey, content, providerFiles...)
	if err != nil {
		return "", "", nil, err
	}
	return result.Response, result.InputForDisplay, result.Metadata, nil
}

// defaultModelClient wraps a provider and sets req.Model when empty.
// It also injects model-level defaults (MaxTokens, ReasoningEffort)
// from the provider's model config when the request doesn't set them.
type defaultModelClient struct {
	provider provider.Provider
	model    string
	models   map[string]provider.ModelInfo
}

func (c *defaultModelClient) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if req.Model == "" {
		cp := *req
		cp.Model = c.model
		req = &cp
	}
	c.applyModelDefaults(req)
	return c.provider.Complete(ctx, req)
}

// Stream implements orchestrator.StreamingLLMClient by delegating to the
// underlying provider's Stream method, filling in the default model if needed.
func (c *defaultModelClient) Stream(ctx context.Context, req *provider.CompletionRequest) (provider.ResponseStream, error) {
	if req.Model == "" {
		cp := *req
		cp.Model = c.model
		cp.Stream = true
		req = &cp
	}
	c.applyModelDefaults(req)
	return c.provider.Stream(ctx, req)
}

// applyModelDefaults injects MaxTokens and ReasoningEffort from the model
// config when the request doesn't already set them.
func (c *defaultModelClient) applyModelDefaults(req *provider.CompletionRequest) {
	m, ok := c.models[req.Model]
	if !ok {
		return
	}
	if req.MaxTokens == 0 && m.MaxTokens > 0 {
		req.MaxTokens = m.MaxTokens
	}
	if req.ReasoningEffort == "" && m.ReasoningEffort != "" {
		req.ReasoningEffort = m.ReasoningEffort
	}
}

// SupportsFeature delegates to the underlying provider so the orchestrator
// can detect reasoning support via type assertion.
func (c *defaultModelClient) SupportsFeature(f provider.Feature) bool {
	return c.provider.SupportsFeature(f)
}

// buildLuaScriptPaths returns a map of Lua plugin name -> path to .lua script,
// from local scripts_dir and from plugins downloaded from GitHub.
func buildLuaScriptPaths(ctx context.Context, dataDir string, cfg *config.Config) map[string]string {
	paths := make(map[string]string)
	if cfg.Lua == nil {
		return paths
	}
	// Local scripts_dir: each .lua file -> name (without extension) -> path
	if cfg.Lua.ScriptsDir != "" {
		dir := cfg.Lua.ScriptsDir
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			rest := strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/")
			dir = filepath.Join(home, rest)
		}
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				name := e.Name()
				if strings.HasSuffix(name, ".lua") {
					pluginName := strings.TrimSuffix(name, ".lua")
					paths[pluginName] = filepath.Join(dir, name)
				}
			}
		}
	}
	// Downloaded plugins: default repo (subdir/name.lua) or per-plugin repo (name.lua at root)
	var defaultRepoPath string
	if cfg.Lua.DefaultGitHub != "" && cfg.Lua.DefaultRef != "" {
		p, err := bundle.EnsureLuaPluginsRepo(ctx, dataDir, cfg.Lua.DefaultGitHub, cfg.Lua.DefaultRef)
		if err != nil {
			slog.Warn("Lua plugins repo failed", "repo", cfg.Lua.DefaultGitHub, "error", err)
		} else {
			defaultRepoPath = p
		}
	}
	for _, plug := range cfg.Lua.Plugins {
		if plug.Name == "" {
			continue
		}
		if plug.GitHub != "" && plug.Ref != "" {
			pluginDir, err := bundle.EnsureLuaPluginDir(ctx, dataDir, plug.Name, plug.GitHub, plug.Ref)
			if err != nil {
				slog.Warn("Lua plugin failed", "plugin", plug.Name, "error", err)
				continue
			}
			paths[plug.Name] = filepath.Join(pluginDir, plug.Name+".lua")
		} else if defaultRepoPath != "" {
			paths[plug.Name] = filepath.Join(defaultRepoPath, plug.Name, plug.Name+".lua")
		}
	}
	return paths
}

// runClean clears cached bundles under the state data dir and exits.
func runClean(configPath, category string) {
	if configPath == "" {
		fmt.Fprintln(os.Stderr, "Error: -clean requires -config <path> so state.data_dir matches your deployment.")
		fmt.Fprintln(os.Stderr, "Example: opentalon -config /data/opentalon/config.yaml -clean plugins")
		os.Exit(1)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	absConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving config path: %v\n", err)
		os.Exit(1)
	}
	dataDir := config.ResolveStateDataDir(cfg, absConfigPath)

	switch category {
	case "all":
		fmt.Fprintf(os.Stderr, "Cleaning all cached bundles in %s...\n", dataDir)
		err = bundle.CleanAll(dataDir)
	case "plugins":
		fmt.Fprintf(os.Stderr, "Cleaning cached plugins in %s...\n", dataDir)
		err = bundle.CleanPlugins(dataDir)
	case "channels":
		fmt.Fprintf(os.Stderr, "Cleaning cached channels in %s...\n", dataDir)
		err = bundle.CleanChannels(dataDir)
	case "skills":
		fmt.Fprintf(os.Stderr, "Cleaning cached skills in %s...\n", dataDir)
		err = bundle.CleanSkills(dataDir)
	case "lua_plugins":
		fmt.Fprintf(os.Stderr, "Cleaning cached Lua plugins in %s...\n", dataDir)
		err = bundle.CleanLuaPlugins(dataDir)
	default:
		fmt.Fprintf(os.Stderr, "Unknown clean category %q. Use: all, plugins, channels, skills, lua_plugins\n", category)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "Clean failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Done. Next run will re-download from configured refs.")
}

// buildProvider returns a provider and the default model ID from config.
// buildProvider builds the LLM provider from config. When routing.fallbacks is
// empty it returns the single primary provider (unchanged behavior). Otherwise
// it builds the primary plus each fallback and wraps them in a health-gated
// provider that prefers the primary while its endpoint is reachable and falls
// back — with recovery hysteresis — to the fallbacks otherwise.
func buildProvider(cfg *config.Config, debugSink provider.DebugEventSink, debugResolve provider.DebugContextResolver, eventSink emit.Sink) (provider.Provider, string, error) {
	prov, modelID, primaryPC, err := buildProviderRef(cfg, cfg.Routing.Primary, debugSink, debugResolve, eventSink)
	if err != nil {
		return nil, "", err
	}
	if len(cfg.Routing.Fallbacks) == 0 {
		return prov, modelID, nil
	}

	entries := make([]provider.ProviderEntry, 0, 1+len(cfg.Routing.Fallbacks))
	entries = append(entries, provider.ProviderEntry{Prov: prov, Model: modelID})
	for _, ref := range cfg.Routing.Fallbacks {
		fp, fModel, _, ferr := buildProviderRef(cfg, ref, debugSink, debugResolve, eventSink)
		if ferr != nil {
			return nil, "", fmt.Errorf("routing fallback %q: %w", ref, ferr)
		}
		entries = append(entries, provider.ProviderEntry{Prov: fp, Model: fModel})
	}

	probePath := cfg.Routing.Health.Path
	if probePath == "" {
		probePath = "/models"
	}
	probeURL := strings.TrimRight(primaryPC.BaseURL, "/") + probePath
	probe := provider.NewHTTPHealthProbe(probeURL, primaryPC.APIKey, nil)
	gate := provider.HealthGateConfig{
		Interval:     parseDurationOrZero(cfg.Routing.Health.Interval),
		Timeout:      parseDurationOrZero(cfg.Routing.Health.Timeout),
		RecoverAfter: cfg.Routing.Health.RecoverAfter,
	}
	slog.Info("llm routing: health-gated fallback enabled",
		"primary", cfg.Routing.Primary,
		"fallbacks", cfg.Routing.Fallbacks,
		"health_probe", probeURL)
	return provider.NewHealthGatedProvider(context.Background(), entries, probe, gate, slog.Default()), modelID, nil
}

// buildProviderRef builds a single provider from a "providerID" or
// "providerID/modelID" routing ref. An empty ref selects the first configured
// provider. It returns the provider, the resolved model id, and the provider's
// config entry (used by the caller to wire the health probe against its endpoint).
func buildProviderRef(cfg *config.Config, ref string, debugSink provider.DebugEventSink, debugResolve provider.DebugContextResolver, eventSink emit.Sink) (provider.Provider, string, config.ProviderConfig, error) {
	providerID := ""
	modelID := ""

	if ref != "" {
		parts := strings.SplitN(ref, "/", 2)
		providerID = parts[0]
		if len(parts) == 2 {
			modelID = parts[1]
		}
	}
	if providerID == "" {
		for id := range cfg.Models.Providers {
			providerID = id
			break
		}
	}
	if providerID == "" {
		return nil, "", config.ProviderConfig{}, fmt.Errorf("no provider configured in models.providers")
	}

	pc, ok := cfg.Models.Providers[providerID]
	if !ok {
		return nil, "", config.ProviderConfig{}, fmt.Errorf("provider %q not found", providerID)
	}

	if modelID == "" {
		if len(pc.Models) > 0 {
			modelID = pc.Models[0].ID
		} else {
			for r := range cfg.Models.Catalog {
				if strings.HasPrefix(r, providerID+"/") {
					modelID = strings.TrimPrefix(r, providerID+"/")
					break
				}
			}
		}
	}
	if modelID == "" {
		modelID = "default"
	}

	models := make([]provider.ModelInfo, 0, len(pc.Models))
	for _, m := range pc.Models {
		features := make([]provider.Feature, len(m.Features))
		for i, f := range m.Features {
			features[i] = provider.Feature(f)
		}
		models = append(models, provider.ModelInfo{
			ID:              m.ID,
			Name:            m.Name,
			ProviderID:      providerID,
			Reasoning:       m.Reasoning,
			ReasoningEffort: m.ReasoningEffort,
			InputTypes:      m.InputTypes,
			ContextWindow:   m.ContextWindow,
			MaxTokens:       m.MaxTokens,
			Cost:            provider.ModelCost{Input: m.Cost.Input, Output: m.Cost.Output},
			Features:        features,
		})
	}

	provCfg := provider.ProviderConfig{
		ID:           providerID,
		BaseURL:      pc.BaseURL,
		APIKey:       pc.APIKey,
		API:          pc.API,
		Models:       models,
		DebugSink:    debugSink,
		DebugResolve: debugResolve,
		EventSink:    eventSink,
		Retry: provider.RetryPolicy{
			MaxAttempts:  pc.Retry.MaxAttempts,
			BaseDelay:    parseDurationOrZero(pc.Retry.BaseDelay),
			MaxDelay:     parseDurationOrZero(pc.Retry.MaxDelay),
			MaxTotalWait: parseDurationOrZero(pc.Retry.MaxTotalWait),
		},
	}
	prov, err := provider.FromConfig(provCfg)
	if err != nil {
		return nil, "", config.ProviderConfig{}, err
	}
	return prov, modelID, pc, nil
}

// parseDurationOrZero parses a Go duration string, returning 0 (which the
// consumer maps to its default) on empty or invalid input.
func parseDurationOrZero(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		slog.Warn("invalid duration in config; falling back to default", "value", s, "error", err)
		return 0
	}
	return d
}

func defaultInt(v, fallback int) int {
	if v != 0 {
		return v
	}
	return fallback
}
