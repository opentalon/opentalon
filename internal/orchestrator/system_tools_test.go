package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
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
	if !cap.Actions[0].ReadOnly {
		t.Errorf("get_tool_details action must be ReadOnly=true — it's a pure schema-lookup tool, no user-confirmation gate makes sense")
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

func TestGetToolDetails_ProfileRestrictedPluginReturnsNotFound(t *testing.T) {
	// The inspected plugin runs through the profile gate; a plugin hidden
	// by WhoAmI.Plugins must return the same "plugin not found" shape as
	// a non-existent plugin so the LLM can't distinguish restricted-but-
	// existing from missing. Promotion side-effect must also not fire on
	// denial.
	store := &fakeInjectionStateStore{}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	// Strict mode: profile allowlist excludes the registered "tools-plugin".
	p := &profile.Profile{EntityID: "u1", Plugins: []string{"something-else"}}
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), p), "s1")

	res := exec.Execute(ctx, ToolCall{
		ID:   "c1",
		Args: map[string]string{"name": "tools-plugin.t1"},
	})

	if res.Error == "" {
		t.Fatalf("expected error, got content: %q", res.Content)
	}
	if !strings.Contains(res.Error, "plugin") || !strings.Contains(res.Error, "not found") {
		t.Errorf("error must mimic 'plugin … not found' shape, got: %q", res.Error)
	}
	if strings.Contains(res.Error, "t1") {
		t.Errorf("error must not leak the action name (would distinguish gated-but-existing from missing), got: %q", res.Error)
	}
	if store.updateCalls != 0 {
		t.Errorf("denied call must not write to InjectionState, got %d UpdateInjectionState calls", store.updateCalls)
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
	if found.Tier != state.KnownToolTier1 {
		t.Errorf("Tier = %q, want %q", found.Tier, state.KnownToolTier1)
	}
	if found.LRURank < 1 {
		t.Errorf("LRURank = %d, want >= 1", found.LRURank)
	}
	// The promoted name must also land in the recent-promotions cache
	// so the next preparer pass surfaces it via
	// promoted_via_get_tool_details. promotedToolsThisTurn drains the
	// cache on read, so a single call returns the slice and a second
	// returns empty.
	got := orch.promotedToolsThisTurn(ctx, "s1")
	if len(got) != 1 || got[0] != "tools-plugin.t1" {
		t.Errorf("promotedToolsThisTurn = %v, want [tools-plugin.t1] (the just-promoted tool)", got)
	}
	if again := orch.promotedToolsThisTurn(ctx, "s1"); len(again) != 0 {
		t.Errorf("promotedToolsThisTurn drain failed: second call returned %v, want empty", again)
	}
}

func TestGetToolDetails_PromotionRecentsCache_DedupsRepeatedPromotion(t *testing.T) {
	// Two get_tool_details calls for the SAME tool in one turn must
	// land in the recent-promotions cache only once. Drain returns
	// a single entry, not duplicates.
	store := &fakeInjectionStateStore{}
	orch := newOrchForMetaTests(t, store, true)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")

	for i := 0; i < 2; i++ {
		res := exec.Execute(ctx, ToolCall{
			ID:   "c-repeat",
			Args: map[string]string{"name": "tools-plugin.t1"},
		})
		if res.Error != "" {
			t.Fatalf("execute %d failed: %q", i, res.Error)
		}
	}
	got := orch.promotedToolsThisTurn(ctx, "s1")
	if len(got) != 1 || got[0] != "tools-plugin.t1" {
		t.Errorf("promotedToolsThisTurn = %v, want exactly [tools-plugin.t1] (deduped)", got)
	}
}

