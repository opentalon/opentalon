package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/opentalon/opentalon/internal/bundle"
	"github.com/opentalon/opentalon/internal/channel"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/plugin"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/requestpkg"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/version"
)

func main() {
	fmt.Fprintln(os.Stderr, "OpenTalon starting...")
	configPath := flag.String("config", "", "path to config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Get())
		os.Exit(0)
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

	// When LOG_LEVEL=debug, optionally send logs to a file (so you can see what we send to the LLM).
	if os.Getenv("LOG_LEVEL") == "debug" && cfg.Log.File != "" {
		logPath := cfg.Log.File
		if strings.HasPrefix(logPath, "~") {
			home, _ := os.UserHomeDir()
			rest := strings.TrimPrefix(strings.TrimPrefix(logPath, "~"), "/")
			logPath = filepath.Join(home, rest)
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0750); err == nil {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				log.SetOutput(f)
			}
		}
	}

	// Build LLM provider and default model from config
	prov, defaultModel, err := buildProvider(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building provider: %v\n", err)
		os.Exit(1)
	}

	// LLM client that sets default model when orchestrator doesn't
	llm := &defaultModelClient{provider: prov, model: defaultModel}

	dataDir := cfg.State.DataDir
	sessions := state.NewSessionStore(dataDir)
	memory := state.NewMemoryStore(dataDir)
	_ = memory.Load()

	// Sessions created on first message per channel (session key from channel ID)

	toolRegistry := orchestrator.NewToolRegistry()

	// Load tool plugins (path from config or from github+ref via plugins.lock)
	ctx := context.Background()
	pluginEntries := make([]plugin.PluginEntry, 0, len(cfg.Plugins))
	for name, p := range cfg.Plugins {
		path := p.Path
		if p.GitHub != "" && p.Ref != "" {
			resolvedPath, err := bundle.EnsurePlugin(ctx, dataDir, name, p.GitHub, p.Ref)
			if err != nil {
				log.Printf("Warning: bundle plugin %s: %v", name, err)
				continue
			}
			path = resolvedPath
		}
		if path == "" {
			log.Printf("Warning: plugin %s has no path and no github/ref", name)
			continue
		}
		pluginEntries = append(pluginEntries, plugin.PluginEntry{
			Name: name, Path: path, Enabled: p.Enabled, Config: p.Config,
		})
	}
	pluginManager := plugin.NewManager(toolRegistry)
	if err := pluginManager.LoadAll(ctx, pluginEntries); err != nil {
		log.Printf("Warning: some plugins failed to load: %v", err)
	}

	// Request packages (skill-style): no compiled plugin, core runs HTTP requests from config/dir
	var requestSets []requestpkg.Set
	if cfg.RequestPackages.Path != "" {
		dirSets, err := requestpkg.LoadDir(cfg.RequestPackages.Path)
		if err != nil {
			log.Printf("Warning: request_packages path %q: %v", cfg.RequestPackages.Path, err)
		} else {
			requestSets = append(requestSets, dirSets...)
		}
	}
	if cfg.RequestPackages.SkillsPath != "" {
		skillSets, err := requestpkg.LoadSkillsDir(cfg.RequestPackages.SkillsPath)
		if err != nil {
			log.Printf("Warning: request_packages skills_path %q: %v", cfg.RequestPackages.SkillsPath, err)
		} else {
			requestSets = append(requestSets, skillSets...)
		}
	}
	// Download skills by name (from default repo or per-skill github/ref)
	var defaultRepoPath string
	if cfg.RequestPackages.DefaultSkillGitHub != "" && cfg.RequestPackages.DefaultSkillRef != "" {
		p, err := bundle.EnsureSkillsRepo(ctx, dataDir, cfg.RequestPackages.DefaultSkillGitHub, cfg.RequestPackages.DefaultSkillRef)
		if err != nil {
			log.Printf("Warning: skills repo %s: %v", cfg.RequestPackages.DefaultSkillGitHub, err)
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
				log.Printf("Warning: skill %s: %v", skill.Name, err)
				continue
			}
			skillDir = path
		case defaultRepoPath != "":
			skillDir = filepath.Join(defaultRepoPath, skill.Name)
		default:
			log.Printf("Warning: skill %s has no github/ref and no default_skill_github/ref", skill.Name)
			continue
		}
		set, err := requestpkg.LoadSkillDir(skillDir)
		if err != nil {
			log.Printf("Warning: load skill %s from %s: %v", skill.Name, skillDir, err)
			continue
		}
		requestSets = append(requestSets, set)
	}
	for _, inl := range cfg.RequestPackages.Inline {
		set := requestpkg.Set{PluginName: inl.Plugin, Description: inl.Description}
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
	if err := requestpkg.Register(toolRegistry, requestSets); err != nil {
		log.Printf("Warning: request_packages: %v", err)
	}

	contentPreparers := make([]orchestrator.ContentPreparerEntry, 0, len(cfg.Orchestrator.ContentPreparers))
	for _, p := range cfg.Orchestrator.ContentPreparers {
		contentPreparers = append(contentPreparers, orchestrator.ContentPreparerEntry{
			Plugin: p.Plugin,
			Action: p.Action,
			ArgKey: p.ArgKey,
		})
	}
	luaScriptPaths := buildLuaScriptPaths(ctx, dataDir, cfg)
	orch := orchestrator.NewWithRules(llm, orchestrator.DefaultParser, toolRegistry, memory, sessions, cfg.Orchestrator.Rules, contentPreparers, luaScriptPaths)

	ensureSession := func(sessionKey string) {
		if _, err := sessions.Get(sessionKey); err != nil {
			sessions.Create(sessionKey)
		}
	}
	runner := &channelRunner{orch: orch}
	handler := channel.NewMessageHandler(ensureSession, runner, orch.RunAction, toolRegistry.HasAction)

	reg := channel.NewRegistry(handler)
	channelManager := channel.NewManager(reg)
	channelEntries := make([]channel.ChannelEntry, 0, len(cfg.Channels))
	for name, ch := range cfg.Channels {
		pathRef := ch.Path
		if ch.GitHub != "" && ch.Ref != "" {
			resolvedPath, err := bundle.EnsureChannel(ctx, dataDir, name, ch.GitHub, ch.Ref)
			if err != nil {
				log.Printf("Warning: bundle channel %s: %v", name, err)
				continue
			}
			pathRef = resolvedPath
		}
		if pathRef == "" {
			log.Printf("Warning: channel %s has no path and no github/ref", name)
			continue
		}
		channelEntries = append(channelEntries, channel.ChannelEntry{
			Name: name, Path: pathRef, Enabled: ch.Enabled, Config: ch.Config,
		})
	}
	if err := channelManager.LoadAll(ctx, channelEntries); err != nil {
		log.Printf("Warning: some channels failed to load: %v", err)
	} else {
		log.Printf("Channels loaded. Use the console prompt below (or Ctrl+C to stop).")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	channelManager.StopAll()
	pluginManager.StopAll()
}

// channelRunner adapts the orchestrator to channel.Runner.
type channelRunner struct {
	orch *orchestrator.Orchestrator
}

func (r *channelRunner) Run(ctx context.Context, sessionKey, content string) (string, string, error) {
	result, err := r.orch.Run(ctx, sessionKey, content)
	if err != nil {
		return "", "", err
	}
	return result.Response, result.InputForDisplay, nil
}

// defaultModelClient wraps a provider and sets req.Model when empty.
type defaultModelClient struct {
	provider provider.Provider
	model    string
}

func (c *defaultModelClient) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if req.Model == "" {
		req = &provider.CompletionRequest{
			Model:       c.model,
			Messages:    req.Messages,
			MaxTokens:   req.MaxTokens,
			Temperature: req.Temperature,
			Stream:      req.Stream,
		}
	}
	return c.provider.Complete(ctx, req)
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
			log.Printf("Warning: Lua plugins repo %s: %v", cfg.Lua.DefaultGitHub, err)
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
				log.Printf("Warning: Lua plugin %s: %v", plug.Name, err)
				continue
			}
			paths[plug.Name] = filepath.Join(pluginDir, plug.Name+".lua")
		} else if defaultRepoPath != "" {
			paths[plug.Name] = filepath.Join(defaultRepoPath, plug.Name, plug.Name+".lua")
		}
	}
	return paths
}

