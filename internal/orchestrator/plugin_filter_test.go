package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
)

// staticGroupLookup is a fake GroupPluginLookup backed by a static map.
type staticGroupLookup struct {
	m map[string][]string // group → plugin IDs
}

func (s *staticGroupLookup) PluginsForGroup(_ context.Context, group string) ([]string, error) {
	return s.m[group], nil
}

// buildFilterRegistry creates a registry with three plugins:
//
//	"public"     — no AllowedGroups (always visible in non-strict mode)
//	"restricted" — AllowedGroups set (gated in non-strict mode)
//	"mymcp"      — no AllowedGroups (always visible in non-strict, gated in strict)
func buildFilterRegistry() *ToolRegistry {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:        "public",
		Description: "A public plugin",
		Actions:     []Action{{Name: "go", Description: "do it"}},
	}, &echoExecutor{})
	_ = reg.Register(PluginCapability{
		Name:          "restricted",
		Description:   "A group-restricted plugin",
		AllowedGroups: []string{"admins"},
		Actions:       []Action{{Name: "go", Description: "do it"}},
	}, &echoExecutor{})
	_ = reg.Register(PluginCapability{
		Name:        "mymcp",
		Description: "An MCP plugin",
		Actions:     []Action{{Name: "call", Description: "call it"}},
	}, &echoExecutor{})
	return reg
}

func capByName(reg *ToolRegistry, name string) (PluginCapability, bool) {
	for _, c := range reg.ListCapabilities() {
		if c.Name == name {
			return c, true
		}
	}
	return PluginCapability{}, false
}

func newFilterOrch(reg *ToolRegistry, lookup GroupPluginLookup) *Orchestrator {
	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	return NewWithRules(
		&fakeLLM{responses: []string{"done"}},
		&fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess,
		OrchestratorOpts{GroupPluginLookup: lookup},
	)
}

// --- resolveAllowedPlugins ---

func TestResolveAllowedPlugins_NoProfile(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	got := o.resolveAllowedPlugins(context.Background())
	if got.m != nil || got.strict {
		t.Errorf("no profile: want zero cachedAllowedPlugins, got %+v", got)
	}
}

func TestResolveAllowedPlugins_WhoAmIPlugins_StrictMode(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"mymcp"}}
	ctx := profile.WithProfile(context.Background(), p)

	got := o.resolveAllowedPlugins(ctx)

	if !got.strict {
		t.Error("strict should be true when Profile.Plugins is set")
	}
	if !got.m["mymcp"] {
		t.Error("mymcp should be in the allowed map")
	}
	if got.m["public"] || got.m["restricted"] {
		t.Error("only WhoAmI-listed plugins should be in the map")
	}
}

func TestResolveAllowedPlugins_WhoAmIEmptyList_StrictDenyAll(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	// Profile.Plugins is non-nil but empty → user has access to no plugins.
	p := &profile.Profile{EntityID: "u1", Plugins: []string{}}
	ctx := profile.WithProfile(context.Background(), p)

	got := o.resolveAllowedPlugins(ctx)

	if !got.strict {
		t.Error("strict should be true even for empty Plugins list")
	}
	if len(got.m) != 0 {
		t.Errorf("map should be empty, got %v", got.m)
	}
}

func TestResolveAllowedPlugins_NilPlugins_FallsBackToGroupDB(t *testing.T) {
	lookup := &staticGroupLookup{m: map[string][]string{"devs": {"restricted"}}}
	o := newFilterOrch(buildFilterRegistry(), lookup)
	// Profile.Plugins is nil → fall back to DB lookup.
	p := &profile.Profile{EntityID: "u1", Group: "devs"}
	ctx := profile.WithProfile(context.Background(), p)

	got := o.resolveAllowedPlugins(ctx)

	if got.strict {
		t.Error("strict should be false when falling back to DB lookup")
	}
	if !got.m["restricted"] {
		t.Error("restricted should be in the map from DB")
	}
}