func TestPromotedToolsThisTurn_EmptyBeforeAnyPromotion(t *testing.T) {
	// The pre-promotion path: promotedToolsThisTurn on a fresh session
	// returns nil, not a partial state. Pins the contract that an
	// empty / missing entry is a clean "no promotions yet" signal.
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{}, true)
	if got := orch.promotedToolsThisTurn(context.Background(), "fresh"); len(got) != 0 {
		t.Errorf("promotedToolsThisTurn on fresh session = %v, want empty", got)
	}
	if got := orch.promotedToolsThisTurn(context.Background(), ""); len(got) != 0 {
		t.Errorf("promotedToolsThisTurn with empty sessionID = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// withPromotedTool + same-turn cachedTools refresh
// ---------------------------------------------------------------------------

func TestWithPromotedTool_AppendsWhenRelevantToolsSet(t *testing.T) {
	// The happy path: a preparer-provided relevant-tools list gets the
	// promoted tool appended. The resulting ctx is what buildToolDefinitions
	// reads to filter — putting the promoted tool here is what makes the
	// next agent-loop round expose it with its full schema.
	ctx := withRelevantTools(context.Background(), []string{"timly.list-items"})
	got := withPromotedTool(ctx, "timly.show-item")

	tools, ok := relevantToolsFromContext(got)
	if !ok {
		t.Fatal("relevant-tools list missing after withPromotedTool")
	}
	want := []string{"timly.list-items", "timly.show-item"}
	if !reflect.DeepEqual(tools, want) {
		t.Errorf("relevant tools = %v, want %v", tools, want)
	}
}

func TestWithPromotedTool_DedupsExistingEntry(t *testing.T) {
	// A second promotion of the same name in one turn must not double the
	// entry. Native-tools-mode providers de-dupe `tools[]` themselves, but
	// keeping the list canonical avoids the round-trip and any vendor-side
	// surprises.
	ctx := withRelevantTools(context.Background(), []string{"timly.list-items"})
	got := withPromotedTool(ctx, "timly.list-items")

	tools, _ := relevantToolsFromContext(got)
	want := []string{"timly.list-items"}
	if !reflect.DeepEqual(tools, want) {
		t.Errorf("relevant tools = %v, want %v (dedup failed)", tools, want)
	}
}

func TestWithPromotedTool_NoRelevantToolsSet_ReturnsCtxUnchanged(t *testing.T) {
	// When no preparer ran (no relevant-tools list on ctx),
	// buildToolDefinitions already exposes every allowed tool — there's
	// nothing to promote. withPromotedTool must short-circuit and return
	// the original ctx; otherwise we'd inadvertently SET the list to a
	// non-nil singleton and flip the filter from "show all" to "show only
	// this one tool".
	ctx := context.Background()
	got := withPromotedTool(ctx, "timly.show-item")

	if _, ok := relevantToolsFromContext(got); ok {
		t.Error("withPromotedTool must not seed a relevant-tools list when none was set")
	}
}

func TestWithPromotedTool_EmptyName_ReturnsCtxUnchanged(t *testing.T) {
	// Defense in depth: an empty toolName must not silently corrupt the
	// list with a blank entry. Caller responsibility, but we double-gate
	// at the helper since the agent-loop trigger already pulls call.Args
	// dynamically.
	ctx := withRelevantTools(context.Background(), []string{"timly.list-items"})
	got := withPromotedTool(ctx, "")

	tools, _ := relevantToolsFromContext(got)
	if !reflect.DeepEqual(tools, []string{"timly.list-items"}) {
		t.Errorf("empty-name promotion mutated the list: %v", tools)
	}
}

func TestBuildToolDefinitions_AfterWithPromotedTool_IncludesPromotedToolFullSchema(t *testing.T) {
	// Composition: relevant-tools filter narrows to [A], then a
	// _meta.get_tool_details promotion adds [B]. The rebuild driven from
	// the agent loop calls buildToolDefinitions with the post-promotion
	// ctx; the resulting tools-array must contain BOTH A and B (with
	// full schemas). This is the property the native-tools-mode LLM
	// relies on to call the promoted tool in the SAME turn.
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{}, true)

	// The fixture registers tools-plugin with two actions: t1 and t2.
	// Profile filter starts with only t1 visible.
	ctx := withAllowedPlugins(context.Background(), cachedAllowedPlugins{
		m:      map[string]bool{"tools-plugin": true},
		strict: false,
	})
	ctx = withRelevantTools(ctx, []string{"tools-plugin.t1"})

	before := orch.buildToolDefinitions(ctx)
	if len(before) != 1 || before[0].Name != "tools-plugin.t1" {
		t.Fatalf("pre-promotion tools = %+v, want exactly [tools-plugin.t1]", before)
	}

	ctx = withPromotedTool(ctx, "tools-plugin.t2")
	after := orch.buildToolDefinitions(ctx)
	names := make([]string, len(after))
	for i, td := range after {
		names[i] = td.Name
	}
	wantNames := map[string]bool{"tools-plugin.t1": true, "tools-plugin.t2": true}
	if len(names) != 2 {
		t.Fatalf("post-promotion tools = %v, want exactly 2 entries", names)
	}
	for _, n := range names {
		if !wantNames[n] {
			t.Errorf("unexpected tool %q in post-promotion array, wanted only %v", n, wantNames)
		}
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
				{ToolName: "tools-plugin.t1", Tier: state.KnownToolTier3, LRURank: 1, Demoted: true},
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
			if kt.Tier != state.KnownToolTier1 {
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
				{ToolName: "tools-plugin.t1", Tier: state.KnownToolTier1, LRURank: 99},
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
