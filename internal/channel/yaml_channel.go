package channel

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"gopkg.in/yaml.v3"
)

// LoadYAMLChannelSpec reads and parses a channel.yaml file.
func LoadYAMLChannelSpec(path string) (*YAMLChannelSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read channel spec %s: %w", path, err)
	}
	var spec YAMLChannelSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse channel spec %s: %w", path, err)
	}
	if spec.ID == "" {
		return nil, fmt.Errorf("channel spec %s: missing id", path)
	}
	return &spec, nil
}

// YAMLChannel implements pkg.Channel, pkg.ConfigurableChannel, and
// pkg.ToolProvider for YAML-driven channels that run in-process.
type YAMLChannel struct {
	spec     *YAMLChannelSpec
	specDir  string // directory containing the spec file (for resolving tools_file)
	config   map[string]string
	selfVars map[string]string
	dedup    *Deduplicator
	tools    []pkg.ToolDefinition
	client   *http.Client
	inbox    chan<- pkg.InboundMessage
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewYAMLChannel creates a new YAMLChannel from a parsed spec.
func NewYAMLChannel(spec *YAMLChannelSpec, specDir string) *YAMLChannel {
	return &YAMLChannel{
		spec:     spec,
		specDir:  specDir,
		config:   make(map[string]string),
		selfVars: make(map[string]string),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

// ID returns the channel identifier from the spec.
func (ch *YAMLChannel) ID() string {
	return ch.spec.ID
}

// Capabilities returns what this channel supports.
func (ch *YAMLChannel) Capabilities() pkg.Capabilities {
	return pkg.Capabilities{
		ID:               ch.spec.ID,
		Name:             ch.spec.Name,
		Threads:          ch.spec.Capabilities.Threads,
		Files:            ch.spec.Capabilities.Files,
		Reactions:        ch.spec.Capabilities.Reactions,
		Edits:            ch.spec.Capabilities.Edits,
		MaxMessageLength: int64(ch.spec.Capabilities.MaxMessageLength),
	}
}

// Configure receives the config map from the host config.yaml and loads tools.
func (ch *YAMLChannel) Configure(cfg map[string]interface{}) error {
	// Flatten config to string map for template substitution
	for k, v := range cfg {
		ch.config[k] = fmt.Sprintf("%v", v)
	}

	// Validate required env vars
	for _, name := range ch.spec.RequiredEnv {
		if os.Getenv(name) == "" {
			return fmt.Errorf("channel %s: required env var %s is not set", ch.spec.ID, name)
		}
	}

	// Load tools from tools_file if specified
	if ch.spec.ToolsFile != "" {
		tools, err := LoadToolDefsFromFile(ch.spec.ToolsFile, ch.specDir)
		if err != nil {
			return fmt.Errorf("channel %s: load tools: %w", ch.spec.ID, err)
		}
		ch.tools = tools
	}

	return nil
}

// Tools returns the channel's tool definitions.
func (ch *YAMLChannel) Tools() []pkg.ToolDefinition {
	return ch.tools
}

// Start initializes the channel: runs init steps, connects WebSocket,
// and starts the read loop.
func (ch *YAMLChannel) Start(ctx context.Context, inbox chan<- pkg.InboundMessage) error {
	ch.ctx, ch.cancel = context.WithCancel(ctx)
	ch.inbox = inbox

	// Initialize dedup if configured
	if ch.spec.Inbound.Dedup.Key != "" {
		ch.dedup = NewDeduplicator(ch.spec.Inbound.Dedup.TTL)
	}

	// Run all init steps
	if err := ch.runInit(ch.spec.Init); err != nil {
		return fmt.Errorf("channel %s init: %w", ch.spec.ID, err)
	}

	// Start WebSocket connection in background
	ch.wg.Add(1)
	go func() {
		defer ch.wg.Done()
		ch.wsLoop()
	}()

	log.Printf("yaml-channel: %s started", ch.spec.ID)
	return nil
}

// Send chunks and sends a message via the outbound HTTP call,
// then runs on_response hooks.
func (ch *YAMLChannel) Send(ctx context.Context, msg pkg.OutboundMessage) error {
	chunks := ChunkMessage(msg.Content, ch.spec.Outbound.Chunking.MaxLength)

	msgCtx := map[string]string{
		"conversation_id": msg.ConversationID,
		"thread_id":       msg.ThreadID,
	}
	for k, v := range msg.Metadata {
		msgCtx["metadata."+k] = v
	}

	for _, chunk := range chunks {
		msgCtx["content"] = chunk
		contexts := ch.buildContexts()
		contexts["msg"] = msgCtx

		if err := ch.doHTTPCall(ctx, ch.spec.Outbound.Send, contexts); err != nil {
			return fmt.Errorf("channel %s send: %w", ch.spec.ID, err)
		}
	}

	// Run on_response hooks (fire-and-forget)
	ch.runHooks(ch.spec.Hooks.OnResponse, msgCtx)

	return nil
}

// Stop gracefully shuts down the channel.
func (ch *YAMLChannel) Stop() error {
	if ch.cancel != nil {
		ch.cancel()
	}
	ch.wg.Wait()
	log.Printf("yaml-channel: %s stopped", ch.spec.ID)
	return nil
}

// buildContexts returns the standard template contexts (env is handled
// directly by substituteTemplate, so not included here).
func (ch *YAMLChannel) buildContexts() map[string]map[string]string {
	return map[string]map[string]string{
		"self":   ch.selfVars,
		"config": ch.config,
	}
}

// specPath returns the absolute path to the spec's directory.
func specDirFromPath(specPath string) string {
	abs, err := filepath.Abs(specPath)
	if err != nil {
		return filepath.Dir(specPath)
	}
	return filepath.Dir(abs)
}