func TestResolveAllowedPlugins_CachedResult(t *testing.T) {
	calls := 0
	lookup := &countingGroupLookup{
		inner: &staticGroupLookup{m: map[string][]string{"g": {"mymcp"}}},
		calls: &calls,
	}
	o := newFilterOrch(buildFilterRegistry(), lookup)
	p := &profile.Profile{EntityID: "u1", Group: "g"}
	ctx := profile.WithProfile(context.Background(), p)

	// Prime the cache.
	first := o.resolveAllowedPlugins(ctx)
	ctx = withAllowedPlugins(ctx, first)

	// Second call must hit the cache, not the DB.
	_ = o.resolveAllowedPlugins(ctx)
	if calls != 1 {
		t.Errorf("DB called %d times, want 1 (second call should use cache)", calls)
	}
}

// callbackExecutor is a PluginExecutor that delegates to an arbitrary function.
type callbackExecutor struct {
	fn func(ToolCall) ToolResult
}

func (c *callbackExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return c.fn(call)
}

type countingGroupLookup struct {
	inner GroupPluginLookup
	calls *int
}

func (c *countingGroupLookup) PluginsForGroup(ctx context.Context, group string) ([]string, error) {
	*c.calls++
	return c.inner.PluginsForGroup(ctx, group)
}

// --- pluginAllowed ---

func TestPluginAllowed_NilMap_Unrestricted(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	for _, cap := range buildFilterRegistry().ListCapabilities() {
		if !o.pluginAllowed(cap, cachedAllowedPlugins{}) {
			t.Errorf("plugin %q should be allowed when map is nil", cap.Name)
		}
	}
}

func TestPluginAllowed_StrictMode_OnlyListedPlugins(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	allowed := cachedAllowedPlugins{m: map[string]bool{"mymcp": true}, strict: true}

	tests := []struct {
		name string
		want bool
	}{
		{"mymcp", true},
		{"public", false},
		{"restricted", false},
	}
	for _, tc := range tests {
		cap, ok := capByName(buildFilterRegistry(), tc.name)
		if !ok {
			t.Fatalf("capability %q not found", tc.name)
		}
		if got := o.pluginAllowed(cap, allowed); got != tc.want {
			t.Errorf("pluginAllowed(%q) = %v, want %v (strict)", tc.name, got, tc.want)
		}
	}
}

func TestPluginAllowed_NonStrictMode_PublicAlwaysVisible(t *testing.T) {
	o := newFilterOrch(buildFilterRegistry(), nil)
	// Non-strict: only "restricted" (AllowedGroups set) is gated.
	allowed := cachedAllowedPlugins{m: map[string]bool{"mymcp": true}, strict: false}

	tests := []struct {
		name string
		want bool
	}{
		{"public", true},      // no AllowedGroups → always visible
		{"mymcp", true},       // no AllowedGroups → always visible
		{"restricted", false}, // AllowedGroups set, not in map → blocked
	}
	for _, tc := range tests {
		cap, ok := capByName(buildFilterRegistry(), tc.name)
		if !ok {
			t.Fatalf("capability %q not found", tc.name)
		}
		if got := o.pluginAllowed(cap, allowed); got != tc.want {
			t.Errorf("pluginAllowed(%q) = %v, want %v (non-strict)", tc.name, got, tc.want)
		}
	}
}

// --- pluginAllowed with aliases ---

func TestPluginAllowed_StrictMode_AliasInAllowlist(t *testing.T) {
	reg := buildFilterRegistry()
	// Register "mcp" with an alias "mymcp-alias"
	_ = reg.Register(PluginCapability{
		Name:    "mcpbridge",
		Actions: []Action{{Name: "call", Description: "call it"}},
	}, &echoExecutor{})
	_ = reg.RegisterAlias("my-jira", "mcpbridge")

	o := newFilterOrch(reg, nil)

	// Strict mode: allowlist has "my-jira" (alias), not "mcpbridge" (parent).
	allowed := cachedAllowedPlugins{m: map[string]bool{"my-jira": true}, strict: true}

	// The parent capability should be allowed because its alias is in the allowlist.
	// capByName won't find it via ListCapabilities (replaced by alias), construct directly.
	parentCap := PluginCapability{Name: "mcpbridge"}
	if !o.pluginAllowed(parentCap, allowed) {
		t.Error("mcpbridge should be allowed via its alias my-jira being in the allowlist")
	}

	// The alias capability itself should also be allowed.
	aliasCap, ok := reg.GetCapability("my-jira")
	if !ok {
		t.Fatal("expected to find alias capability")
	}
	if !o.pluginAllowed(aliasCap, allowed) {
		t.Error("my-jira alias capability should be allowed directly")
	}
}

