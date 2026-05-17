package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

func newOrchForMetaTests(t *testing.T, store *fakeInjectionStateStore, enable bool) *Orchestrator {
	t.Helper()
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "tools-plugin", Description: "Five business tools",
		Actions: []Action{
			{Name: "t1", Description: "Tool one detailed description.", Parameters: []Parameter{
				{Name: "arg1", Description: "first arg", Required: true},
				{Name: "arg2", Description: "second arg", Required: false},
			}},
			{Name: "t2", Description: "Tool two."},
		},
	}, &fixedResultExecutor{content: "result"})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	opts := OrchestratorOpts{
		ToolTiers: ToolTiersConfig{Enabled: true, EnableGetToolDetails: enable},
	}
	if store != nil {
		opts.InjectionStateStore = store
	}
	return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, opts)
}

func TestRegisterGetToolDetails_RegistersMetaPluginWithAlwaysInclude(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	cap, ok := orch.registry.GetCapability(metaPluginName)
	if !ok {
		t.Fatalf("meta plugin %q must be registered when EnableGetToolDetails=true", metaPluginName)
	}
	if len(cap.Actions) != 1 || cap.Actions[0].Name != metaGetToolDetails {
		t.Fatalf("meta plugin must expose exactly %q action, got %+v", metaGetToolDetails, cap.Actions)
	}
	if !cap.Actions[0].AlwaysInclude {
		t.Errorf("get_tool_details action must be AlwaysInclude=true so the tier decision pins it to Tier 0")
	}
	if len(cap.Actions[0].Parameters) != 1 || !cap.Actions[0].Parameters[0].Required {
		t.Errorf("get_tool_details must require a single 'name' parameter, got %+v", cap.Actions[0].Parameters)
	}
}

func TestRegisterGetToolDetails_NotRegisteredWhenFlagOff(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, false)
	if _, ok := orch.registry.GetCapability(metaPluginName); ok {
		t.Errorf("meta plugin must NOT be registered when EnableGetToolDetails=false")
	}
}

func TestGetToolDetails_ReturnsFormattedDescription(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, ok := orch.registry.GetExecutor(metaPluginName)
	if !ok {
		t.Fatal("meta plugin executor missing")
	}
	res := exec.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "tools-plugin.t1"},
	})
	if res.Error != "" {
		t.Fatalf("expected no error, got %q", res.Error)
	}
	if !strings.Contains(res.Content, "Tool: tools-plugin.t1") {
		t.Errorf("output missing Tool: header, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "Tool one detailed description.") {
		t.Errorf("output missing description, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "- arg1 (required): first arg") {
		t.Errorf("output missing required-marker for arg1, got: %q", res.Content)
	}
	if !strings.Contains(res.Content, "- arg2: second arg") {
		t.Errorf("output missing arg2, got: %q", res.Content)
	}
	if strings.Contains(res.Content, "arg2 (required)") {
		t.Errorf("non-required arg must NOT carry (required) suffix, got: %q", res.Content)
	}
}

func TestGetToolDetails_ParameterlessActionRendersNoneSentinel(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "tools-plugin.t2"},
	})
	if !strings.Contains(res.Content, "Parameters: (none)") {
		t.Errorf("zero-param action must render 'Parameters: (none)', got: %q", res.Content)
	}
}

func TestGetToolDetails_MissingNameArgReturnsError(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{ID: "c1", Args: map[string]string{}})
	if res.Error == "" || !strings.Contains(res.Error, "name") {
		t.Errorf(`error must mention missing "name", got: %q`, res.Error)
	}
}

func TestGetToolDetails_MalformedFQNReturnsError(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "no-dot-here"},
	})
	if res.Error == "" {
		t.Errorf("malformed FQN must produce an error result")
	}
}

func TestGetToolDetails_UnknownPluginReturnsError(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "missing.action"},
	})
	if !strings.Contains(res.Error, "plugin") {
		t.Errorf("error must mention unknown plugin, got: %q", res.Error)
	}
}

func TestGetToolDetails_UnknownActionReturnsError(t *testing.T) {
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "tools-plugin.unknown"},
	})
	if !strings.Contains(res.Error, "action") {
		t.Errorf("error must mention unknown action, got: %q", res.Error)
	}
}

