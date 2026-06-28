package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
)

// fakeInjectionStateStore is a minimal InjectionStateStore
// implementation used by the load_tools sticky-promotion and tool-error
// tracking tests. It round-trips state per session (so a two-turn test
// sees turn 1's writes in turn 2), counts calls for assertion, and lets
// a test inject failure modes via failGetErr / failUpdateErr to exercise
// the orchestrator's "warn-and-continue" fallback paths.
type fakeInjectionStateStore struct {
	getCalls      int
	updateCalls   int
	lastWritten   state.InjectionState
	store         map[string]state.InjectionState
	failGetErr    error
	failUpdateErr error
}

func (f *fakeInjectionStateStore) GetInjectionState(_ context.Context, sessionID string) (state.InjectionState, error) {
	f.getCalls++
	if f.failGetErr != nil {
		return state.InjectionState{}, f.failGetErr
	}
	if f.store == nil {
		return state.InjectionState{}, nil
	}
	return f.store[sessionID], nil
}

func (f *fakeInjectionStateStore) UpdateInjectionState(_ context.Context, sessionID string, st state.InjectionState) error {
	f.updateCalls++
	f.lastWritten = st
	if f.failUpdateErr != nil {
		return f.failUpdateErr
	}
	if f.store == nil {
		f.store = make(map[string]state.InjectionState)
	}
	f.store[sessionID] = st
	return nil
}

func newOrchForMetaTests(t *testing.T, store *fakeInjectionStateStore) *Orchestrator {
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
	opts := OrchestratorOpts{}
	if store != nil {
		opts.InjectionStateStore = store
	}
	return NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, opts)
}

// decodeLoadResult unmarshals the load_tools status JSON for assertions.
func decodeLoadResult(t *testing.T, res ToolResult) loadToolsResult {
	t.Helper()
	if res.Error != "" {
		t.Fatalf("unexpected error result: %q", res.Error)
	}
	var out loadToolsResult
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatalf("load_tools content not valid JSON (%q): %v", res.Content, err)
	}
	return out
}

func TestRegisterLoadTools_RegistersMetaPluginWithAlwaysInclude(t *testing.T) {
	// load_tools is the core discovery mechanism — it must ALWAYS be
	// registered, AlwaysInclude (so it stays in the native tools array),
	// and ReadOnly (no user-confirmation gate).
	orch := newOrchForMetaTests(t, nil)
	cap, ok := orch.registry.GetCapability(metaPluginName)
	if !ok {
		t.Fatalf("meta plugin %q must always be registered", metaPluginName)
	}
	if len(cap.Actions) != 1 || cap.Actions[0].Name != metaLoadTools {
		t.Fatalf("meta plugin must expose exactly %q action, got %+v", metaLoadTools, cap.Actions)
	}
	if !cap.Actions[0].AlwaysInclude {
		t.Errorf("load_tools action must be AlwaysInclude=true so it stays in the native tools array")
	}
	if !cap.Actions[0].ReadOnly {
		t.Errorf("load_tools action must be ReadOnly=true — pure bookkeeping, no user-confirmation gate")
	}
	if len(cap.Actions[0].Parameters) != 1 || cap.Actions[0].Parameters[0].Name != "names" || !cap.Actions[0].Parameters[0].Required {
		t.Errorf("load_tools must require a single 'names' parameter, got %+v", cap.Actions[0].Parameters)
	}
}

func TestLoadTools_ReturnsStatusJSON_NotDescription(t *testing.T) {
	// load_tools must return STATUS ONLY (loaded/ready) — never the
	// description or parameter schema. The schema reaches the LLM via the
	// native tools array on the next request, not from this result.
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{})
	exec, ok := orch.registry.GetExecutor(metaPluginName)
	if !ok {
		t.Fatal("meta plugin executor missing")
	}
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"tools-plugin__t1"}) {
		t.Errorf("loaded = %v, want [tools-plugin__t1]", got.Loaded)
	}
	if !got.Ready {
		t.Errorf("ready = false, want true when a tool loaded")
	}
	if len(got.Failed) != 0 {
		t.Errorf("failed = %v, want empty", got.Failed)
	}
	// The full description / parameters must NOT be in the result.
	for _, leak := range []string{"Tool one detailed description.", "first arg", "Parameters"} {
		if strings.Contains(res.Content, leak) {
			t.Errorf("load_tools result leaked description/schema text %q: %q", leak, res.Content)
		}
	}
}