// buildProvider returns a provider and the default model ID from config.
func buildProvider(cfg *config.Config) (provider.Provider, string, error) {
	providerID := ""
	modelID := ""

	if cfg.Routing.Primary != "" {
		parts := strings.SplitN(cfg.Routing.Primary, "/", 2)
		if len(parts) == 2 {
			providerID = parts[0]
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
		return nil, "", fmt.Errorf("no provider configured in models.providers")
	}

	pc, ok := cfg.Models.Providers[providerID]
	if !ok {
		return nil, "", fmt.Errorf("provider %q not found", providerID)
	}

	if modelID == "" {
		if len(pc.Models) > 0 {
			modelID = pc.Models[0].ID
		} else {
			for ref := range cfg.Models.Catalog {
				if strings.HasPrefix(ref, providerID+"/") {
					modelID = strings.TrimPrefix(ref, providerID+"/")
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
		models = append(models, provider.ModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ProviderID:    providerID,
			Reasoning:     m.Reasoning,
			InputTypes:    m.InputTypes,
			ContextWindow: m.ContextWindow,
			MaxTokens:     m.MaxTokens,
			Cost:          provider.ModelCost{Input: m.Cost.Input, Output: m.Cost.Output},
		})
	}

	provCfg := provider.ProviderConfig{
		ID:      providerID,
		BaseURL: pc.BaseURL,
		APIKey:  pc.APIKey,
		API:     pc.API,
		Models:  models,
	}
	prov, err := provider.FromConfig(provCfg)
	if err != nil {
		return nil, "", err
	}
	return prov, modelID, nil
}