// --- end-to-end: system prompt ---

func TestSystemPrompt_StrictMode_OnlyListedPluginsVisible(t *testing.T) {
	reg := buildFilterRegistry()
	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-prompt", "", "")

	llm := &capturingLLM{responses: []string{"done"}}
	o := NewWithRules(llm, &fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess, OrchestratorOpts{})

	p := &profile.Profile{EntityID: "u1", Plugins: []string{"mymcp"}}
	ctx := profile.WithProfile(context.Background(), p)

	if _, err := o.Run(ctx, "s-prompt", "hello"); err != nil {
		t.Fatal(err)
	}

	if len(llm.requests) == 0 {
		t.Fatal("LLM was never called")
	}
	system := llm.requests[0].Messages[0].Content

	if !strings.Contains(system, "mymcp") {
		t.Error("system prompt should mention mymcp")
	}
	if strings.Contains(system, "## public") {
		t.Error("system prompt should NOT mention public (strict mode, not in WhoAmI list)")
	}
	if strings.Contains(system, "## restricted") {
		t.Error("system prompt should NOT mention restricted (strict mode, not in WhoAmI list)")
	}
}

// --- end-to-end: internal calls exempt from strict allowlist ---

// TestExecute_StrictMode_GuardNotInWhoAmI_StillRuns verifies that a guard
// plugin configured by the host is not blocked by the strict-mode allowlist
// even when it is absent from the WhoAmI Plugins list. Internal calls
// (FromLLM == false) must be exempt so that guards, preparers, and pipelines
// work correctly regardless of what the profile exposes to the LLM.
func TestExecute_StrictMode_GuardNotInWhoAmI_StillRuns(t *testing.T) {
	reg := buildFilterRegistry()
	// Register an additional "xss-guard" plugin that is NOT in the WhoAmI list.
	guardCalled := false
	_ = reg.Register(PluginCapability{
		Name:        "xss-guard",
		Description: "Content guard",
		Actions:     []Action{{Name: "check", Description: "check content"}},
	}, &callbackExecutor{fn: func(_ ToolCall) ToolResult {
		guardCalled = true
		return ToolResult{Content: `{"send_to_llm": true}`}
	}})

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-guard", "", "")

	llm := &capturingLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(_ string) []ToolCall { return nil }}
	o := NewWithRules(llm, parser, reg, mem, sess, OrchestratorOpts{
		ContentPreparers: []ContentPreparerEntry{
			{Plugin: "xss-guard", Action: "check", Guard: true},
		},
	})

	// Strict mode: only "mymcp" is in the WhoAmI list; "xss-guard" is not.
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"mymcp"}}
	ctx := profile.WithProfile(context.Background(), p)

	if _, err := o.Run(ctx, "s-guard", "hello"); err != nil {
		t.Fatal(err)
	}
	if !guardCalled {
		t.Error("guard plugin should have been called even though it is not in the WhoAmI Plugins list")
	}
}