func TestLoadTools_BatchLoadsMultipleAndReportsFailures(t *testing.T) {
	// Comma-separated names load each in turn; an unresolvable name lands
	// in failed without aborting the rest of the batch.
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{
		ID:   "c1",
		Args: map[string]string{"names": "tools-plugin__t1 , tools-plugin__nope , tools-plugin__t2"},
	})
	got := decodeLoadResult(t, res)
	wantLoaded := []string{"tools-plugin__t1", "tools-plugin__t2"}
	if !reflect.DeepEqual(got.Loaded, wantLoaded) {
		t.Errorf("loaded = %v, want %v", got.Loaded, wantLoaded)
	}
	if !reflect.DeepEqual(got.Failed, []string{"tools-plugin__nope"}) {
		t.Errorf("failed = %v, want [tools-plugin__nope]", got.Failed)
	}
	if !got.Ready {
		t.Errorf("ready = false, want true (at least one tool loaded)")
	}
}

func TestLoadTools_SingleNameFallback(t *testing.T) {
	// Backward tolerance: a single "name" arg works when "names" is absent.
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"name": "tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"tools-plugin__t1"}) {
		t.Errorf("loaded = %v, want [tools-plugin__t1] via name fallback", got.Loaded)
	}
}

func TestLoadTools_AllFailedReportsNotReady(t *testing.T) {
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "missing__a,bad__b"}})
	got := decodeLoadResult(t, res)
	if got.Ready {
		t.Errorf("ready = true, want false when nothing loaded")
	}
	if len(got.Loaded) != 0 {
		t.Errorf("loaded = %v, want empty", got.Loaded)
	}
	if !reflect.DeepEqual(got.Failed, []string{"missing__a", "bad__b"}) {
		t.Errorf("failed = %v, want [missing__a bad__b]", got.Failed)
	}
}

func TestLoadTools_MissingNamesArgReturnsError(t *testing.T) {
	orch := newOrchForMetaTests(t, nil)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	res := exec.Execute(context.Background(), ToolCall{ID: "c1", Args: map[string]string{}})
	if res.Error == "" || !strings.Contains(res.Error, "names") {
		t.Errorf(`error must mention missing "names", got: %q`, res.Error)
	}
}

func TestLoadTools_DeduplicatesRepeatedNamesInOneBatch(t *testing.T) {
	orch := newOrchForMetaTests(t, &fakeInjectionStateStore{})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1,tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"tools-plugin__t1"}) {
		t.Errorf("loaded = %v, want a single deduped [tools-plugin__t1]", got.Loaded)
	}
}

// TestLoadTools_ResolvesBridgedMCPBareName pins the fix for the
// double-prefix tool name an mcp bridge produces: a server "timly" surfaces
// as alias "timly" whose actions keep the "timly__" prefix, so the canonical
// FQN is "timly__timly__delete-item". The execute path forgives the dropped
// prefix; load_tools must too, or an LLM that addresses the tool as
// "timly__delete-item" gets a phantom failure and never loads it.
func TestLoadTools_ResolvesBridgedMCPBareName(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "mcp", Description: "MCP bridge",
		Actions: []Action{
			{Name: "timly__delete-item", Description: "Permanently remove an item.", Parameters: []Parameter{
				{Name: "scope_token", Description: "act on every item in a frozen set", Required: false},
			}},
		},
	}, &fixedResultExecutor{content: "ok"})
	if err := registry.RegisterAlias("timly", "mcp"); err != nil {
		t.Fatalf("RegisterAlias: %v", err)
	}
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		InjectionStateStore: &fakeInjectionStateStore{},
	})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")

	for _, name := range []string{
		"timly__timly__delete-item", // canonical (split on first "__")
		"timly__delete-item",        // LLM dropped the redundant server prefix
		"timly.timly__delete-item",  // legacy dotted form still accepted
		"timly.delete-item",         // legacy dotted, dropped prefix
	} {
		res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": name}})
		got := decodeLoadResult(t, res)
		if len(got.Loaded) != 1 || got.Loaded[0] != "timly__timly__delete-item" {
			t.Errorf("name %q: loaded = %v, want canonical [timly__timly__delete-item]", name, got.Loaded)
		}
	}
}