func TestGetToolDetails_PromotionPersistsTier1Entry(t *testing.T) {
	// Calling get_tool_details for a tool not yet in KnownTools must
	// add a tier="tier1" entry with LRURank=currentTurn so the next
	// turn's tier decision keeps it visible. The previously-empty
	// state path covers the "fresh promotion" branch.
	store := &fakeInjectionStateStore{}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "tools-plugin.t1"},
	})
	if res.Error != "" {
		t.Fatalf("execute failed: %q", res.Error)
	}
	if store.updateCalls != 1 {
		t.Fatalf("UpdateInjectionState calls = %d, want 1", store.updateCalls)
	}
	var found *state.KnownToolEntry
	for i := range store.lastWritten.KnownTools {
		if store.lastWritten.KnownTools[i].ToolName == "tools-plugin.t1" {
			found = &store.lastWritten.KnownTools[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("promoted tool missing from KnownTools, got %+v", store.lastWritten.KnownTools)
	}
	if found.Tier != "tier1" {
		t.Errorf("Tier = %q, want %q", found.Tier, "tier1")
	}
	if found.LRURank < 1 {
		t.Errorf("LRURank = %d, want >= 1", found.LRURank)
	}
}

func TestGetToolDetails_PromotionClearsDemotedFlag(t *testing.T) {
	// An existing Demoted=true entry must self-heal to Demoted=false
	// on explicit promotion — RFC: "any successful invocation clears
	// the demoted flag", and an explicit user-driven promotion is an
	// even stronger signal.
	store := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownTools: []state.KnownToolEntry{
				{ToolName: "tools-plugin.t1", Tier: "tier3", LRURank: 1, Demoted: true},
			}},
		},
	}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin.t1"}})

	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "tools-plugin.t1" {
			if kt.Demoted {
				t.Errorf("Demoted must clear on promotion, got Demoted=true")
			}
			if kt.Tier != "tier1" {
				t.Errorf("Tier must upgrade to tier1, got %q", kt.Tier)
			}
		}
	}
}

func TestGetToolDetails_StoreReadFailureSkipsPromotionButReturnsDescription(t *testing.T) {
	// Read failure must NOT abort the call — the LLM still gets the
	// description in the tool result. Only the promotion side effect
	// is skipped (logged as warn).
	store := &fakeInjectionStateStore{failGetErr: errors.New("simulated db read failure")}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin.t1"}})

	if res.Error != "" {
		t.Errorf("read failure must NOT bubble up to LLM, got error: %q", res.Error)
	}
	if !strings.Contains(res.Content, "Tool: tools-plugin.t1") {
		t.Errorf("description must still be returned, got: %q", res.Content)
	}
	if store.updateCalls != 0 {
		t.Errorf("read failure must skip the write, got %d update calls", store.updateCalls)
	}
}

func TestGetToolDetails_PromotionWriteFailureLogsAndContinues(t *testing.T) {
	// Write failure must NOT bubble up to the LLM — the description
	// is already produced. The handler logs a warning and continues
	// so the round-trip stays useful even when the store is
	// transiently misbehaving. The next turn's tier decision will
	// see the un-promoted state but the LLM still received the
	// schema in the current round-trip.
	store := &fakeInjectionStateStore{failUpdateErr: errors.New("simulated db write failure")}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin.t1"}})
	if res.Error != "" {
		t.Errorf("write failure must NOT surface to LLM, got error: %q", res.Error)
	}
	if !strings.Contains(res.Content, "Tool: tools-plugin.t1") {
		t.Errorf("description must still render, got: %q", res.Content)
	}
	if store.updateCalls != 1 {
		t.Errorf("expected one update attempt, got %d", store.updateCalls)
	}
}

func TestGetToolDetails_PromotionDoesNotRegressLRURank(t *testing.T) {
	// Existing tier="tier1" entry at LRURank >= currentTurn must not
	// have its rank decreased on a re-promotion (guards the
	// "rank only bumps upward" branch in persistToolPromotion).
	store := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownTools: []state.KnownToolEntry{
				{ToolName: "tools-plugin.t1", Tier: "tier1", LRURank: 99},
			}},
		},
	}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin.t1"}})

	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "tools-plugin.t1" && kt.LRURank < 99 {
			t.Errorf("LRURank regressed from 99 to %d on re-promotion", kt.LRURank)
		}
	}
}

func TestGetToolDetails_NoStoreWiredStillReturnsDescription(t *testing.T) {
	// Some tests / minimal deploys wire the meta-tool without an
	// InjectionStateStore. The handler must gracefully degrade —
	// description still served, promotion silently skipped.
	orch := newOrchForMetaTests(t, nil, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin.t1"}})
	if res.Error != "" {
		t.Errorf("no-store path must not error, got: %q", res.Error)
	}
	if !strings.Contains(res.Content, "Tool: tools-plugin.t1") {
		t.Errorf("description must still render, got: %q", res.Content)
	}
}