func TestSystemPrompt_StrictMode_MCPAliasVisible(t *testing.T) {
	reg := NewToolRegistry()
	_ = reg.Register(PluginCapability{
		Name:        "mcp",
		Description: "MCP bridge",
		Actions:     []Action{{Name: "search_issues", Description: "Search Jira issues"}},
	}, &echoExecutor{})
	_ = reg.RegisterAlias("jira", "mcp")

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-alias", "", "")

	llm := &capturingLLM{responses: []string{"done"}}
	o := NewWithRules(llm, &fakeParser{parseFn: func(_ string) []ToolCall { return nil }},
		reg, mem, sess, OrchestratorOpts{})

	// WhoAmI says plugins=["jira"] — which is an alias for "mcp".
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"jira"}}
	ctx := profile.WithProfile(context.Background(), p)

	if _, err := o.Run(ctx, "s-alias", "hello"); err != nil {
		t.Fatal(err)
	}

	if len(llm.requests) == 0 {
		t.Fatal("LLM was never called")
	}
	system := llm.requests[0].Messages[0].Content

	if !strings.Contains(system, "jira") {
		t.Error("system prompt should mention 'jira' (alias name)")
	}
	if strings.Contains(system, "## mcp") {
		t.Error("system prompt should NOT mention 'mcp' (parent is replaced by alias)")
	}
	if !strings.Contains(system, "search_issues") {
		t.Error("system prompt should include actions from the MCP plugin under the jira alias")
	}
}

func TestExecute_StrictMode_MCPAlias_ToolCallAllowed(t *testing.T) {
	reg := NewToolRegistry()
	mcpExec := &callbackExecutor{fn: func(call ToolCall) ToolResult {
		return ToolResult{CallID: call.ID, Content: "jira result"}
	}}
	_ = reg.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "search_issues", Description: "Search"}},
	}, mcpExec)
	_ = reg.RegisterAlias("jira", "mcp")

	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-alias-exec", "", "")

	callNum := 0
	llm := &capturingLLM{responses: []string{"[tool] jira.search_issues", "done"}}
	parser := &fakeParser{parseFn: func(_ string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "c1", Plugin: "jira", Action: "search_issues", FromLLM: true}}
		}
		return nil
	}}
	o := NewWithRules(llm, parser, reg, mem, sess, OrchestratorOpts{})

	// WhoAmI: plugins=["jira"]
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"jira"}}
	ctx := profile.WithProfile(context.Background(), p)

	result, err := o.Run(ctx, "s-alias-exec", "search jira")
	if err != nil {
		t.Fatal(err)
	}

	// Verify the tool call was executed, not blocked.
	if len(result.Results) == 0 {
		t.Fatal("expected at least one tool result")
	}
	if result.Results[0].Error != "" {
		t.Errorf("tool call should not have been blocked, got error: %s", result.Results[0].Error)
	}
	if result.Results[0].Content != "jira result" {
		t.Errorf("Content = %q, want 'jira result'", result.Results[0].Content)
	}
}

// --- end-to-end: tool call blocking ---

func TestExecute_StrictMode_BlocksUnlistedPlugin(t *testing.T) {
	reg := buildFilterRegistry()
	mem := state.NewMemoryStore("")
	sess := state.NewSessionStore("")
	sess.Create("s-block", "", "")

	callNum := 0
	llm := &capturingLLM{responses: []string{"[tool] public.go", "done"}}
	parser := &fakeParser{parseFn: func(_ string) []ToolCall {
		callNum++
		if callNum == 1 {
			return []ToolCall{{ID: "c1", Plugin: "public", Action: "go", FromLLM: true}}
		}
		return nil
	}}
	o := NewWithRules(llm, parser, reg, mem, sess, OrchestratorOpts{})

	// Only mymcp is allowed; "public" has no AllowedGroups but must be blocked in strict mode.
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"mymcp"}}
	ctx := profile.WithProfile(context.Background(), p)

	if _, err := o.Run(ctx, "s-block", "go"); err != nil {
		t.Fatal(err)
	}

	// The block fires and the tool result error is fed back to the LLM in the next request.
	// Verify the second LLM call received a message containing the block reason.
	if len(llm.requests) < 2 {
		t.Fatalf("expected at least 2 LLM calls (tool call + retry), got %d", len(llm.requests))
	}
	found := false
	for _, msg := range llm.requests[1].Messages {
		if strings.Contains(msg.Content, "not available") {
			found = true
			break
		}
	}
	if !found {
		t.Error("second LLM request should contain the 'not available' block message")
	}
}