func TestLoadTools_ProfileRestrictedPluginFails(t *testing.T) {
	// The loaded plugin runs through the profile gate; a plugin hidden by
	// WhoAmI.Plugins must land in failed (not loaded) and must NOT write a
	// promotion, so load_tools can't enumerate or load tools the operator
	// hid.
	store := &fakeInjectionStateStore{}
	orch := newOrchForMetaTests(t, store)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	p := &profile.Profile{EntityID: "u1", Plugins: []string{"something-else"}}
	ctx := actor.WithSessionID(profile.WithProfile(context.Background(), p), "s1")

	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if len(got.Loaded) != 0 {
		t.Errorf("loaded = %v, want empty for profile-restricted plugin", got.Loaded)
	}
	if !reflect.DeepEqual(got.Failed, []string{"tools-plugin__t1"}) {
		t.Errorf("failed = %v, want [tools-plugin__t1]", got.Failed)
	}
	if store.updateCalls != 0 {
		t.Errorf("denied load must not write to InjectionState, got %d UpdateInjectionState calls", store.updateCalls)
	}
}

// TestLoadTools_FilteredByUserOnlyActionFails pins the action-level palette
// gate: cap.Actions may contain an action the per-session palette excludes
// (UserOnly here). Without the gate, load_tools would promote a tool the
// LLM can never invoke. A denied name lands in failed and writes no
// promotion.
func TestLoadTools_FilteredByUserOnlyActionFails(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "tools-plugin", Description: "tools",
		Actions: []Action{
			{Name: "public-tool", Description: "Public tool the LLM may see."},
			{Name: "user-only-tool", Description: "Sensitive; UserOnly excludes from LLM palette.", UserOnly: true},
		},
	}, &fixedResultExecutor{content: "result"})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	store := &fakeInjectionStateStore{}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		InjectionStateStore: store,
	})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")

	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__user-only-tool"}})
	got := decodeLoadResult(t, res)
	if len(got.Loaded) != 0 {
		t.Errorf("loaded = %v, want empty for UserOnly action", got.Loaded)
	}
	if !reflect.DeepEqual(got.Failed, []string{"tools-plugin__user-only-tool"}) {
		t.Errorf("failed = %v, want [tools-plugin__user-only-tool]", got.Failed)
	}
	if store.updateCalls != 0 {
		t.Errorf("denied load must not promote a palette-filtered tool, got %d UpdateInjectionState calls", store.updateCalls)
	}
}

// TestLoadTools_FilteredByPreparerActionFails pins the same gate on the
// preparer/guard axis: those actions live in cap.Actions for invocation but
// the LLM should never load them.
func TestLoadTools_FilteredByPreparerActionFails(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag", Description: "RAG preparer",
		Actions: []Action{
			{Name: "prepare", Description: "Preparer-internal; LLM should not load."},
			{Name: "ask", Description: "LLM-callable knowledge lookup."},
		},
	}, &fixedResultExecutor{content: "result"})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	store := &fakeInjectionStateStore{}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag", Action: "prepare"}},
		InjectionStateStore: store,
	})
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")

	// Preparer action must fail; a non-preparer action on the same plugin must
	// still load — proving the gate is action-level, not plugin-level.
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "rag__prepare,rag__ask"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"rag__ask"}) {
		t.Errorf("loaded = %v, want [rag__ask]", got.Loaded)
	}
	if !reflect.DeepEqual(got.Failed, []string{"rag__prepare"}) {
		t.Errorf("failed = %v, want [rag__prepare]", got.Failed)
	}
}

// TestAllowedToolsSet_ConsistentWithFQNs pins the invariant that both
// consumers of the per-session palette — the JSON-array form for the
// allowed_tools ContextArgProvider (RAG plugins consume it via gRPC) and
// the map form for the load_tools action-level gate — see the same set.
func TestAllowedToolsSet_ConsistentWithFQNs(t *testing.T) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{
			{Name: "analyze_code", Description: "Analyze code"},
			{Name: "internal_panel", Description: "Internal panel", UserOnly: true},
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "jira", Description: "Jira",
		Actions: []Action{{Name: "create_issue", Description: "Create issue"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "rag", Description: "RAG",
		Actions: []Action{{Name: "prepare", Description: "Preparer"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	preparers := []ContentPreparerEntry{{Plugin: "rag", Action: "prepare"}}
	orch := NewWithRules(&fakeLLM{}, &fakeParser{}, registry, memory, sessions, OrchestratorOpts{
		ContentPreparers: preparers,
	})

	ctx := context.Background()
	set := allowedToolsSet(ctx, orch)
	jsonStr := resolveAllowedToolFQNs(ctx, orch)

	if len(set) == 0 {
		t.Fatal("test fixture must produce a non-empty palette")
	}
	if jsonStr == "" {
		t.Fatalf("JSON form must be non-empty when set is non-empty (set=%v)", set)
	}
	var fqns []string
	if err := json.Unmarshal([]byte(jsonStr), &fqns); err != nil {
		t.Fatalf("JSON form not valid: %v", err)
	}
	if len(fqns) != len(set) {
		t.Fatalf("set size %d != JSON array length %d (set=%v, json=%v)", len(set), len(fqns), set, fqns)
	}
	for _, fqn := range fqns {
		if _, ok := set[fqn]; !ok {
			t.Errorf("JSON contains %q which is not in set (drift: %v vs %v)", fqn, fqns, set)
		}
	}
}

func TestLoadTools_PromotionPersistsStickyEntry(t *testing.T) {
	// Loading a tool not yet in KnownTools must add a tier="tier1" entry
	// with LRURank=currentTurn so the next request's tools array keeps it.
	store := &fakeInjectionStateStore{}
	orch := newOrchForMetaTests(t, store)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})
	_ = decodeLoadResult(t, res)
	if store.updateCalls != 1 {
		t.Fatalf("UpdateInjectionState calls = %d, want 1", store.updateCalls)
	}
	var found *state.KnownToolEntry
	for i := range store.lastWritten.KnownTools {
		if store.lastWritten.KnownTools[i].ToolName == "tools-plugin__t1" {
			found = &store.lastWritten.KnownTools[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("loaded tool missing from KnownTools, got %+v", store.lastWritten.KnownTools)
	}
	if found.Tier != state.KnownToolTier1 {
		t.Errorf("Tier = %q, want %q", found.Tier, state.KnownToolTier1)
	}
	if found.LRURank < 1 {
		t.Errorf("LRURank = %d, want >= 1", found.LRURank)
	}
}

func TestLoadTools_PromotionClearsDemotedFlag(t *testing.T) {
	// An existing Demoted=true entry must self-heal to Demoted=false on an
	// explicit load — an explicit load is a strong relevance signal.
	store := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownTools: []state.KnownToolEntry{
				{ToolName: "tools-plugin__t1", Tier: state.KnownToolTier3, LRURank: 1, Demoted: true},
			}},
		},
	}
	orch := newOrchForMetaTests(t, store)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})

	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "tools-plugin__t1" {
			if kt.Demoted {
				t.Errorf("Demoted must clear on load, got Demoted=true")
			}
			if kt.Tier != state.KnownToolTier1 {
				t.Errorf("Tier must upgrade to tier1, got %q", kt.Tier)
			}
		}
	}
}

func TestLoadTools_StoreReadFailureStillReportsLoaded(t *testing.T) {
	// A read failure must NOT fail the call — the name still counts as
	// loaded for this round-trip; only the durable promotion is skipped.
	store := &fakeInjectionStateStore{failGetErr: errors.New("simulated db read failure")}
	orch := newOrchForMetaTests(t, store)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"tools-plugin__t1"}) {
		t.Errorf("loaded = %v, want [tools-plugin__t1] even on read failure", got.Loaded)
	}
	if store.updateCalls != 0 {
		t.Errorf("read failure must skip the write, got %d update calls", store.updateCalls)
	}
}

func TestLoadTools_PromotionDoesNotRegressLRURank(t *testing.T) {
	// Existing tier1 entry at LRURank >= currentTurn must not have its rank
	// decreased on a re-load (guards the upward-only bump in persistToolPromotion).
	store := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownTools: []state.KnownToolEntry{
				{ToolName: "tools-plugin__t1", Tier: state.KnownToolTier1, LRURank: 99},
			}},
		},
	}
	orch := newOrchForMetaTests(t, store)
	exec, _ := orch.registry.GetExecutor(metaPluginName)

	ctx := actor.WithSessionID(context.Background(), "s1")
	_ = exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})

	for _, kt := range store.lastWritten.KnownTools {
		if kt.ToolName == "tools-plugin__t1" && kt.LRURank < 99 {
			t.Errorf("LRURank regressed from 99 to %d on re-load", kt.LRURank)
		}
	}
}

func TestLoadTools_NoStoreWiredStillReportsLoaded(t *testing.T) {
	// Minimal deploys may wire load_tools without an InjectionStateStore.
	// The handler degrades gracefully — loaded reported, promotion skipped.
	orch := newOrchForMetaTests(t, nil)
	exec, _ := orch.registry.GetExecutor(metaPluginName)
	ctx := actor.WithSessionID(context.Background(), "s1")
	res := exec.Execute(ctx, ToolCall{ID: "c1", Args: map[string]string{"names": "tools-plugin__t1"}})
	got := decodeLoadResult(t, res)
	if !reflect.DeepEqual(got.Loaded, []string{"tools-plugin__t1"}) {
		t.Errorf("loaded = %v, want [tools-plugin__t1] with no store wired", got.Loaded)
	}
}
