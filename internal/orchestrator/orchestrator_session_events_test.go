package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/pipeline"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

var errSnapshotStoreFailure = errors.New("simulated snapshot store failure")

// recordingEventSink captures emit.Event values for assertion in tests.
// The mutex makes concurrent Emit calls safe; the snapshot accessor
// returns a copy so callers can iterate without holding the lock.
type recordingEventSink struct {
	mu     sync.Mutex
	events []emit.Event
}

func (s *recordingEventSink) Emit(_ context.Context, e emit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recordingEventSink) snapshot() []emit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]emit.Event, len(s.events))
	copy(out, s.events)
	return out
}

// nativeToolsLLM is a fake LLM that opts in to FeatureTools so the
// orchestrator builds the cachedTools list at the start of each turn.
// Embedding fakeLLM by pointer promotes its Complete method.
type nativeToolsLLM struct{ *fakeLLM }

func (nativeToolsLLM) SupportsFeature(f provider.Feature) bool {
	return f == provider.FeatureTools
}

// recordingSnapshotStore captures UpsertPromptSnapshot calls.
type recordingSnapshotStore struct {
	mu      sync.Mutex
	calls   []promptSnapshotCall
	failErr error // if non-nil, every call returns this error (still recorded)
}

type promptSnapshotCall struct {
	SHA256, Kind, Content string
}

func (s *recordingSnapshotStore) UpsertPromptSnapshot(_ context.Context, sha256, kind, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, promptSnapshotCall{SHA256: sha256, Kind: kind, Content: content})
	return s.failErr
}

func (s *recordingSnapshotStore) snapshot() []promptSnapshotCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]promptSnapshotCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func setupOrchestratorWithSink(llm LLMClient, parser ToolCallParser, sink emit.Sink) (*Orchestrator, string) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:                 "gitlab",
		Description:          "GitLab integration",
		SystemPromptAddition: "Use gitlab to analyze code.",
		Actions: []Action{
			{Name: "analyze_code", Description: "Analyze code for issues"},
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:        "jira",
		Description: "Jira integration",
		Actions:     []Action{{Name: "create_issue", Description: "Create a Jira issue"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("test-session", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{EventSink: sink})
	return orch, "test-session"
}

func setupOrchestratorWithSinkAndStore(llm LLMClient, parser ToolCallParser, sink emit.Sink, store PromptSnapshotUpserter) (*Orchestrator, string) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:                 "gitlab",
		Description:          "GitLab integration",
		SystemPromptAddition: "Use gitlab to analyze code.",
		Actions: []Action{
			{Name: "analyze_code", Description: "Analyze code for issues"},
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:        "jira",
		Description: "Jira integration",
		Actions:     []Action{{Name: "create_issue", Description: "Create a Jira issue"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("test-session", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		PromptSnapshotStore: store,
	})
	return orch, "test-session"
}

func TestOrchestrator_EmitsUserMessage(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"hello back"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "Hello, world!"); err != nil {
		t.Fatal(err)
	}

	var seen int
	var got events.UserMessagePayload
	var gotSessionID string
	for _, e := range sink.snapshot() {
		if e.EventType != events.TypeUserMessage {
			continue
		}
		seen++
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatalf("unmarshal user_message payload: %v", err)
		}
		gotSessionID = e.SessionID
	}
	if seen != 1 {
		t.Fatalf("user_message emitted %d times, want 1", seen)
	}
	if got.Content != "Hello, world!" {
		t.Errorf("Content = %q, want %q", got.Content, "Hello, world!")
	}
	if got.ContentLength != len("Hello, world!") {
		t.Errorf("ContentLength = %d, want %d", got.ContentLength, len("Hello, world!"))
	}
	if got.V != events.UserMessageVersion {
		t.Errorf("Header.V = %d, want %d", got.V, events.UserMessageVersion)
	}
	if gotSessionID != sessID {
		t.Errorf("SessionID = %q, want %q", gotSessionID, sessID)
	}
}

func TestOrchestrator_EmitsTurnStart_HashesSystemPromptAndServerInstructions(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "do the thing"); err != nil {
		t.Fatal(err)
	}

	p := findTurnStart(t, sink.snapshot())
	if p.V != events.TurnStartVersion {
		t.Errorf("Header.V = %d, want %d", p.V, events.TurnStartVersion)
	}
	// system_prompt_sha256 must be a 64-hex digest (sha256 over the actual
	// system prompt built by buildSystemPrompt — exact value depends on
	// internal defaults, so we only sanity-check the shape).
	if len(p.SystemPromptSHA256) != 64 {
		t.Errorf("SystemPromptSHA256 length = %d, want 64; got %q", len(p.SystemPromptSHA256), p.SystemPromptSHA256)
	}
	// Only gitlab has a SystemPromptAddition; jira must not appear.
	if len(p.ServerInstructions) != 1 {
		t.Fatalf("ServerInstructions count = %d, want 1; got %+v", len(p.ServerInstructions), p.ServerInstructions)
	}
	if p.ServerInstructions[0].Name != "gitlab" {
		t.Errorf("ServerInstructions[0].Name = %q, want gitlab", p.ServerInstructions[0].Name)
	}
	wantHash := sha256.Sum256([]byte("Use gitlab to analyze code."))
	if p.ServerInstructions[0].SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Errorf("ServerInstructions[0].SHA256 = %q, want %q", p.ServerInstructions[0].SHA256, hex.EncodeToString(wantHash[:]))
	}
}

func TestOrchestrator_TurnStart_AvailableToolsTextMode_Empty(t *testing.T) {
	// Plain fakeLLM does not implement ReasoningProvider, so
	// supportsNativeTools() returns false → cachedTools is never built →
	// AvailableTools must be empty. Text-mode tool catalogues live inside
	// the system prompt (already covered by system_prompt_sha256), so
	// double-counting them in available_tools would be misleading.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	p := findTurnStart(t, sink.snapshot())
	if len(p.AvailableTools) != 0 {
		t.Errorf("text mode: AvailableTools = %d entries, want 0", len(p.AvailableTools))
	}
}

func TestOrchestrator_TurnStart_AvailableToolsNativeMode_Populated(t *testing.T) {
	sink := &recordingEventSink{}
	llm := nativeToolsLLM{fakeLLM: &fakeLLM{responses: []string{"answer"}}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	p := findTurnStart(t, sink.snapshot())
	if len(p.AvailableTools) == 0 {
		t.Fatal("native mode: AvailableTools is empty")
	}
	got := make(map[string]string, len(p.AvailableTools))
	for _, tr := range p.AvailableTools {
		got[tr.Name] = tr.DescSHA256
	}
	if _, ok := got["gitlab__analyze_code"]; !ok {
		t.Errorf("gitlab__analyze_code missing from AvailableTools; got: %v", got)
	}
	if _, ok := got["jira__create_issue"]; !ok {
		t.Errorf("jira__create_issue missing from AvailableTools; got: %v", got)
	}
	wantDesc := sha256.Sum256([]byte("Analyze code for issues"))
	if got["gitlab__analyze_code"] != hex.EncodeToString(wantDesc[:]) {
		t.Errorf("gitlab__analyze_code desc_sha256 = %s, want %s", got["gitlab__analyze_code"], hex.EncodeToString(wantDesc[:]))
	}
}

func TestOrchestrator_TurnStartFiresOnce_AcrossMultiRoundAgentLoop(t *testing.T) {
	// Three LLM rounds in one Run. turn_start fires once at agent-loop
	// entry, NOT per round; user_message fires once at Run entry. The
	// surrounding model_id/system_prompt/tool catalogue don't change
	// mid-turn, so re-emitting would be noise.
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab__analyze_code",
		"[tool] jira__create_issue",
		"Done!",
	}}
	callNum := 0
	parser := &fakeParser{parseFn: func(string) []ToolCall {
		callNum++
		switch callNum {
		case 1:
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "analyze_code"}}
		case 2:
			return []ToolCall{{ID: "c2", Plugin: "jira", Action: "create_issue"}}
		}
		return nil
	}}
	sink := &recordingEventSink{}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "go"); err != nil {
		t.Fatal(err)
	}

	var turns, users int
	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypeTurnStart:
			turns++
		case events.TypeUserMessage:
			users++
		}
	}
	if turns != 1 {
		t.Errorf("turn_start emitted %d times, want 1", turns)
	}
	if users != 1 {
		t.Errorf("user_message emitted %d times, want 1", users)
	}
}

func TestOrchestrator_EmitsUserMessage_EmptyContent(t *testing.T) {
	// Empty user message (e.g. user sent only file attachments) is captured
	// verbatim. The event still fires — content_length = 0 is the truthful
	// representation of "user posted with no text". File metadata is
	// intentionally out of scope here.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"empty echo"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, ""); err != nil {
		t.Fatal(err)
	}
	var got events.UserMessagePayload
	var seen int
	for _, e := range sink.snapshot() {
		if e.EventType != events.TypeUserMessage {
			continue
		}
		seen++
		if err := json.Unmarshal(e.Payload, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	}
	if seen != 1 {
		t.Fatalf("user_message events = %d, want 1", seen)
	}
	if got.Content != "" {
		t.Errorf("Content = %q, want empty", got.Content)
	}
	if got.ContentLength != 0 {
		t.Errorf("ContentLength = %d, want 0", got.ContentLength)
	}
}

func TestOrchestrator_TurnStart_ProfileModelOverridesModelID(t *testing.T) {
	// When a profile in ctx supplies a model, turn_start's ModelID must
	// reflect that override (not the empty string the bare config would
	// have produced). turn_start captures *intent* — what the orchestrator
	// asked the provider for — not the resolved model the provider may
	// substitute downstream (that lands in llm_response).
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"ok"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	ctx := profile.WithProfile(context.Background(), &profile.Profile{Model: "openai/gpt-4o-mini"})
	if _, err := orch.Run(ctx, sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	p := findTurnStart(t, sink.snapshot())
	// Profile prefix "openai/" must be stripped — matches the orchestrator's
	// own resolution logic (line ~1265).
	if p.ModelID != "gpt-4o-mini" {
		t.Errorf("ModelID = %q, want %q", p.ModelID, "gpt-4o-mini")
	}
}

func TestOrchestrator_TurnStart_ServerInstructionsSortedByName(t *testing.T) {
	// Registry uses Go maps internally; iteration order is randomized.
	// buildTurnStartArgs must sort ServerInstructions deterministically
	// so downstream consumers comparing payloads across turns don't see
	// spurious diffs. Three plugins with additions ensures the sort is
	// actually exercised (with one or two, accidentally-deterministic
	// runs can mask a missing sort).
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "zeta", SystemPromptAddition: "Zeta instructions",
		Actions: []Action{{Name: "a", Description: "a"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "alpha", SystemPromptAddition: "Alpha instructions",
		Actions: []Action{{Name: "a", Description: "a"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "mike", SystemPromptAddition: "Mike instructions",
		Actions: []Action{{Name: "a", Description: "a"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"ok"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	if _, err := orch.Run(context.Background(), "s1", "hi"); err != nil {
		t.Fatal(err)
	}
	p := findTurnStart(t, sink.snapshot())

	names := make([]string, 0, len(p.ServerInstructions))
	for _, si := range p.ServerInstructions {
		names = append(names, si.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("ServerInstructions not sorted by name: %v", names)
	}
	if len(names) != 3 {
		t.Fatalf("ServerInstructions count = %d, want 3", len(names))
	}
}

func TestOrchestrator_SessionNotFound_NoEventEmitted(t *testing.T) {
	// Session lookup happens BEFORE EmitUserMessage; if the session is
	// missing, Run returns with no events emitted. Asserting this ensures
	// the EmitUserMessage placement stays after the lookup — if it ever
	// moves earlier and starts firing on nonexistent sessions, sessionID
	// would carry stale values into analytics.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"unused"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	// Deliberately NOT calling sessions.Create — the session must not exist.

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{EventSink: sink})
	if _, err := orch.Run(context.Background(), "missing-session", "hi"); err == nil {
		t.Fatal("expected session-lookup error, got nil")
	}
	if events := sink.snapshot(); len(events) != 0 {
		t.Errorf("expected zero events on session-not-found, got %d: %+v", len(events), events)
	}
}

func TestOrchestrator_PendingToolCallRejected_EmitsUserMessageButNotTurnStart(t *testing.T) {
	// When the user rejects a previously-queued tool-call confirmation,
	// the orchestrator returns "OK, action cancelled." without entering
	// the agent loop. Per the doc-comment at the EmitTurnStart call site,
	// turn_start must NOT fire in this path (no LLM turn was started),
	// but user_message must (the user did send "no").
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"unused"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	// Seed a pending tool call so Run takes the confirmation branch.
	orch.pendingMu.Lock()
	orch.pendingToolCalls[sessID] = &ToolCall{
		ID:     "call-1",
		Plugin: "gitlab",
		Action: "analyze_code",
		Args:   map[string]string{"repo": "myrepo"},
	}
	orch.pendingMu.Unlock()

	result, err := orch.Run(context.Background(), sessID, "no")
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["action"] != "confirmation_rejected" {
		t.Fatalf("expected confirmation_rejected metadata, got: %+v", result.Metadata)
	}

	var users, turns int
	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypeUserMessage:
			users++
		case events.TypeTurnStart:
			turns++
		}
	}
	if users != 1 {
		t.Errorf("user_message events = %d, want 1", users)
	}
	if turns != 0 {
		t.Errorf("turn_start events = %d, want 0 (rejection path skips agent loop)", turns)
	}
}

func TestOrchestrator_PromptSnapshot_UpsertsSystemPromptAndServerInstructions(t *testing.T) {
	// Every sha256 reference in a turn_start event must resolve to a row
	// in prompt_snapshots — otherwise the Rails review UI sees a dangling
	// hash and the dedup design (one row per unique prompt body, regardless
	// of session count) collapses.
	sink := &recordingEventSink{}
	store := &recordingSnapshotStore{}
	llm := &fakeLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSinkAndStore(llm, parser, sink, store)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}

	p := findTurnStart(t, sink.snapshot())
	if p.SystemPromptSHA256 == "" {
		t.Fatal("turn_start carries no SystemPromptSHA256 — cannot verify upsert")
	}

	// Collect upserts keyed by (sha, kind). Each kind/sha pair must
	// appear exactly once for this turn — the helper does not dedup
	// within a single emission; that's the store's idempotency contract.
	byKey := make(map[string]promptSnapshotCall)
	for _, c := range store.snapshot() {
		byKey[c.SHA256+":"+c.Kind] = c
	}

	// 1. The system_prompt hash must have a matching upsert.
	sysCall, ok := byKey[p.SystemPromptSHA256+":"+events.PromptKindSystemPrompt]
	if !ok {
		t.Fatalf("no system_prompt upsert for sha %s; got %+v", p.SystemPromptSHA256, store.snapshot())
	}
	if sysCall.Content == "" {
		t.Error("system_prompt upsert has empty content")
	}
	// Sanity check: the content must hash back to the emitted sha256.
	gotSum := sha256.Sum256([]byte(sysCall.Content))
	if hex.EncodeToString(gotSum[:]) != p.SystemPromptSHA256 {
		t.Error("system_prompt upsert content does not hash to the emitted sha256")
	}

	// 2. Each server_instructions ref must have a matching upsert with
	// kind=server_instructions and content that hashes back.
	for _, si := range p.ServerInstructions {
		c, ok := byKey[si.SHA256+":"+events.PromptKindServerInstructions]
		if !ok {
			t.Errorf("no server_instructions upsert for %s (sha %s)", si.Name, si.SHA256)
			continue
		}
		gotSum := sha256.Sum256([]byte(c.Content))
		if hex.EncodeToString(gotSum[:]) != si.SHA256 {
			t.Errorf("server_instructions content for %s does not hash to %s", si.Name, si.SHA256)
		}
	}
}

func TestOrchestrator_PromptSnapshot_UpsertsToolDescriptions_NativeMode(t *testing.T) {
	// Native-tools turns carry available_tools[].desc_sha256 in turn_start.
	// Each must resolve via prompt_snapshots (kind=tool_description).
	sink := &recordingEventSink{}
	store := &recordingSnapshotStore{}
	llm := nativeToolsLLM{fakeLLM: &fakeLLM{responses: []string{"done"}}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSinkAndStore(llm, parser, sink, store)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	p := findTurnStart(t, sink.snapshot())
	if len(p.AvailableTools) == 0 {
		t.Fatal("native mode produced no AvailableTools — wiring regression")
	}

	byKey := make(map[string]promptSnapshotCall)
	for _, c := range store.snapshot() {
		byKey[c.SHA256+":"+c.Kind] = c
	}
	for _, tr := range p.AvailableTools {
		c, ok := byKey[tr.DescSHA256+":"+events.PromptKindToolDescription]
		if !ok {
			t.Errorf("no tool_description upsert for %s (sha %s)", tr.Name, tr.DescSHA256)
			continue
		}
		gotSum := sha256.Sum256([]byte(c.Content))
		if hex.EncodeToString(gotSum[:]) != tr.DescSHA256 {
			t.Errorf("tool_description content for %s does not hash to %s", tr.Name, tr.DescSHA256)
		}
	}
}

func TestOrchestrator_PromptSnapshot_NilStore_NoPanic(t *testing.T) {
	// PromptSnapshotStore is optional. With no store configured, Run
	// must succeed without upsert side effects — the event still ships;
	// the consumer just won't be able to resolve the hashes. This path
	// covers the always-on capture case where state DB isn't configured.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	// And turn_start still emits — the absent store does not gate the event.
	_ = findTurnStart(t, sink.snapshot())
}

func TestOrchestrator_PromptSnapshot_UpsertFailure_TurnContinues(t *testing.T) {
	// A transient snapshot-store error must NOT kill the user turn. The
	// alternative — failing every Run because analytics can't write —
	// is worse than a dangling sha reference until the next upsert
	// succeeds.
	sink := &recordingEventSink{}
	store := &recordingSnapshotStore{failErr: errSnapshotStoreFailure}
	llm := &fakeLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSinkAndStore(llm, parser, sink, store)
	result, err := orch.Run(context.Background(), sessID, "hi")
	if err != nil {
		t.Fatalf("Run returned %v despite snapshot upsert failures; should swallow", err)
	}
	if result == nil || result.Response != "answer" {
		t.Errorf("expected unchanged result, got %+v", result)
	}
	// Even with failures, turn_start still emits.
	_ = findTurnStart(t, sink.snapshot())
	// And every upsert was attempted (failure is recorded).
	if len(store.snapshot()) == 0 {
		t.Error("expected upsert attempts despite failures, got none")
	}
}

func TestOrchestrator_NoEventSinkConfigured_DefaultsToNoOp(t *testing.T) {
	// setupOrchestrator uses New() which doesn't set EventSink — the
	// constructor must fall back to emit.NoOpSink so Run doesn't panic
	// on nil dereference. Run with no callers to the sink also exercises
	// the happy path with the default.
	llm := &fakeLLM{responses: []string{"hi"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestrator(llm, parser)
	if _, err := orch.Run(context.Background(), sessID, "ping"); err != nil {
		t.Fatal(err)
	}
}

// findTurnStart returns the first turn_start payload found in the slice.
// Fails the test if none is present.
func findTurnStart(t *testing.T, evs []emit.Event) events.TurnStartPayload {
	t.Helper()
	for _, e := range evs {
		if e.EventType != events.TypeTurnStart {
			continue
		}
		var p events.TurnStartPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal turn_start payload: %v", err)
		}
		return p
	}
	t.Fatal("turn_start event not found")
	return events.TurnStartPayload{}
}

// -----------------------------------------------------------------------------
// Tool dispatcher instrumentation tests
// -----------------------------------------------------------------------------

// nativeToolCallingLLM returns native ToolCalls on its first Complete call,
// then a plain text response on subsequent rounds so the agent loop
// terminates. Implements FeatureTools so the orchestrator picks the native
// path. Embedding by value so SupportsFeature is promoted on the value.
type nativeToolCallingLLM struct {
	toolCalls []provider.ToolCall
	textAfter string
	calls     int
}

func (l *nativeToolCallingLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.calls++
	if l.calls == 1 {
		return &provider.CompletionResponse{ToolCalls: l.toolCalls}, nil
	}
	return &provider.CompletionResponse{Content: l.textAfter}, nil
}

func (l *nativeToolCallingLLM) SupportsFeature(f provider.Feature) bool {
	return f == provider.FeatureTools
}

// errorExecutor returns a tool error result, exercising the
// tool_call_result status=error branch.
type errorExecutor struct{ msg string }

func (e *errorExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{CallID: call.ID, Error: e.msg}
}

// structuredExecutor returns a ToolResult with both Content and
// StructuredContent populated — exercises the audit-log capture of the
// structured half that nativeToolContent appends to the LLM-bound
// message. The optional errMsg lets one stub cover the defensive
// "error result that happens to carry structured data" case.
type structuredExecutor struct {
	content    string
	structured string
	errMsg     string
}

func (s *structuredExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{
		CallID:            call.ID,
		Content:           s.content,
		StructuredContent: s.structured,
		Error:             s.errMsg,
	}
}

// findToolCallEvents collects all events of the given type into typed payloads.
func findToolCallExtractedPayloads(t *testing.T, evs []emit.Event) []events.ToolCallExtractedPayload {
	t.Helper()
	var out []events.ToolCallExtractedPayload
	for _, e := range evs {
		if e.EventType != events.TypeToolCallExtracted {
			continue
		}
		var p events.ToolCallExtractedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal tool_call_extracted: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func findToolCallResultPayloads(t *testing.T, evs []emit.Event) []events.ToolCallResultPayload {
	t.Helper()
	var out []events.ToolCallResultPayload
	for _, e := range evs {
		if e.EventType != events.TypeToolCallResult {
			continue
		}
		var p events.ToolCallResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal tool_call_result: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestOrchestrator_ExecuteCall_EmitsExtractedAndResult_NativeMode(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:   "call-1",
			Name: "gitlab__analyze_code",
			// gitlab__analyze_code declares no Parameters in the test fixture,
			// so any args would trip rejectUnknownArgs. Leave empty for the
			// happy-path assertion.
			Arguments: map[string]string{},
		}},
		textAfter: "analysis complete",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "analyze main.go"); err != nil {
		t.Fatal(err)
	}

	extracted := findToolCallExtractedPayloads(t, sink.snapshot())
	if len(extracted) != 1 {
		t.Fatalf("tool_call_extracted count = %d, want 1", len(extracted))
	}
	got := extracted[0]
	if got.V != events.ToolCallExtractedVersion {
		t.Errorf("Header.V = %d, want %d", got.V, events.ToolCallExtractedVersion)
	}
	if got.Mode != "native" {
		t.Errorf("Mode = %q, want native", got.Mode)
	}
	if got.CallID != "call-1" {
		t.Errorf("CallID = %q, want call-1", got.CallID)
	}
	if got.Plugin != "gitlab" || got.Action != "analyze_code" {
		t.Errorf("Plugin/Action = %q/%q, want gitlab__analyze_code", got.Plugin, got.Action)
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].CallID != "call-1" {
		t.Errorf("result.CallID = %q, want call-1", results[0].CallID)
	}
	if results[0].Status != "ok" {
		t.Errorf("result.Status = %q, want ok", results[0].Status)
	}
	if results[0].ResponseExcerpt == "" {
		t.Errorf("result.ResponseExcerpt is empty; want echoed body")
	}
}

func TestOrchestrator_ExecuteCall_EmitsExtractedAndResult_TextMode(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"call the tool", "done"}}
	parseCount := 0
	parser := &fakeParser{parseFn: func(string) []ToolCall {
		parseCount++
		if parseCount == 1 {
			return []ToolCall{{
				ID:     "call-text-1",
				Plugin: "gitlab",
				Action: "analyze_code",
				// gitlab__analyze_code declares no Parameters; args would
				// trip rejectUnknownArgs. Happy-path assertion uses none.
				Args: map[string]string{},
			}}
		}
		return nil
	}}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}

	extracted := findToolCallExtractedPayloads(t, sink.snapshot())
	if len(extracted) != 1 {
		t.Fatalf("tool_call_extracted count = %d, want 1", len(extracted))
	}
	if extracted[0].Mode != "text" {
		t.Errorf("Mode = %q, want text (plain fakeLLM has no FeatureTools)", extracted[0].Mode)
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "ok" {
		t.Errorf("result.Status = %q, want ok", results[0].Status)
	}
}

func TestOrchestrator_ExecuteCall_ResultStatus_ErrorOnDispatchError(t *testing.T) {
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "broken",
		Actions: []Action{{Name: "do"}},
	}, &errorExecutor{msg: "exec blew up"})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	call := ToolCall{ID: "c1", Plugin: "broken", Action: "do", FromLLM: true}
	res := orch.executeCall(context.Background(), call)
	if res.Error == "" {
		t.Fatal("expected error result")
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "error" {
		t.Errorf("Status = %q, want error", results[0].Status)
	}
	if results[0].ResponseExcerpt != "exec blew up" {
		t.Errorf("ResponseExcerpt = %q, want %q", results[0].ResponseExcerpt, "exec blew up")
	}
}

// TestOrchestrator_ExecuteCall_PassesStructuredContent pins the
// dispatch-site contract: when a tool returns a ToolResult with both
// Content and StructuredContent populated, the emitted
// tool_call_result event captures BOTH halves as independent excerpt
// fields. A future copy-paste regression that drops Structured from
// the ToolCallResultArgs payload fails this test.
func TestOrchestrator_ExecuteCall_PassesStructuredContent(t *testing.T) {
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "structured",
		Actions: []Action{{Name: "fetch"}},
	}, &structuredExecutor{
		content:    "human-readable summary",
		structured: `{"items":[{"id":42}]}`,
	})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	call := ToolCall{ID: "c1", Plugin: "structured", Action: "fetch", FromLLM: true}
	res := orch.executeCall(context.Background(), call)
	if res.Error != "" {
		t.Fatalf("unexpected error result: %q", res.Error)
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "ok" {
		t.Errorf("Status = %q, want ok", results[0].Status)
	}
	if results[0].ResponseExcerpt != "human-readable summary" {
		t.Errorf("ResponseExcerpt = %q, want %q", results[0].ResponseExcerpt, "human-readable summary")
	}
	if results[0].StructuredExcerpt != `{"items":[{"id":42}]}` {
		t.Errorf("StructuredExcerpt = %q, want raw JSON passthrough", results[0].StructuredExcerpt)
	}
	if results[0].ResponseTruncated || results[0].StructuredTruncated {
		t.Errorf("unexpected truncation: resp=%v struct=%v", results[0].ResponseTruncated, results[0].StructuredTruncated)
	}
}

// TestOrchestrator_ExecuteCall_ErrorClearsStructured pins the
// error-path invariant: when a tool returns an error, the
// tool_call_result event records the error message in
// ResponseExcerpt and DOES NOT carry over a structured payload —
// even if the ToolResult happens to have one populated. A
// status:"error" event with structured_excerpt set would mislead
// operators reading the audit log.
func TestOrchestrator_ExecuteCall_ErrorClearsStructured(t *testing.T) {
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "structured",
		Actions: []Action{{Name: "fetch"}},
	}, &structuredExecutor{
		content:    "should be ignored",
		structured: `{"partial":true}`,
		errMsg:     "downstream blew up",
	})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	call := ToolCall{ID: "c1", Plugin: "structured", Action: "fetch", FromLLM: true}
	if res := orch.executeCall(context.Background(), call); res.Error == "" {
		t.Fatal("expected error result")
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "error" {
		t.Errorf("Status = %q, want error", results[0].Status)
	}
	if results[0].ResponseExcerpt != "downstream blew up" {
		t.Errorf("ResponseExcerpt = %q, want error message", results[0].ResponseExcerpt)
	}
	if results[0].StructuredExcerpt != "" {
		t.Errorf("StructuredExcerpt = %q on error path, want empty (partial structured data must not leak under status:error)", results[0].StructuredExcerpt)
	}
}

func TestOrchestrator_ExecuteCall_EmitsNotFound_UnknownPlugin(t *testing.T) {
	sink := &recordingEventSink{}
	orch, _ := setupOrchestratorWithSink(&fakeLLM{},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }}, sink)

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "no-such-plugin", Action: "anything", FromLLM: true,
	})
	if res.Error == "" {
		t.Fatal("expected error result")
	}

	evs := sink.snapshot()
	var notFound []emit.Event
	var extracted int
	for _, e := range evs {
		switch e.EventType {
		case events.TypeToolCallNotFound:
			notFound = append(notFound, e)
		case events.TypeToolCallExtracted:
			extracted++
		}
	}
	if extracted != 1 {
		t.Errorf("extracted count = %d, want 1", extracted)
	}
	if len(notFound) != 1 {
		t.Fatalf("not_found count = %d, want 1", len(notFound))
	}
	var p events.ToolCallNotFoundPayload
	if err := json.Unmarshal(notFound[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.RequestedName != "no-such-plugin__anything" {
		t.Errorf("RequestedName = %q, want no-such-plugin__anything", p.RequestedName)
	}
	// A tool_call_result MUST NOT fire on the not-found path (no dispatch happened).
	if len(findToolCallResultPayloads(t, evs)) != 0 {
		t.Errorf("tool_call_result emitted on not_found path; should not be")
	}
}

func TestOrchestrator_ExecuteCall_EmitsNotFound_UnknownAction(t *testing.T) {
	sink := &recordingEventSink{}
	orch, _ := setupOrchestratorWithSink(&fakeLLM{},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }}, sink)

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "gitlab", Action: "totally-bogus-action", FromLLM: true,
	})
	if res.Error == "" {
		t.Fatal("expected error result")
	}

	var notFound int
	for _, e := range sink.snapshot() {
		if e.EventType == events.TypeToolCallNotFound {
			notFound++
		}
	}
	if notFound != 1 {
		t.Errorf("not_found count = %d, want 1", notFound)
	}
}

func TestOrchestrator_ExecuteCall_EmitsArgsInvalid_OnRejectUnknownArgs(t *testing.T) {
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "strict",
		Actions: []Action{{
			Name:       "do",
			Parameters: []Parameter{{Name: "expected", Required: false}},
		}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	res := orch.executeCall(context.Background(), ToolCall{
		ID:      "c1",
		Plugin:  "strict",
		Action:  "do",
		Args:    map[string]string{"stray": "value"},
		FromLLM: true,
	})
	if res.Error == "" {
		t.Fatal("expected validation error")
	}

	evs := sink.snapshot()
	var invalid []emit.Event
	for _, e := range evs {
		if e.EventType == events.TypeToolCallArgsInvalid {
			invalid = append(invalid, e)
		}
	}
	if len(invalid) != 1 {
		t.Fatalf("args_invalid count = %d, want 1", len(invalid))
	}
	var p events.ToolCallArgsInvalidPayload
	if err := json.Unmarshal(invalid[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.CallID != "c1" || p.Plugin != "strict" || p.Action != "do" {
		t.Errorf("payload identity drift: %+v", p)
	}
	if p.ValidationError == "" {
		t.Errorf("ValidationError empty; want non-empty error text")
	}
	// No tool_call_result on the args_invalid path (validation rejects before dispatch).
	if len(findToolCallResultPayloads(t, evs)) != 0 {
		t.Errorf("tool_call_result emitted on args_invalid path; should not be")
	}
}

func TestOrchestrator_ExecuteCall_NoEmission_WhenFromLLMFalse(t *testing.T) {
	// Internal calls (preparers, guards, pipelines) carry FromLLM=false.
	// They are host-orchestrated, not part of the LLM reasoning trace, and
	// must not pollute session_events analytics.
	sink := &recordingEventSink{}
	orch, _ := setupOrchestratorWithSink(&fakeLLM{},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }}, sink)

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "internal-1", Plugin: "gitlab", Action: "analyze_code", FromLLM: false,
	})
	if res.Error != "" {
		t.Fatalf("internal call failed unexpectedly: %s", res.Error)
	}

	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypeToolCallExtracted, events.TypeToolCallResult,
			events.TypeToolCallNotFound, events.TypeToolCallArgsInvalid:
			t.Errorf("internal call emitted %q; should not emit any tool_call_* event", e.EventType)
		}
	}
}

func TestOrchestrator_ExecuteCall_RawCapture_ExtractedHasOriginalActionBeforeNormalization(t *testing.T) {
	// LLMs frequently emit underscore-style action names (list_persons)
	// when the registry uses hyphen-style (list-persons). The orchestrator
	// normalizes silently; raw-capture rule says the extracted event MUST
	// preserve the original (un-normalized) action the LLM emitted.
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "persons",
		Actions: []Action{{Name: "list-persons"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "persons", Action: "list_persons", FromLLM: true,
	})
	if res.Error != "" {
		t.Fatalf("normalization should have succeeded: %s", res.Error)
	}

	extracted := findToolCallExtractedPayloads(t, sink.snapshot())
	if len(extracted) != 1 {
		t.Fatalf("extracted count = %d", len(extracted))
	}
	if extracted[0].Action != "list_persons" {
		t.Errorf("extracted.Action = %q, want raw %q (pre-normalization)",
			extracted[0].Action, "list_persons")
	}
}

func TestOrchestrator_ExecuteCall_LatencyMSPopulatedAndNonNegative(t *testing.T) {
	sink := &recordingEventSink{}
	orch, _ := setupOrchestratorWithSink(&fakeLLM{},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }}, sink)

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "gitlab", Action: "analyze_code", FromLLM: true,
	})
	if res.Error != "" {
		t.Fatal(res.Error)
	}
	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("result count = %d", len(results))
	}
	if results[0].LatencyMS < 0 {
		t.Errorf("LatencyMS = %d, want >= 0", results[0].LatencyMS)
	}
}

func TestOrchestrator_ExecuteCall_EmitsResultError_OnUserOnlyRefusal(t *testing.T) {
	// Policy refusal: UserOnly actions cannot be called by the LLM. The
	// call was extracted and identified before the gate fired, so the
	// orchestrator emits tool_call_result with status="error" (NOT
	// tool_call_not_found — the action was found, just refused).
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "admin",
		Actions: []Action{{Name: "destroy", UserOnly: true}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	res := orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "admin", Action: "destroy", FromLLM: true,
	})
	if res.Error == "" {
		t.Fatal("expected UserOnly refusal")
	}

	evs := sink.snapshot()
	results := findToolCallResultPayloads(t, evs)
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "error" {
		t.Errorf("Status = %q, want error", results[0].Status)
	}
	if results[0].ResponseExcerpt == "" {
		t.Errorf("ResponseExcerpt empty; want refusal message")
	}
	// Must NOT emit not_found / args_invalid — the action was found and
	// args are not the issue; this is a policy refusal.
	for _, e := range evs {
		if e.EventType == events.TypeToolCallNotFound || e.EventType == events.TypeToolCallArgsInvalid {
			t.Errorf("unexpected %q on UserOnly refusal path", e.EventType)
		}
	}
}

func TestOrchestrator_ExecuteCall_EmitsResultError_OnRestrictedPluginRefusal(t *testing.T) {
	// Policy refusal via strict allowlist (profile.Plugins). The LLM's
	// extracted plugin name resolves, but the profile excludes it →
	// pluginAllowed returns false → tool_call_result with status="error".
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "restricted",
		Actions: []Action{{Name: "do"}},
	}, &echoExecutor{})

	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{EventSink: sink})

	// Strict allowlist that excludes the "restricted" plugin.
	ctx := profile.WithProfile(context.Background(), &profile.Profile{
		Plugins: []string{"other-plugin"},
	})
	res := orch.executeCall(ctx, ToolCall{
		ID: "c1", Plugin: "restricted", Action: "do", FromLLM: true,
	})
	if res.Error == "" {
		t.Fatal("expected restricted-plugin refusal")
	}

	results := findToolCallResultPayloads(t, sink.snapshot())
	if len(results) != 1 {
		t.Fatalf("tool_call_result count = %d, want 1", len(results))
	}
	if results[0].Status != "error" {
		t.Errorf("Status = %q, want error", results[0].Status)
	}
}

// -----------------------------------------------------------------------------
// Text-parser instrumentation tests
// -----------------------------------------------------------------------------

// findParseFailedPayloads collects all tool_call_parse_failed payloads.
func findParseFailedPayloads(t *testing.T, evs []emit.Event) []events.ToolCallParseFailedPayload {
	t.Helper()
	var out []events.ToolCallParseFailedPayload
	for _, e := range evs {
		if e.EventType != events.TypeToolCallParseFailed {
			continue
		}
		var p events.ToolCallParseFailedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal tool_call_parse_failed: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestOrchestrator_ParseFailed_EmittedOnMalformedToolCallBody(t *testing.T) {
	// LLM emits a [tool_call] block whose body is unparseable. Default
	// parser falls through every format (json.Unmarshal fails, body
	// doesn't start with '{' so the bare-JSON placeholder doesn't fire,
	// inline parse fails) and at end-of-function returns the sawBlock
	// empty-plugin placeholder. tool_call_parse_failed must fire once.
	sink := &recordingEventSink{}
	// Use DefaultParser so the real parser logic decides — fakeParser
	// would bypass the marker-vs-empty-plugin signal that drives the
	// parse_failed emission.
	llm := &fakeLLM{responses: []string{
		"[tool_call] this is not a valid tool call body [/tool_call]",
		"fallback final answer",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)

	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}

	parseFailed := findParseFailedPayloads(t, sink.snapshot())
	if len(parseFailed) != 1 {
		t.Fatalf("tool_call_parse_failed count = %d, want 1", len(parseFailed))
	}
	got := parseFailed[0]
	if got.V != events.ToolCallParseFailedVersion {
		t.Errorf("Header.V = %d, want %d", got.V, events.ToolCallParseFailedVersion)
	}
	if got.ParserUsed != "default" {
		t.Errorf("ParserUsed = %q, want default", got.ParserUsed)
	}
	if got.ParseError == "" {
		t.Errorf("ParseError empty; want non-empty error text")
	}
	if got.RawSnippet == "" {
		t.Errorf("RawSnippet empty; want full response content")
	}
}

func TestOrchestrator_ParseFailed_EmittedOnBareJSONWithoutToolKey(t *testing.T) {
	// LLM wraps a JSON object in [tool_call] markers but forgets the
	// "tool" key. Parser's Format-A json.Unmarshal succeeds but
	// block.Tool=="", so it falls through to Format B/C. Body starts
	// with '{' → bare-JSON placeholder fires inside the loop (Plugin="").
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		`[tool_call]{"args": {"name": "test"}}[/tool_call]`,
		"final",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}
	parseFailed := findParseFailedPayloads(t, sink.snapshot())
	if len(parseFailed) != 1 {
		t.Fatalf("tool_call_parse_failed count = %d, want 1", len(parseFailed))
	}
}

func TestOrchestrator_ParseFailed_NotEmittedOnPlainTextResponse(t *testing.T) {
	// Pure text response with no tool-call markers — parser returns nil,
	// containsToolCallMarker returns false. No event must fire (this is
	// the most common path and would drown analytics).
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"Just a plain answer with no tool call attempts."}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 0 {
		t.Errorf("tool_call_parse_failed fired on plain-text response (%d events); should not", len(pf))
	}
}

func TestOrchestrator_ParseFailed_NotEmittedOnSuccessfulParse(t *testing.T) {
	// LLM emits a valid Format A tool call. Parser returns one call with
	// a real Plugin name. emitParseFailedIfApplicable sees no empty-plugin
	// entry → no event.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		`[tool_call]{"tool": "gitlab__analyze_code", "args": {}}[/tool_call]`,
		"summary after tool call",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 0 {
		t.Errorf("tool_call_parse_failed fired on successful parse (%d events); should not", len(pf))
	}
	// Sanity: tool_call_extracted still fires for the successful path.
	if len(findToolCallExtractedPayloads(t, sink.snapshot())) != 1 {
		t.Errorf("expected one tool_call_extracted on successful parse")
	}
}

func TestOrchestrator_ParseFailed_NotEmittedOnNarratedPlaceholder(t *testing.T) {
	// LLM narrates a tool call in plain text without using markers
	// ("We'll fetch the inventory."). Parser detects via narratedIntentRe
	// and returns narratedToolCallPlaceholder. containsToolCallMarker
	// returns false (no [tool_call] tag); even if it did,
	// IsNarratedPlaceholder check would skip emission.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"I'll fetch the inventory list for you.",
		"final after narration retry",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "show inventory"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 0 {
		t.Errorf("tool_call_parse_failed fired on narrated placeholder (%d events); should not", len(pf))
	}
}

func TestOrchestrator_ParseFailed_EmittedOnAnthropicXMLLeak(t *testing.T) {
	// Claude sometimes leaks <function_calls> XML when our prompt asks
	// for [tool_call] format. Parser's parseXMLFunctionCalls path can
	// extract calls — but if the XML is malformed (missing name attr,
	// invalid tool name), no calls come out and the sawBlock fallback
	// fires because containsInternalBlock detects <function_calls>.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		`<function_calls><invoke>no name attribute</invoke></function_calls>`,
		"final",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 1 {
		t.Errorf("tool_call_parse_failed count = %d, want 1 on malformed XML", len(pf))
	}
}

func TestOrchestrator_ParseFailed_EmittedOnUnclosedToolCallMarker(t *testing.T) {
	// [tool_call] opens but no closing [/tool_call] tag. The default
	// parser's "no closing tag" branch (parser.go line 61-64) takes the
	// rest-of-string as body, attempts Format A then Format B/C, and
	// produces an empty-plugin placeholder via the sawBlock fallback.
	// The parse_failed event must catch this — without a closing marker,
	// the response is incoherent and analytics needs to see the failure.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		"[tool_call] this has no closing marker and is malformed",
		"final",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 1 {
		t.Errorf("tool_call_parse_failed count = %d, want 1 on unclosed marker", len(pf))
	}
}

func TestOrchestrator_ParseFailed_NoEventSink_DoesNotPanic(t *testing.T) {
	// NoOpSink default path coverage for the parse_failed emit site.
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	llm := &fakeLLM{responses: []string{
		"[tool_call] garbage [/tool_call]",
		"final",
	}}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{})
	if _, err := orch.Run(context.Background(), "sess", "ping"); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestrator_ExecuteCall_NoEventSink_DoesNotPanic(t *testing.T) {
	// NoOpSink default-path coverage for the dispatcher emit site.
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Actions: []Action{{Name: "analyze_code"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(&fakeLLM{}, &fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, memory, sessions, OrchestratorOpts{}) // no EventSink

	_ = orch.executeCall(context.Background(), ToolCall{
		ID: "c1", Plugin: "gitlab", Action: "analyze_code", FromLLM: true,
	})
	_ = orch.executeCall(context.Background(), ToolCall{
		ID: "c2", Plugin: "missing", Action: "anything", FromLLM: true,
	})
}

// -----------------------------------------------------------------------------
// Planner + summarization instrumentation tests
// -----------------------------------------------------------------------------

func findPayloadsByType(t *testing.T, evs []emit.Event, eventType string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, e := range evs {
		if e.EventType == eventType {
			out = append(out, e.Payload)
		}
	}
	return out
}

// setupOrchestratorWithPlanner builds an orchestrator with PipelineEnabled
// so the real *pipeline.Planner is constructed against the fake LLM. The
// fake LLM must produce a parseable JSON plan as its first response.
func setupOrchestratorWithPlanner(llm LLMClient, parser ToolCallParser, sink emit.Sink) (*Orchestrator, string) {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "gitlab",
		Actions: []Action{{Name: "analyze_code"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:       sink,
		PipelineEnabled: true,
	})
	return orch, "sess"
}

func TestOrchestrator_Planner_EmitsInvokedRequestResponseOnDirectPlan(t *testing.T) {
	sink := &recordingEventSink{}
	// Round 1: planner LLM returns "direct" plan.
	// Round 2: agent loop LLM returns final answer.
	llm := &fakeLLM{responses: []string{
		`{"type":"direct"}`,
		"final answer",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithPlanner(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "hello"); err != nil {
		t.Fatal(err)
	}

	evs := sink.snapshot()
	if len(findPayloadsByType(t, evs, events.TypePlannerInvoked)) != 1 {
		t.Errorf("planner_invoked count != 1")
	}
	if len(findPayloadsByType(t, evs, events.TypePlannerRequest)) != 1 {
		t.Errorf("planner_request count != 1")
	}
	if len(findPayloadsByType(t, evs, events.TypePlannerResponse)) != 1 {
		t.Errorf("planner_response count != 1")
	}
	// Direct plan has no steps, no planner_step events.
	if n := len(findPayloadsByType(t, evs, events.TypePlannerStep)); n != 0 {
		t.Errorf("planner_step count = %d, want 0 on direct plan", n)
	}

	// planner_invoked payload: reason set.
	var inv events.PlannerInvokedPayload
	_ = json.Unmarshal(findPayloadsByType(t, evs, events.TypePlannerInvoked)[0], &inv)
	if inv.Reason == "" {
		t.Errorf("planner_invoked.Reason empty")
	}

	// planner_response payload: synthetic summary, latency >= 0.
	var resp events.PlannerResponsePayload
	_ = json.Unmarshal(findPayloadsByType(t, evs, events.TypePlannerResponse)[0], &resp)
	if !strings.Contains(resp.RawContentExcerpt, "type=direct") {
		t.Errorf("planner_response.RawContentExcerpt = %q, want contains type=direct", resp.RawContentExcerpt)
	}
	if resp.LatencyMS < 0 {
		t.Errorf("planner_response.LatencyMS = %d, want >= 0", resp.LatencyMS)
	}
}

func TestOrchestrator_Planner_EmitsStepEventsOnPipelinePlan(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		// Pipeline plan with 2 steps. Single-step pipelines are special-cased,
		// so use 2 to trigger the full pipeline path (and reach the planner
		// branch we care about — single-step paths return early).
		`{"type":"pipeline","steps":[
			{"id":"s1","name":"step one","plugin":"gitlab","action":"analyze_code","args":{},"depends_on":[]},
			{"id":"s2","name":"step two","plugin":"gitlab","action":"analyze_code","args":{},"depends_on":["s1"]}
		]}`,
		// In case pipeline confirmation needs an LLM round.
		"continue",
		"final",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithPlanner(llm, parser, sink)

	// Plan-only path can pause for confirmation. We only need the planner
	// events; tolerate any final state.
	_, _ = orch.Run(context.Background(), sessID, "do two things")

	steps := findPayloadsByType(t, sink.snapshot(), events.TypePlannerStep)
	if len(steps) != 2 {
		t.Fatalf("planner_step count = %d, want 2", len(steps))
	}
	for i, raw := range steps {
		var p events.PlannerStepPayload
		_ = json.Unmarshal(raw, &p)
		if p.StepIndex != i {
			t.Errorf("step[%d].StepIndex = %d, want %d", i, p.StepIndex, i)
		}
		if p.StepKind != "tool" {
			t.Errorf("step[%d].StepKind = %q, want tool", i, p.StepKind)
		}
		if p.Note != "gitlab__analyze_code" {
			t.Errorf("step[%d].Note = %q, want gitlab__analyze_code", i, p.Note)
		}
	}
}

func TestOrchestrator_Planner_EmitsResponseOnPlannerError(t *testing.T) {
	// Planner LLM returns unparseable content; parsePlanResponse falls back
	// to {Type: "direct"} but returns no error. To trigger an actual error
	// we'd need the LLM Complete itself to fail — use an LLM that errors on
	// the first call. The agent loop then runs against a fresh LLM call.
	sink := &recordingEventSink{}
	llm := &erroringPlannerLLM{plannerErr: errPlannerCallFailed}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithPlanner(llm, parser, sink)

	_, _ = orch.Run(context.Background(), sessID, "hi")

	evs := sink.snapshot()
	// planner_invoked + planner_request + planner_response all fire even on error.
	if len(findPayloadsByType(t, evs, events.TypePlannerInvoked)) != 1 {
		t.Errorf("planner_invoked count != 1 on error path")
	}
	if len(findPayloadsByType(t, evs, events.TypePlannerResponse)) != 1 {
		t.Errorf("planner_response count != 1 on error path")
	}
	var resp events.PlannerResponsePayload
	_ = json.Unmarshal(findPayloadsByType(t, evs, events.TypePlannerResponse)[0], &resp)
	if !strings.Contains(resp.RawContentExcerpt, "planner error:") {
		t.Errorf("planner_response.RawContentExcerpt = %q, want contains 'planner error:'", resp.RawContentExcerpt)
	}
}

var errPlannerCallFailed = errors.New("simulated planner LLM failure")

// erroringPlannerLLM returns plannerErr on the first Complete (the planner's
// own LLM call) and "fallback ok" on subsequent ones so the orchestrator's
// agent loop can still terminate.
type erroringPlannerLLM struct {
	plannerErr error
	calls      int
}

func (l *erroringPlannerLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.calls++
	if l.calls == 1 {
		return nil, l.plannerErr
	}
	return &provider.CompletionResponse{Content: "fallback ok"}, nil
}

func TestOrchestrator_Summarization_EmitsTriggeredAndCompleted(t *testing.T) {
	// Pre-populate the session with messages over the threshold, then call
	// maybeSummarizeSession directly (avoid goroutine timing flakiness — the
	// production caller fires it via `go ...` after Run, but the function
	// itself is fully synchronous once invoked).
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	for i := 0; i < 10; i++ {
		_ = sessions.AddMessage("sess", provider.Message{
			Role: provider.RoleUser, Content: "msg " + strconv.Itoa(i),
		})
	}
	llm := &fakeLLM{responses: []string{"summary of older turns"}}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:               sink,
		SummarizeAfterMessages:  5,
		MaxMessagesAfterSummary: 3,
	})

	orch.maybeSummarizeSession(context.Background(), "sess")

	evs := sink.snapshot()
	trig := findPayloadsByType(t, evs, events.TypeSummarizationTriggered)
	comp := findPayloadsByType(t, evs, events.TypeSummarizationCompleted)
	if len(trig) != 1 {
		t.Fatalf("summarization_triggered count = %d, want 1", len(trig))
	}
	if len(comp) != 1 {
		t.Fatalf("summarization_completed count = %d, want 1", len(comp))
	}
	var tp events.SummarizationTriggeredPayload
	_ = json.Unmarshal(trig[0], &tp)
	if tp.MessageCount != 10 {
		t.Errorf("triggered.MessageCount = %d, want 10", tp.MessageCount)
	}
	if tp.Reason == "" {
		t.Errorf("triggered.Reason empty")
	}
	var cp events.SummarizationCompletedPayload
	_ = json.Unmarshal(comp[0], &cp)
	if cp.SummaryExcerpt != "summary of older turns" {
		t.Errorf("completed.SummaryExcerpt = %q", cp.SummaryExcerpt)
	}
	if cp.KeptMessages != 3 {
		t.Errorf("completed.KeptMessages = %d, want 3", cp.KeptMessages)
	}
	if cp.LatencyMS < 0 {
		t.Errorf("completed.LatencyMS = %d, want >= 0", cp.LatencyMS)
	}
}

func TestOrchestrator_Summarization_PopulatesReleasedKnowledgeIDs(t *testing.T) {
	// When summarization replaces a message range that carried tagged
	// [knowledge_context] blocks, summarization_completed lists the
	// released article_ids so consumers correlate with InjectionState
	// without diffing two reconciliation snapshots.
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	kc := func(id, sha string) string {
		return "[knowledge_context id=\"" + id + "\" sha=\"" + sha + "\"]body[/knowledge_context]\n\nuser text"
	}
	// 10 seeded; SummarizeAfterMessages=5, MaxMessagesAfterSummary=3 →
	// kept = last 3, summarized-away = first 7. Seed two KC-bearing
	// user messages in the first half (will be summarized away) plus
	// a duplicate article_id to verify dedup.
	toSummarize := []provider.Message{
		{Role: provider.RoleUser, Content: kc("kb_a", "sha_a1")},
		{Role: provider.RoleAssistant, Content: "answer-a"},
		{Role: provider.RoleUser, Content: kc("kb_b", "sha_b1")},
		{Role: provider.RoleAssistant, Content: "answer-b"},
		{Role: provider.RoleUser, Content: kc("kb_a", "sha_a2")}, // duplicate article_id
		{Role: provider.RoleAssistant, Content: "answer-a2"},
		{Role: provider.RoleUser, Content: "plain text"},
	}
	keep := []provider.Message{
		{Role: provider.RoleUser, Content: kc("kb_c", "sha_c1")},
		{Role: provider.RoleAssistant, Content: "answer-c"},
		{Role: provider.RoleUser, Content: "tail"},
	}
	for _, m := range append(toSummarize, keep...) {
		_ = sessions.AddMessage("sess", m)
	}

	llm := &fakeLLM{responses: []string{"summary of older turns"}}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:               sink,
		SummarizeAfterMessages:  5,
		MaxMessagesAfterSummary: 3,
	})

	orch.maybeSummarizeSession(context.Background(), "sess")

	comp := findPayloadsByType(t, sink.snapshot(), events.TypeSummarizationCompleted)
	if len(comp) != 1 {
		t.Fatalf("summarization_completed count = %d, want 1", len(comp))
	}
	var cp events.SummarizationCompletedPayload
	_ = json.Unmarshal(comp[0], &cp)
	want := []string{"kb_a", "kb_b"}
	if !reflect.DeepEqual(cp.ReleasedKnowledgeIDs, want) {
		t.Errorf("ReleasedKnowledgeIDs = %v, want %v (dedup of kb_a + kb_b in summarized range; kb_c is in kept and must not appear)",
			cp.ReleasedKnowledgeIDs, want)
	}
}

func TestOrchestrator_Summarization_NoCompletedOnLLMError(t *testing.T) {
	// LLM errors on summarization → triggered fires, completed does not.
	// Consumer-side analytics counts "triggered minus completed" as failure rate.
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	for i := 0; i < 10; i++ {
		_ = sessions.AddMessage("sess", provider.Message{
			Role: provider.RoleUser, Content: "msg",
		})
	}
	llm := &erroringPlannerLLM{plannerErr: errPlannerCallFailed}
	orch := NewWithRules(llm, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:               sink,
		SummarizeAfterMessages:  5,
		MaxMessagesAfterSummary: 3,
	})
	orch.maybeSummarizeSession(context.Background(), "sess")

	evs := sink.snapshot()
	if len(findPayloadsByType(t, evs, events.TypeSummarizationTriggered)) != 1 {
		t.Errorf("triggered count != 1")
	}
	if n := len(findPayloadsByType(t, evs, events.TypeSummarizationCompleted)); n != 0 {
		t.Errorf("completed count = %d, want 0 on LLM error", n)
	}
}

func TestOrchestrator_Summarization_BelowThreshold_NoEvents(t *testing.T) {
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	// Only 2 messages; threshold is 5 → maybeSummarizeSession returns early.
	for i := 0; i < 2; i++ {
		_ = sessions.AddMessage("sess", provider.Message{Role: provider.RoleUser, Content: "m"})
	}
	orch := NewWithRules(&fakeLLM{}, DefaultParser, registry, memory, sessions, OrchestratorOpts{
		EventSink:               sink,
		SummarizeAfterMessages:  5,
		MaxMessagesAfterSummary: 3,
	})
	orch.maybeSummarizeSession(context.Background(), "sess")

	if n := len(findPayloadsByType(t, sink.snapshot(), events.TypeSummarizationTriggered)); n != 0 {
		t.Errorf("triggered fired below threshold (%d events)", n)
	}
}

func TestOrchestrator_Planner_NoEventsWhenPlannerDisabled(t *testing.T) {
	// PipelineEnabled defaults to false → planner is nil → no events.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"plain answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	for _, e := range sink.snapshot() {
		switch e.EventType {
		case events.TypePlannerInvoked, events.TypePlannerRequest,
			events.TypePlannerResponse, events.TypePlannerStep:
			t.Errorf("planner event %q fired with no planner configured", e.EventType)
		}
	}
}

// -----------------------------------------------------------------------------
// Retry instrumentation tests
// -----------------------------------------------------------------------------

func findRetryPayloads(t *testing.T, evs []emit.Event) []events.RetryPayload {
	t.Helper()
	var out []events.RetryPayload
	for _, e := range evs {
		if e.EventType != events.TypeRetry {
			continue
		}
		var p events.RetryPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal retry: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestOrchestrator_Retry_EmitsOnHallucinatedResult(t *testing.T) {
	sink := &recordingEventSink{}
	// Round 1: response contains a fabricated template variable that
	// hasHallucinatedResult matches. Round 2: plain answer to terminate.
	llm := &fakeLLM{responses: []string{
		"Result: {{plugin_output.data.count}} items found",
		"Result: 5 items found",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "count items"); err != nil {
		t.Fatal(err)
	}

	retries := findRetryPayloads(t, sink.snapshot())
	if len(retries) != 1 {
		t.Fatalf("retry count = %d, want 1", len(retries))
	}
	got := retries[0]
	if got.V != events.RetryVersion {
		t.Errorf("Header.V = %d, want %d", got.V, events.RetryVersion)
	}
	if got.Phase != "llm_call" {
		t.Errorf("Phase = %q, want llm_call", got.Phase)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.LastError != "hallucinated tool result" {
		t.Errorf("LastError = %q", got.LastError)
	}
}

func TestOrchestrator_Retry_EmitsOnEmptyResponse(t *testing.T) {
	sink := &recordingEventSink{}
	// Round 1: only an internal block, which StripInternalBlocks reduces to "".
	// fakeParser returns nil so the empty-response retry path fires.
	// Round 2: plain answer to terminate.
	llm := &fakeLLM{responses: []string{
		"[tool_call]unparseable garbage[/tool_call]",
		"answer after retry",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "ask"); err != nil {
		t.Fatal(err)
	}

	retries := findRetryPayloads(t, sink.snapshot())
	if len(retries) != 1 {
		t.Fatalf("retry count = %d, want 1", len(retries))
	}
	got := retries[0]
	if got.Phase != "llm_call" {
		t.Errorf("Phase = %q, want llm_call", got.Phase)
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", got.Attempt)
	}
	if got.LastError != "empty or unparseable response" {
		t.Errorf("LastError = %q", got.LastError)
	}
}

func TestOrchestrator_Retry_EmitsOnPlannerExpectedTools(t *testing.T) {
	sink := &recordingEventSink{}
	// Three rounds of plain text — the agent loop retries twice (toolRetries
	// caps at 2) then gives up and finalizes with the third response.
	llm := &fakeLLM{responses: []string{"plain 1", "plain 2", "plain 3"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	// Seed expected-tools directly on the ctx so the planner-informed retry
	// branch fires without needing a planner setup. Use a sentinel step
	// (no Command) so the retries-exhausted fallback skips server-side
	// invocation and the loop finalizes with the third response.
	ctx := withExpectedTools(context.Background(), []*pipeline.Step{{ID: "direct"}})

	if _, err := orch.Run(ctx, sessID, "do it"); err != nil {
		t.Fatal(err)
	}

	retries := findRetryPayloads(t, sink.snapshot())
	if len(retries) != 2 {
		t.Fatalf("retry count = %d, want 2", len(retries))
	}
	for i, got := range retries {
		if got.Phase != "llm_call" {
			t.Errorf("retry[%d].Phase = %q, want llm_call", i, got.Phase)
		}
		if got.Attempt != i+1 {
			t.Errorf("retry[%d].Attempt = %d, want %d", i, got.Attempt, i+1)
		}
		if got.LastError != "planner expected tool call but LLM returned plain text" {
			t.Errorf("retry[%d].LastError = %q", i, got.LastError)
		}
	}
}

// -----------------------------------------------------------------------------
// Confirmation instrumentation tests
// -----------------------------------------------------------------------------

func findConfirmationResolvedPayloads(t *testing.T, evs []emit.Event) []events.ConfirmationResolvedPayload {
	t.Helper()
	var out []events.ConfirmationResolvedPayload
	for _, e := range evs {
		if e.EventType != events.TypeConfirmationResolved {
			continue
		}
		var p events.ConfirmationResolvedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal confirmation_resolved: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func findConfirmationRequestedPayloads(t *testing.T, evs []emit.Event) []events.ConfirmationRequestedPayload {
	t.Helper()
	var out []events.ConfirmationRequestedPayload
	for _, e := range evs {
		if e.EventType != events.TypeConfirmationRequested {
			continue
		}
		var p events.ConfirmationRequestedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal confirmation_requested: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func TestOrchestrator_Confirmation_PipelineApproved_EmitsResolved(t *testing.T) {
	sink := &recordingEventSink{}
	// fakeLLM is only reached if the pending-pipeline path falls through to the
	// agent loop after approval; the empty pipeline executes immediately so
	// one fallback response is enough to terminate cleanly.
	llm := &fakeLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	orch.pendingMu.Lock()
	orch.pendingPipelines[sessID] = pipeline.NewPipeline(nil, pipeline.PipelineConfig{})
	orch.pendingMu.Unlock()

	if _, err := orch.Run(context.Background(), sessID, "yes"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationResolvedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_resolved count = %d, want 1", len(got))
	}
	if got[0].Choice != "approve" {
		t.Errorf("Choice = %q, want approve", got[0].Choice)
	}
	if got[0].ToolCallID != "" {
		t.Errorf("ToolCallID = %q, want empty for pipeline confirmation", got[0].ToolCallID)
	}
}

func TestOrchestrator_Confirmation_PipelineRejected_EmitsResolved(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"never reached"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	orch.pendingMu.Lock()
	orch.pendingPipelines[sessID] = pipeline.NewPipeline(nil, pipeline.PipelineConfig{})
	orch.pendingMu.Unlock()

	if _, err := orch.Run(context.Background(), sessID, "no"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationResolvedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_resolved count = %d, want 1", len(got))
	}
	if got[0].Choice != "reject" {
		t.Errorf("Choice = %q, want reject", got[0].Choice)
	}
}

func TestOrchestrator_Confirmation_ToolCallApproved_EmitsResolved(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final summary"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	pending := &ToolCall{
		ID:     "pending-1",
		Plugin: "gitlab",
		Action: "analyze_code",
		Args:   map[string]string{},
	}
	orch.pendingMu.Lock()
	orch.pendingToolCalls[sessID] = pending
	orch.pendingMu.Unlock()

	if _, err := orch.Run(context.Background(), sessID, "yes"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationResolvedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_resolved count = %d, want 1", len(got))
	}
	if got[0].Choice != "approve" {
		t.Errorf("Choice = %q, want approve", got[0].Choice)
	}
	if got[0].ToolCallID != "pending-1" {
		t.Errorf("ToolCallID = %q, want pending-1", got[0].ToolCallID)
	}
}

func TestOrchestrator_Confirmation_ToolCallRejected_EmitsResolved(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"never reached"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	pending := &ToolCall{
		ID:     "pending-2",
		Plugin: "gitlab",
		Action: "analyze_code",
		Args:   map[string]string{},
	}
	orch.pendingMu.Lock()
	orch.pendingToolCalls[sessID] = pending
	orch.pendingMu.Unlock()

	if _, err := orch.Run(context.Background(), sessID, "no"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationResolvedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_resolved count = %d, want 1", len(got))
	}
	if got[0].Choice != "reject" {
		t.Errorf("Choice = %q, want reject", got[0].Choice)
	}
	if got[0].ToolCallID != "pending-2" {
		t.Errorf("ToolCallID = %q, want pending-2", got[0].ToolCallID)
	}
}

// confirmingExecutor stubs a confirmation-plugin call: returns JSON requiring
// confirmation so the orchestrator pauses on the next tool call.
type confirmingExecutor struct{}

func (confirmingExecutor) Execute(_ context.Context, call ToolCall) ToolResult {
	return ToolResult{
		CallID:  call.ID,
		Content: `{"requires_confirmation":true,"confirm_before_step":0}`,
	}
}

func TestOrchestrator_Confirmation_ToolCallRequiresConfirmation_EmitsRequested(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:        "tc-1",
			Name:      "gitlab__analyze_code",
			Arguments: map[string]string{},
		}},
		textAfter: "summary",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "gitlab",
		Actions: []Action{{Name: "analyze_code"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:    "conf",
		Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:          sink,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})

	if _, err := orch.Run(context.Background(), "sess", "analyze it"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationRequestedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_requested count = %d, want 1", len(got))
	}
	if got[0].V != events.ConfirmationRequestedVersion {
		t.Errorf("Header.V = %d, want %d", got[0].V, events.ConfirmationRequestedVersion)
	}
	if got[0].ToolCallID != "tc-1" {
		t.Errorf("ToolCallID = %q, want tc-1", got[0].ToolCallID)
	}
	if len(got[0].Choices) != 2 || got[0].Choices[0] != "approve" || got[0].Choices[1] != "reject" {
		t.Errorf("Choices = %v, want [approve reject]", got[0].Choices)
	}
	if got[0].Prompt == "" {
		t.Errorf("Prompt is empty")
	}
}

func TestOrchestrator_Confirmation_ReadOnlyAction_SkipsPrompt(t *testing.T) {
	// A read-only action must never trigger the confirmation gate, even
	// when the confirmation plugin is wired and would otherwise fail-safe
	// to "require confirmation" (the local-dev signal that surfaced the
	// noise originally). The action's manifest carries
	// ReadOnly=true and the orchestrator short-circuits before
	// checkConfirmationPlugin runs — so no confirmation_requested event
	// reaches the sink, the tool executes inline, and the LLM's
	// "textAfter" closes the turn normally.
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:        "tc-readonly",
			Name:      "gitlab__list_issues",
			Arguments: map[string]string{},
		}},
		textAfter: "here are your issues",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "gitlab",
		Actions: []Action{{Name: "list_issues", ReadOnly: true}},
	}, &echoExecutor{})
	// A confirming executor backs the confirmation plugin — if the gate
	// runs, it returns RequiresConfirmation=true and we'd see a
	// confirmation_requested event in the sink. The point of this test
	// is that this code path must not even be reached.
	_ = registry.Register(PluginCapability{
		Name:    "conf",
		Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:          sink,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})

	if _, err := orch.Run(context.Background(), "sess", "list my issues"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationRequestedPayloads(t, sink.snapshot())
	if len(got) != 0 {
		t.Errorf("confirmation_requested count = %d, want 0 for read_only action (got prompts: %v)",
			len(got), got)
	}
}

func TestOrchestrator_Confirmation_ReadOnlyAction_MatchesPrefixedManifestName(t *testing.T) {
	// Regression: mcp-plugin stores each action's Name in the
	// prefixed form "server__tool" (e.g. "timly__list-items"). The LLM
	// emits the same name; parseToolName splits on `__` so the
	// orchestrator-side ToolCall ends up with Plugin="timly",
	// Action="list-items" (the unprefixed form). IsActionReadOnly's
	// lookup must accept both shapes — without it the short-circuit
	// would silently miss every mcp-sourced read-only tool.
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:        "tc-mcp",
			Name:      "timly__list-items",
			Arguments: map[string]string{},
		}},
		textAfter: "results above",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	// Capability registered exactly the way mcp-plugin emits it:
	// Cap.Name = "mcp", Actions carry the server-prefixed action name.
	// An alias "timly" -> "mcp" resolves the LLM's call name to the
	// underlying capability.
	_ = registry.Register(PluginCapability{
		Name:    "mcp",
		Actions: []Action{{Name: "timly__list-items", ReadOnly: true}},
	}, &echoExecutor{})
	if err := registry.RegisterAlias("timly", "mcp"); err != nil {
		t.Fatalf("RegisterAlias: %v", err)
	}
	_ = registry.Register(PluginCapability{
		Name:    "conf",
		Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:          sink,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})

	if _, err := orch.Run(context.Background(), "sess", "list my items"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationRequestedPayloads(t, sink.snapshot())
	if len(got) != 0 {
		t.Errorf("confirmation_requested count = %d, want 0 for mcp-style prefixed read-only action", len(got))
	}
}

func TestOrchestrator_Confirmation_NonReadOnlyAction_StillPrompts(t *testing.T) {
	// Sibling to the ReadOnly skip test: an action that is NOT declared
	// read-only must still go through the confirmation gate, even if a
	// sibling action on the same plugin is read-only. Locks in that the
	// short-circuit is keyed strictly on the per-action flag and doesn't
	// accidentally infect every call to the same plugin.
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:        "tc-write",
			Name:      "gitlab__create_issue",
			Arguments: map[string]string{},
		}},
		textAfter: "done",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "gitlab",
		Actions: []Action{
			{Name: "list_issues", ReadOnly: true},
			{Name: "create_issue"}, // ReadOnly=false (default)
		},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:    "conf",
		Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:          sink,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})

	if _, err := orch.Run(context.Background(), "sess", "create an issue"); err != nil {
		t.Fatal(err)
	}

	got := findConfirmationRequestedPayloads(t, sink.snapshot())
	if len(got) != 1 {
		t.Fatalf("confirmation_requested count = %d, want 1 for non-read-only action", len(got))
	}
	if got[0].ToolCallID != "tc-write" {
		t.Errorf("ToolCallID = %q, want tc-write", got[0].ToolCallID)
	}
}

// -----------------------------------------------------------------------------
// parent_id linkage tests — verify that producer-side event_id generation
// and WithParent ctx wiring produces the expected event tree.
// -----------------------------------------------------------------------------

// findEventByType returns the first event of the given type, or nil.
func findEventByType(evs []emit.Event, eventType string) *emit.Event {
	for i := range evs {
		if evs[i].EventType == eventType {
			return &evs[i]
		}
	}
	return nil
}

// findEventsByType returns all events of the given type.
func findEventsByType(evs []emit.Event, eventType string) []emit.Event {
	var out []emit.Event
	for i := range evs {
		if evs[i].EventType == eventType {
			out = append(out, evs[i])
		}
	}
	return out
}

func TestParentID_TurnStartParentsUserMessage(t *testing.T) {
	// Post-RFC-#249-Phase-2 tree: user_message is the root of every turn;
	// turn_start (and the preparer-phase events emitted between them)
	// hang off user_message. The old "turn_start is root" assertion was
	// pre-RFC and is replaced here.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"hello back"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "hi"); err != nil {
		t.Fatal(err)
	}
	snap := sink.snapshot()
	um := findEventByType(snap, events.TypeUserMessage)
	ts := findEventByType(snap, events.TypeTurnStart)
	if um == nil {
		t.Fatal("user_message not emitted")
	}
	if ts == nil {
		t.Fatal("turn_start not emitted")
	}
	if ts.ID == "" {
		t.Errorf("turn_start.ID empty — producer-side id generation broken")
	}
	if um.ParentID != "" {
		t.Errorf("user_message.ParentID = %q, want empty (root of turn tree)", um.ParentID)
	}
	if ts.ParentID != um.ID {
		t.Errorf("turn_start.ParentID = %q, want user_message.ID (%q)", ts.ParentID, um.ID)
	}
}

func TestParentID_ToolCallResultParentsExtracted(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID:        "call-1",
			Name:      "gitlab__analyze_code",
			Arguments: map[string]string{},
		}},
		textAfter: "done",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "analyze"); err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	extracted := findEventByType(evs, events.TypeToolCallExtracted)
	result := findEventByType(evs, events.TypeToolCallResult)
	if extracted == nil || result == nil {
		t.Fatalf("missing events: extracted=%v result=%v", extracted != nil, result != nil)
	}
	if extracted.ID == "" {
		t.Errorf("tool_call_extracted.ID empty")
	}
	if result.ParentID != extracted.ID {
		t.Errorf("tool_call_result.ParentID = %q, want %q (extracted.ID)", result.ParentID, extracted.ID)
	}
}

func TestParentID_PlannerRequestResponseStepParentsInvoked(t *testing.T) {
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{
		// Round 1: planner returns pipeline plan with 2 steps.
		`{"type":"pipeline","steps":[
			{"id":"s1","name":"one","plugin":"gitlab","action":"analyze_code","args":{},"depends_on":[]},
			{"id":"s2","name":"two","plugin":"gitlab","action":"analyze_code","args":{},"depends_on":["s1"]}
		]}`,
		"continue",
		"final",
	}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithPlanner(llm, parser, sink)
	_, _ = orch.Run(context.Background(), sessID, "two-step task")
	evs := sink.snapshot()
	invoked := findEventByType(evs, events.TypePlannerInvoked)
	req := findEventByType(evs, events.TypePlannerRequest)
	resp := findEventByType(evs, events.TypePlannerResponse)
	steps := findEventsByType(evs, events.TypePlannerStep)
	if invoked == nil || req == nil || resp == nil {
		t.Fatalf("missing planner events: invoked=%v request=%v response=%v",
			invoked != nil, req != nil, resp != nil)
	}
	if invoked.ID == "" {
		t.Errorf("planner_invoked.ID empty")
	}
	if req.ParentID != invoked.ID {
		t.Errorf("planner_request.ParentID = %q, want %q", req.ParentID, invoked.ID)
	}
	if resp.ParentID != invoked.ID {
		t.Errorf("planner_response.ParentID = %q, want %q", resp.ParentID, invoked.ID)
	}
	for i, s := range steps {
		if s.ParentID != invoked.ID {
			t.Errorf("planner_step[%d].ParentID = %q, want %q", i, s.ParentID, invoked.ID)
		}
	}
}

func TestParentID_ConfirmationResolvedParentsRequested_ToolCall(t *testing.T) {
	// Turn 1: native LLM emits a tool call, confirmation plugin says
	// requires_confirmation → confirmation_requested fires, pending state
	// stored. Turn 2: user replies "yes" → confirmation_resolved fires
	// with parent_id = requested.ID.
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID: "tc-7", Name: "gitlab__analyze_code", Arguments: map[string]string{},
		}},
		textAfter: "after-confirm-summary",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "gitlab",
		Actions: []Action{{Name: "analyze_code"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name:    "conf",
		Actions: []Action{{Name: "check"}},
	}, confirmingExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("sess", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:          sink,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
	})

	if _, err := orch.Run(context.Background(), "sess", "analyze"); err != nil {
		t.Fatal(err)
	}
	if _, err := orch.Run(context.Background(), "sess", "yes"); err != nil {
		t.Fatal(err)
	}

	evs := sink.snapshot()
	req := findEventByType(evs, events.TypeConfirmationRequested)
	resolved := findEventByType(evs, events.TypeConfirmationResolved)
	if req == nil || resolved == nil {
		t.Fatalf("missing events: requested=%v resolved=%v", req != nil, resolved != nil)
	}
	if req.ID == "" {
		t.Errorf("confirmation_requested.ID empty")
	}
	if resolved.ParentID != req.ID {
		t.Errorf("confirmation_resolved.ParentID = %q, want %q (requested.ID)", resolved.ParentID, req.ID)
	}
}

func TestParentID_ConfirmationResolvedParentsRequested_Pipeline(t *testing.T) {
	// Inject a pending pipeline + the matching confirmation_requested id
	// directly so we don't have to run a full planner+confirmation turn
	// first. The under-test linkage is the *resolved* side reading from
	// pendingConfirmationIDs and stamping it as parent.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"done"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	const fakeReqID = "00000000000000000000000000000001"
	orch.pendingMu.Lock()
	orch.pendingPipelines[sessID] = pipeline.NewPipeline(nil, pipeline.PipelineConfig{})
	orch.pendingConfirmationIDs[sessID] = fakeReqID
	orch.pendingMu.Unlock()

	if _, err := orch.Run(context.Background(), sessID, "yes"); err != nil {
		t.Fatal(err)
	}
	resolved := findEventByType(sink.snapshot(), events.TypeConfirmationResolved)
	if resolved == nil {
		t.Fatal("confirmation_resolved not emitted")
	}
	if resolved.ParentID != fakeReqID {
		t.Errorf("confirmation_resolved.ParentID = %q, want %q", resolved.ParentID, fakeReqID)
	}
}

// emittingLLM mimics what the real provider does: emits an llm_response
// event via the shared sink and threads the returned event id onto
// CompletionResponse.EventID. The orchestrator then wraps ctx with that
// id before dispatching tool calls, so tool_call_extracted should parent
// to llm_response. This is the only fake LLM in the test suite that
// closes the provider→orchestrator parent_id contract.
type emittingLLM struct {
	sink      emit.Sink
	toolCalls []provider.ToolCall
	textAfter string
	calls     int
}

func (l *emittingLLM) Complete(ctx context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.calls++
	id := emit.EmitLLMResponse(ctx, l.sink, emit.LLMResponseArgs{
		RawContent:   "synthetic",
		FinishReason: "stop",
	})
	if l.calls == 1 {
		return &provider.CompletionResponse{ToolCalls: l.toolCalls, EventID: id}, nil
	}
	return &provider.CompletionResponse{Content: l.textAfter, EventID: id}, nil
}

func (l *emittingLLM) SupportsFeature(f provider.Feature) bool {
	return f == provider.FeatureTools
}

func TestParentID_ToolCallExtractedParentsLLMResponse(t *testing.T) {
	// Cross-package linkage test: the LLM-equivalent (a fake that mirrors
	// what the openai provider does) emits llm_response and surfaces its
	// id on CompletionResponse.EventID. The orchestrator stamps that id
	// as parent on ctx, so the subsequent tool_call_extracted event must
	// link back to the llm_response it was derived from. This is the
	// contract that would silently break if the provider stopped
	// populating EventID or the orchestrator stopped reading it.
	sink := &recordingEventSink{}
	llm := &emittingLLM{
		sink: sink,
		toolCalls: []provider.ToolCall{{
			ID:        "tc-9",
			Name:      "gitlab__analyze_code",
			Arguments: map[string]string{},
		}},
		textAfter: "done",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "go"); err != nil {
		t.Fatal(err)
	}
	evs := sink.snapshot()
	llmResp := findEventByType(evs, events.TypeLLMResponse)
	extracted := findEventByType(evs, events.TypeToolCallExtracted)
	if llmResp == nil || extracted == nil {
		t.Fatalf("missing events: llm_response=%v extracted=%v", llmResp != nil, extracted != nil)
	}
	if llmResp.ID == "" {
		t.Fatal("llm_response.ID empty — emit framework regression")
	}
	if extracted.ParentID != llmResp.ID {
		t.Errorf("tool_call_extracted.ParentID = %q, want %q (llm_response.ID)",
			extracted.ParentID, llmResp.ID)
	}
}

func TestParentID_EveryEventHasID(t *testing.T) {
	// Smoke test: across a representative run with planner + tool calls,
	// every emitted event has a non-empty ID. Catches future helpers that
	// might forget to thread the send() return value through.
	sink := &recordingEventSink{}
	llm := &nativeToolCallingLLM{
		toolCalls: []provider.ToolCall{{
			ID: "x", Name: "gitlab__analyze_code", Arguments: map[string]string{},
		}},
		textAfter: "done",
	}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)
	if _, err := orch.Run(context.Background(), sessID, "go"); err != nil {
		t.Fatal(err)
	}
	for i, e := range sink.snapshot() {
		if e.ID == "" {
			t.Errorf("event[%d] type=%q has empty ID", i, e.EventType)
		}
	}
}

func TestOrchestrator_PreparerPhase_EmitsRetrievalAndDecision(t *testing.T) {
	// RFC #249 Phase 2: a preparer that returns structured knowledge /
	// glossary / tool candidates should trigger three retrieval events
	// (one per non-empty corpus) plus a composite preparer_decision —
	// all parented to user_message so the session timeline reads as a
	// tree.
	//
	// Mode is instrumentation_only since Phase 2 has no dedup state;
	// every candidate appears under Knowledge.Injected with that
	// reason, and tool names land under Tools.Tier1New.

	preparerJSON := `{
		"send_to_llm": true,
		"message": "do the thing",
		"knowledge_candidates": [
			{"article_id": "kb_a", "title": "A", "content": "body-a", "content_sha256": "sha-a", "score": 0.9, "source": "knowledge_base"},
			{"article_id": "kb_b", "title": "B", "content": "body-bb", "content_sha256": "sha-b", "score": 0.6}
		],
		"glossary_candidates": [
			{"term": "ticket", "content": "definition", "score": 0.71}
		],
		"tool_candidates": [
			{"tool_name": "gitlab__analyze_code", "score": 0.88, "position_in_results": 0}
		],
		"retrieval_metrics": {
			"knowledge": {"search_text_source": "enriched", "top_k": 5, "min_score": 0.45, "latency_ms": 142},
			"glossary":  {"top_k": 3, "min_score": 0.50, "latency_ms": 38},
			"tools":     {"top_k": 8, "min_score": 0.60, "latency_ms": 98}
		}
	}`

	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG preparer",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:        sink,
		ContentPreparers: []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
	})

	if _, err := orch.Run(context.Background(), "s1", "do the thing"); err != nil {
		t.Fatal(err)
	}

	evs := sink.snapshot()
	userMsg := findEventByType(evs, events.TypeUserMessage)
	if userMsg == nil || userMsg.ID == "" {
		t.Fatal("user_message event missing or has empty ID")
	}

	// One event per corpus retrieved + one composite decision.
	wantTypes := []string{
		events.TypeKnowledgeRetrieval,
		events.TypeGlossaryRetrieval,
		events.TypeToolRetrieval,
		events.TypePreparerDecision,
	}
	for _, wantType := range wantTypes {
		e := findEventByType(evs, wantType)
		if e == nil {
			t.Errorf("missing event of type %q", wantType)
			continue
		}
		if e.ParentID != userMsg.ID {
			t.Errorf("%s.ParentID = %q, want user_message.ID (%q)", wantType, e.ParentID, userMsg.ID)
		}
	}

	// Spot-check the knowledge_retrieval payload: hits + metrics from
	// the plugin should round-trip into the event.
	kr := findEventByType(evs, events.TypeKnowledgeRetrieval)
	if kr == nil {
		t.Fatal("knowledge_retrieval event missing")
	}
	var krPayload events.KnowledgeRetrievalPayload
	if err := json.Unmarshal(kr.Payload, &krPayload); err != nil {
		t.Fatalf("unmarshal knowledge_retrieval payload: %v", err)
	}
	if len(krPayload.Hits) != 2 || krPayload.Hits[0].ArticleID != "kb_a" {
		t.Errorf("knowledge_retrieval hits mismatch: %+v", krPayload.Hits)
	}
	if krPayload.SearchTextSource != events.SearchTextSourceEnriched {
		t.Errorf("knowledge_retrieval SearchTextSource = %q, want %q",
			krPayload.SearchTextSource, events.SearchTextSourceEnriched)
	}
	if krPayload.TopK != 5 || krPayload.MinScore != 0.45 || krPayload.LatencyMS != 142 {
		t.Errorf("knowledge_retrieval metrics mismatch: TopK=%d MinScore=%v LatencyMS=%d",
			krPayload.TopK, krPayload.MinScore, krPayload.LatencyMS)
	}

	// Spot-check glossary + tool retrievals too: per-corpus metrics
	// must round-trip independently. A regression where the wrong
	// corpus's metrics get pasted into knowledge_retrieval shows up
	// here.
	gr := findEventByType(evs, events.TypeGlossaryRetrieval)
	if gr == nil {
		t.Fatal("glossary_retrieval event missing")
	}
	var grPayload events.GlossaryRetrievalPayload
	if err := json.Unmarshal(gr.Payload, &grPayload); err != nil {
		t.Fatalf("unmarshal glossary_retrieval payload: %v", err)
	}
	if len(grPayload.Hits) != 1 || grPayload.Hits[0].Term != "ticket" {
		t.Errorf("glossary_retrieval hits mismatch: %+v", grPayload.Hits)
	}
	if grPayload.LatencyMS != 38 || grPayload.TopK != 3 {
		t.Errorf("glossary_retrieval metrics mismatch: TopK=%d LatencyMS=%d", grPayload.TopK, grPayload.LatencyMS)
	}

	tr := findEventByType(evs, events.TypeToolRetrieval)
	if tr == nil {
		t.Fatal("tool_retrieval event missing")
	}
	var trPayload events.ToolRetrievalPayload
	if err := json.Unmarshal(tr.Payload, &trPayload); err != nil {
		t.Fatalf("unmarshal tool_retrieval payload: %v", err)
	}
	if len(trPayload.Hits) != 1 || trPayload.Hits[0].ToolName != "gitlab__analyze_code" {
		t.Errorf("tool_retrieval hits mismatch: %+v", trPayload.Hits)
	}
	if trPayload.LatencyMS != 98 || trPayload.TopK != 8 {
		t.Errorf("tool_retrieval metrics mismatch: TopK=%d LatencyMS=%d", trPayload.TopK, trPayload.LatencyMS)
	}

	// preparer_decision should carry mode=instrumentation_only with
	// both knowledge and tools populated from the candidate slices.
	pd := findEventByType(evs, events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision event missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal preparer_decision payload: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeInstrumentationOnly {
		t.Errorf("preparer_decision Mode = %q, want %q",
			pdPayload.Mode, events.PreparerDecisionModeInstrumentationOnly)
	}
	if len(pdPayload.Knowledge.CandidateIDs) != 2 {
		t.Errorf("preparer_decision Knowledge.CandidateIDs len = %d, want 2", len(pdPayload.Knowledge.CandidateIDs))
	}
	if len(pdPayload.Knowledge.Injected) != 2 {
		t.Errorf("preparer_decision Knowledge.Injected len = %d, want 2", len(pdPayload.Knowledge.Injected))
	}
	if pdPayload.Knowledge.InjectedBytes != len("body-a")+len("body-bb") {
		t.Errorf("preparer_decision Knowledge.InjectedBytes = %d, want %d",
			pdPayload.Knowledge.InjectedBytes, len("body-a")+len("body-bb"))
	}
	if len(pdPayload.Tools.Tier1New) != 1 || pdPayload.Tools.Tier1New[0] != "gitlab__analyze_code" {
		t.Errorf("preparer_decision Tools.Tier1New = %v", pdPayload.Tools.Tier1New)
	}
}

func TestOrchestrator_PreparerPhase_NoPreparerNoDecisionEvent(t *testing.T) {
	// When the orchestrator has no preparers (or all are STT), the
	// preparer_decision event should NOT fire — emitting an empty
	// composite would just produce noise. user_message and turn_start
	// still emit as normal.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"hi"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	orch, sessID := setupOrchestratorWithSink(llm, parser, sink)

	if _, err := orch.Run(context.Background(), sessID, "hello"); err != nil {
		t.Fatal(err)
	}
	for _, e := range sink.snapshot() {
		if e.EventType == events.TypePreparerDecision {
			t.Errorf("preparer_decision should not emit when no preparers retrieve anything; got payload: %s", e.Payload)
		}
		if e.EventType == events.TypeKnowledgeRetrieval || e.EventType == events.TypeGlossaryRetrieval {
			t.Errorf("%s should not emit without a preparer producing candidates", e.EventType)
		}
	}
}

func TestOrchestrator_PreparerPhase_LegacyPluginNoCandidates(t *testing.T) {
	// A pre-RFC plugin returning only `message` + `relevant_tools`
	// still gets a tool_retrieval event (synthesized from relevant_tools
	// with score=0) plus a preparer_decision capturing the tool names
	// under Tier1New. knowledge_retrieval / glossary_retrieval do NOT
	// fire — there are no candidates and no metrics signaling those
	// corpora ran.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "do the thing",
		"relevant_tools": ["gitlab__analyze_code", "jira__create_issue"]
	}`

	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "legacy-plugin", Description: "Legacy",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	_ = registry.Register(PluginCapability{
		Name: "gitlab", Description: "GitLab",
		Actions: []Action{{Name: "analyze_code", Description: "Analyze"}},
	}, &echoExecutor{})
	_ = registry.Register(PluginCapability{
		Name: "jira", Description: "Jira",
		Actions: []Action{{Name: "create_issue", Description: "Create"}},
	}, &echoExecutor{})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:        sink,
		ContentPreparers: []ContentPreparerEntry{{Plugin: "legacy-plugin", Action: "prepare"}},
	})

	if _, err := orch.Run(context.Background(), "s1", "do the thing"); err != nil {
		t.Fatal(err)
	}

	evs := sink.snapshot()
	if findEventByType(evs, events.TypeKnowledgeRetrieval) != nil {
		t.Error("knowledge_retrieval must not fire for legacy plugins without knowledge candidates")
	}
	if findEventByType(evs, events.TypeGlossaryRetrieval) != nil {
		t.Error("glossary_retrieval must not fire for legacy plugins without glossary candidates")
	}
	tr := findEventByType(evs, events.TypeToolRetrieval)
	if tr == nil {
		t.Fatal("tool_retrieval should fire from legacy relevant_tools")
	}
	var trPayload events.ToolRetrievalPayload
	if err := json.Unmarshal(tr.Payload, &trPayload); err != nil {
		t.Fatalf("unmarshal tool_retrieval payload: %v", err)
	}
	if len(trPayload.Hits) != 2 {
		t.Errorf("tool_retrieval hits len = %d, want 2 (from relevant_tools)", len(trPayload.Hits))
	}
	if trPayload.Hits[0].Score != 0 {
		t.Errorf("tool_retrieval legacy synthesized hit must have score=0, got %v", trPayload.Hits[0].Score)
	}

	pd := findEventByType(evs, events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision should still fire for legacy tool retrieval")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal preparer_decision payload: %v", err)
	}
	if len(pdPayload.Tools.Tier1New) != 2 {
		t.Errorf("preparer_decision Tools.Tier1New len = %d, want 2", len(pdPayload.Tools.Tier1New))
	}
}

func TestOrchestrator_PreparerPhase_MultiPreparerAggregation(t *testing.T) {
	// When the orchestrator runs more than one preparer per turn, the
	// composite preparer_decision must aggregate the candidate lists
	// across all of them. Two preparers each returning one knowledge
	// candidate should produce two CandidateIDs + two Injected entries
	// with InjectedBytes summed.
	prepA := `{"send_to_llm": true, "message": "x",
	 "knowledge_candidates": [{"article_id": "kb_a", "content": "AAA", "score": 0.9}]}`
	prepB := `{"send_to_llm": true, "message": "x",
	 "knowledge_candidates": [{"article_id": "kb_b", "content": "BBBB", "score": 0.7}]}`

	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-a", Description: "RAG A",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepA})
	_ = registry.Register(PluginCapability{
		Name: "rag-b", Description: "RAG B",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepB})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink: sink,
		ContentPreparers: []ContentPreparerEntry{
			{Plugin: "rag-a", Action: "prepare"},
			{Plugin: "rag-b", Action: "prepare"},
		},
	})

	if _, err := orch.Run(context.Background(), "s1", "do the thing"); err != nil {
		t.Fatal(err)
	}

	evs := sink.snapshot()
	// Two knowledge_retrieval events fire (one per preparer), each
	// carrying that preparer's single hit. They must NOT be merged
	// into a single event — per-preparer granularity is the audit
	// trail.
	var kCount int
	for _, e := range evs {
		if e.EventType == events.TypeKnowledgeRetrieval {
			kCount++
		}
	}
	if kCount != 2 {
		t.Errorf("knowledge_retrieval event count = %d, want 2 (one per preparer)", kCount)
	}

	// preparer_decision fires exactly once with both candidates merged.
	pd := findEventByType(evs, events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision event missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal preparer_decision payload: %v", err)
	}
	if len(pdPayload.Knowledge.CandidateIDs) != 2 {
		t.Errorf("CandidateIDs len = %d, want 2 (kb_a + kb_b across both preparers)",
			len(pdPayload.Knowledge.CandidateIDs))
	}
	if pdPayload.Knowledge.InjectedBytes != len("AAA")+len("BBBB") {
		t.Errorf("InjectedBytes = %d, want %d (sum across preparers)",
			pdPayload.Knowledge.InjectedBytes, len("AAA")+len("BBBB"))
	}
}

func TestOrchestrator_PreparerPhase_DedupEnabledEmitsFullMode(t *testing.T) {
	// RFC #249 Phase 3: with knowledge_dedup.enabled + a wired
	// InjectionStateStore, the orchestrator runs the dedup decision
	// over the candidate list, rewrites the user-turn content with
	// an ID-tagged [knowledge_context] block, persists the updated
	// state, and emits preparer_decision with mode=full.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "[knowledge_context]\nplugin-rendered body\n[/knowledge_context]\n\nuser question",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body-a", "content_sha256": "sha-a", "score": 0.9},
			{"article_id": "kb_b", "content": "body-bb", "content_sha256": "sha-b", "score": 0.6}
		]
	}`

	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{"final answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	dedupStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG preparer",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:        sink,
		ContentPreparers: []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		KnowledgeDedup: KnowledgeDedupConfig{
			Enabled:                true,
			ReinjectScoreThreshold: 0.95,
			ReinjectTopKForce:      3,
			CapPerTurn:             5,
		},
		InjectionStateStore: dedupStore,
	})

	if _, err := orch.Run(context.Background(), "s1", "user question"); err != nil {
		t.Fatal(err)
	}

	// preparer_decision must report mode=full with both candidates injected
	// as "new" (first turn, empty state).
	evs := sink.snapshot()
	pd := findEventByType(evs, events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision event missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal preparer_decision payload: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeFull {
		t.Errorf("Mode = %q, want %q", pdPayload.Mode, events.PreparerDecisionModeFull)
	}
	if len(pdPayload.Knowledge.Injected) != 2 {
		t.Fatalf("Injected len = %d, want 2", len(pdPayload.Knowledge.Injected))
	}
	for _, item := range pdPayload.Knowledge.Injected {
		if item.Reason != "new" {
			t.Errorf("first-turn injected reason = %q, want %q", item.Reason, "new")
		}
	}

	// The state store must have been written with both SHAs.
	if dedupStore.updateCalls != 1 {
		t.Errorf("UpdateInjectionState called %d times, want 1", dedupStore.updateCalls)
	}
	if len(dedupStore.lastWritten.KnownKnowledge) != 2 {
		t.Errorf("persisted KnownKnowledge len = %d, want 2", len(dedupStore.lastWritten.KnownKnowledge))
	}

	// The LLM's user message must carry the rebuilt ID-tagged KC
	// block (with id="kb_a") and the user's original text, NOT the
	// plugin's pre-tagged "plugin-rendered body" string.
	if len(llm.requests) == 0 {
		t.Fatal("LLM was not called")
	}
	var lastUser provider.Message
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleUser {
			lastUser = m
		}
	}
	if !strings.Contains(lastUser.Content, `[knowledge_context id="kb_a"`) {
		t.Errorf("LLM user message missing ID-tagged KC block, got: %q", lastUser.Content)
	}
	if strings.Contains(lastUser.Content, "plugin-rendered body") {
		t.Errorf("LLM user message must drop plugin's pre-tagged KC, got: %q", lastUser.Content)
	}
	if !strings.Contains(lastUser.Content, "user question") {
		t.Errorf("LLM user message missing user text, got: %q", lastUser.Content)
	}
}

func TestOrchestrator_PreparerPhase_DedupSecondTurnReusesKnownState(t *testing.T) {
	// Two-turn test: turn 1 introduces two candidates, turn 2 returns
	// the same candidates. With reinject_top_k_force=1 only kb_a (at
	// index 0) re-injects via top_k_force; kb_b is skipped as
	// content_sha_already_known.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "user question",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body-a", "content_sha256": "sha-a", "score": 0.7},
			{"article_id": "kb_b", "content": "body-bb", "content_sha256": "sha-b", "score": 0.5}
		]
	}`

	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{"first", "second"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	dedupStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG preparer",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:        sink,
		ContentPreparers: []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		KnowledgeDedup: KnowledgeDedupConfig{
			Enabled:                true,
			ReinjectScoreThreshold: 0.95,
			ReinjectTopKForce:      1,
			CapPerTurn:             5,
		},
		InjectionStateStore: dedupStore,
	})

	// Turn 1: both candidates inject as "new".
	if _, err := orch.Run(context.Background(), "s1", "first user question"); err != nil {
		t.Fatal(err)
	}
	// Turn 2: same candidates, expect 1 injected (top_k_force) + 1 skipped (known).
	if _, err := orch.Run(context.Background(), "s1", "second user question"); err != nil {
		t.Fatal(err)
	}

	// LLM-content sanity check: the second-turn user message must
	// carry kb_a's body but NOT kb_b's body (kb_b was skipped as
	// already_known). The preparer replaces the user's literal input
	// with its own `pr.Message`, then dedup prepends the rebuilt KC
	// block — so the assertion targets the rendered KC content, not
	// the user's typed text.
	if len(llm.requests) < 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(llm.requests))
	}
	turn2Msgs := llm.requests[1].Messages
	var lastTurn2User string
	for _, m := range turn2Msgs {
		if m.Role == provider.RoleUser {
			lastTurn2User = m.Content
		}
	}
	if lastTurn2User == "" {
		t.Fatalf("turn-2 user message missing from LLM request: %+v", turn2Msgs)
	}
	if !strings.Contains(lastTurn2User, "body-a") {
		t.Errorf("turn-2 user message must carry kb_a's body, got: %q", lastTurn2User)
	}
	if strings.Contains(lastTurn2User, "body-bb") {
		t.Errorf("turn-2 user message must NOT carry kb_b's body (already known), got: %q", lastTurn2User)
	}
	if !strings.Contains(lastTurn2User, `id="kb_a"`) {
		t.Errorf("turn-2 KC block must be ID-tagged, got: %q", lastTurn2User)
	}

	// Find the SECOND preparer_decision event (turn 2's). The first one
	// reports turn 1's "all new" decision.
	evs := sink.snapshot()
	var pds []emit.Event
	for _, e := range evs {
		if e.EventType == events.TypePreparerDecision {
			pds = append(pds, e)
		}
	}
	if len(pds) != 2 {
		t.Fatalf("got %d preparer_decision events, want 2", len(pds))
	}

	var turn2 events.PreparerDecisionPayload
	if err := json.Unmarshal(pds[1].Payload, &turn2); err != nil {
		t.Fatalf("unmarshal turn-2 preparer_decision payload: %v", err)
	}
	if len(turn2.Knowledge.Injected) != 1 || turn2.Knowledge.Injected[0].ArticleID != "kb_a" {
		t.Fatalf("turn 2 must inject only kb_a, got %+v", turn2.Knowledge.Injected)
	}
	if turn2.Knowledge.Injected[0].Reason != "top_k_force" {
		t.Errorf("kb_a reason = %q, want top_k_force", turn2.Knowledge.Injected[0].Reason)
	}
	if len(turn2.Knowledge.SkippedKnown) != 1 || turn2.Knowledge.SkippedKnown[0].ArticleID != "kb_b" {
		t.Fatalf("turn 2 must skip kb_b, got %+v", turn2.Knowledge.SkippedKnown)
	}
	if turn2.Knowledge.SkippedKnown[0].Reason != "content_sha_already_known" {
		t.Errorf("kb_b skip reason = %q", turn2.Knowledge.SkippedKnown[0].Reason)
	}

	// The dedup store should still hold both SHAs after turn 2 (not
	// duplicated despite being seen twice across turns).
	if len(dedupStore.lastWritten.KnownKnowledge) != 2 {
		t.Errorf("post-turn-2 KnownKnowledge len = %d, want 2", len(dedupStore.lastWritten.KnownKnowledge))
	}
}

func TestOrchestrator_PreparerPhase_DedupEnabledWithNilStoreStaysInstrumentationOnly(t *testing.T) {
	// Defensive guard: even with KnowledgeDedup.Enabled=true, the
	// preparer-loop's `o.injectionStateStore != nil` precondition
	// short-circuits when no store was wired. The path must stay on
	// the Phase-2 instrumentation_only branch without panicking.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "x",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body", "content_sha256": "sha-a", "score": 0.9}
		]
	}`
	sink := &recordingEventSink{}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:        sink,
			ContentPreparers: []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
			KnowledgeDedup:   KnowledgeDedupConfig{Enabled: true},
			// InjectionStateStore intentionally nil.
		})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeInstrumentationOnly {
		t.Errorf("enabled-flag + nil-store must stay in instrumentation_only, got %q", pdPayload.Mode)
	}
}

func TestOrchestrator_PreparerPhase_DedupStoreReadFailureStartsFresh(t *testing.T) {
	// RFC #249 robustness: a store-read failure must NOT abort the
	// user turn. The dedup logic falls back to an empty existing
	// state — every candidate appears as "new" — and the event still
	// emits with mode=full.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "x",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body", "content_sha256": "sha-a", "score": 0.9}
		]
	}`
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{failGetErr: errors.New("simulated read failure")}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: dedupStore,
		})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatalf("dedup must not abort run on read failure: %v", err)
	}
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing despite read failure")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeFull {
		t.Errorf("mode = %q, want full (graceful-degradation still emits full)", pdPayload.Mode)
	}
	if len(pdPayload.Knowledge.Injected) != 1 || pdPayload.Knowledge.Injected[0].Reason != events.PreparerDecisionReasonNew {
		t.Errorf("read-failure fallback must treat candidate as new, got %+v", pdPayload.Knowledge.Injected)
	}
}

func TestOrchestrator_PreparerPhase_DedupStoreWriteFailureStillEmitsEvent(t *testing.T) {
	// RFC #249 invariant: "Event emission precedes state writes." A
	// write failure must not lose the event — the next preparer pass's
	// reconciliation step will catch the drift later.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "x",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body", "content_sha256": "sha-a", "score": 0.9}
		]
	}`
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{failUpdateErr: errors.New("simulated write failure")}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: dedupStore,
		})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatalf("dedup must not abort run on write failure: %v", err)
	}
	if dedupStore.updateCalls != 1 {
		t.Errorf("UpdateInjectionState must still be attempted, got %d calls", dedupStore.updateCalls)
	}
	// Event must have emitted before the (failing) write — i.e. it
	// exists despite the write returning an error.
	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing — emit must precede state write")
	}
}

func TestOrchestrator_PreparerPhase_DedupReconciliationEmitsDriftAndCorrectsState(t *testing.T) {
	// Pre-load the dedup store with a SHA that the session's visible
	// messages don't carry (the message was deleted between turns —
	// summarization, retention, or external mutation). The next
	// preparer pass must:
	//   - detect the drift via lazy reconciliation
	//   - emit drift_detected with kb_gone in missing_from_visible
	//   - run applyKnowledgeDedup against the CORRECTED state, so the
	//     new candidate kb_new is treated as "new" (an empty corrected
	//     state means kb_new is unseen).
	preparerJSON := `{
		"send_to_llm": true,
		"message": "user question",
		"knowledge_candidates": [
			{"article_id": "kb_new", "content": "fresh body", "content_sha256": "sha-new", "score": 0.9}
		]
	}`
	sink := &recordingEventSink{}
	dedupStore := &fakeInjectionStateStore{
		store: map[string]state.InjectionState{
			"s1": {KnownKnowledge: []state.KnownKnowledgeEntry{
				{ArticleID: "kb_gone", ContentSHA256: "sha-gone", FirstInjectedTurn: 1},
			}},
		},
	}
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(&fakeLLM{responses: []string{"final"}},
		&fakeParser{parseFn: func(string) []ToolCall { return nil }},
		registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
			EventSink:           sink,
			ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
			KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
			InjectionStateStore: dedupStore,
		})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	// drift_detected must have fired with kb_gone missing and kb_new
	// not yet visible (the preparer-loop runs reconciliation BEFORE
	// the orchestrator splices the new KC block — so the snapshot the
	// reconciler sees has no current-turn KC yet).
	evs := sink.snapshot()
	drift := findEventByType(evs, events.TypeDriftDetected)
	if drift == nil {
		t.Fatal("drift_detected event missing")
	}
	var dp events.DriftDetectedPayload
	if err := json.Unmarshal(drift.Payload, &dp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(dp.MissingFromVisible) != 1 || dp.MissingFromVisible[0] != "sha-gone" {
		t.Errorf("MissingFromVisible = %v, want [sha-gone]", dp.MissingFromVisible)
	}

	// preparer_decision must report kb_new as "new" (corrected state
	// is empty, so the kb_new SHA isn't recognized as known) and
	// kb_gone must NOT appear under SkippedKnown (the reconciliation
	// already dropped it).
	pd := findEventByType(evs, events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeFull {
		t.Errorf("Mode = %q, want full", pdPayload.Mode)
	}
	if len(pdPayload.Knowledge.Injected) != 1 || pdPayload.Knowledge.Injected[0].Reason != events.PreparerDecisionReasonNew {
		t.Errorf("kb_new must inject as new, got %+v", pdPayload.Knowledge.Injected)
	}

	// Persisted state after the turn should only carry kb_new (the
	// reconciliation dropped kb_gone, the dedup added kb_new).
	if len(dedupStore.lastWritten.KnownKnowledge) != 1 || dedupStore.lastWritten.KnownKnowledge[0].ContentSHA256 != "sha-new" {
		t.Errorf("post-turn state must only carry kb_new, got %+v", dedupStore.lastWritten.KnownKnowledge)
	}
}

func TestOrchestrator_PreparerPhase_DedupDisabledStaysInstrumentationOnly(t *testing.T) {
	// Regression guard: even with an InjectionStateStore wired, if the
	// master Enabled flag is false the orchestrator must keep emitting
	// mode=instrumentation_only and must NOT call the store.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "x",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body", "content_sha256": "sha-a", "score": 0.9}
		]
	}`

	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	dedupStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG preparer",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: preparerJSON})
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		KnowledgeDedup:      KnowledgeDedupConfig{Enabled: false}, // explicit off
		InjectionStateStore: dedupStore,
	})

	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pdPayload.Mode != events.PreparerDecisionModeInstrumentationOnly {
		t.Errorf("Mode = %q, want %q", pdPayload.Mode, events.PreparerDecisionModeInstrumentationOnly)
	}
	if dedupStore.getCalls != 0 || dedupStore.updateCalls != 0 {
		t.Errorf("disabled dedup must not touch the store, got get=%d update=%d",
			dedupStore.getCalls, dedupStore.updateCalls)
	}
}

func TestOrchestrator_MessagesTruncated_EmittedWhenSlidingWindowCuts(t *testing.T) {
	// Sliding window: when sess.Messages exceeds ContextMessages, the
	// orchestrator drops the oldest N entries and emits one
	// messages_truncated event carrying the dropped index range and
	// count. With no [knowledge_context] blocks in the dropped slice,
	// ReleasedKnowledgeIDs stays nil and RemainingKnownKnowledgeCount
	// stays 0 — both omitted under `omitempty`. The KC-bearing case is
	// covered by TestOrchestrator_MessagesTruncated_PopulatesReleasedKnowledgeIDs.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	// Seed sess.Messages with 6 user/assistant pairs (12 messages) so the
	// cutter has work to do. ContextMessages = 4 → expect 8 dropped.
	for i := 0; i < 6; i++ {
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "user-msg"})
		_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleAssistant, Content: "asst-msg"})
	}

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:       sink,
		ContextMessages: 4,
	})

	if _, err := orch.Run(context.Background(), "s1", "next message"); err != nil {
		t.Fatal(err)
	}

	mt := findEventByType(sink.snapshot(), events.TypeMessagesTruncated)
	if mt == nil {
		t.Fatal("messages_truncated event not emitted")
	}
	var p events.MessagesTruncatedPayload
	if err := json.Unmarshal(mt.Payload, &p); err != nil {
		t.Fatalf("unmarshal messages_truncated payload: %v", err)
	}
	// 12 seeded + 1 current-turn user message (added before buildMessages
	// runs) = 13; ContextMessages = 4 → drop 9 oldest (indices 0..8).
	if p.DroppedCount != 9 {
		t.Errorf("DroppedCount = %d, want 9 (12 seeded + 1 current = 13, keep 4)", p.DroppedCount)
	}
	if len(p.DroppedSeqRange) != 2 || p.DroppedSeqRange[0] != 0 || p.DroppedSeqRange[1] != 8 {
		t.Errorf("DroppedSeqRange = %v, want [0, 8]", p.DroppedSeqRange)
	}
}

func TestOrchestrator_MessagesTruncated_PopulatesReleasedKnowledgeIDs(t *testing.T) {
	// When the sliding-window cutter drops messages that carry tagged
	// [knowledge_context id="..." sha="..."] blocks, the emitted
	// messages_truncated event lists the released article_ids (deduped,
	// in-order) and the remaining KC-bearing article count among the
	// kept messages. RFC #249 Pillar C — consumers correlate with
	// InjectionState without re-reconciling.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	// Seed: 8 messages. Run() adds 1 current-turn user message before
	// applySlidingWindow fires → 9 total. ContextMessages=4 → drop the
	// first 5 (indices 0..4), keep the last 4 (indices 5..8). Place two
	// distinct + one duplicate article_id in the dropped range; one
	// fresh article_id in the kept range.
	kc := func(id, sha string) string {
		return "[knowledge_context id=\"" + id + "\" sha=\"" + sha + "\"]body[/knowledge_context]\n\nuser text"
	}
	seed := []provider.Message{
		{Role: provider.RoleUser, Content: kc("kb_a", "sha_a1")}, // 0 dropped
		{Role: provider.RoleAssistant, Content: "answer-1"},      // 1 dropped
		{Role: provider.RoleUser, Content: kc("kb_b", "sha_b1")}, // 2 dropped
		{Role: provider.RoleUser, Content: kc("kb_a", "sha_a2")}, // 3 dropped — duplicate article_id
		{Role: provider.RoleAssistant, Content: "answer-b"},      // 4 dropped
		{Role: provider.RoleUser, Content: kc("kb_c", "sha_c1")}, // 5 kept
		{Role: provider.RoleAssistant, Content: "answer-c"},      // 6 kept
		{Role: provider.RoleUser, Content: "plain follow-up"},    // 7 kept
		// index 8 is the "next message" Run() prepends — kept, no KC
	}
	for _, m := range seed {
		_ = sessions.AddMessage("s1", m)
	}

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:       sink,
		ContextMessages: 4,
	})

	if _, err := orch.Run(context.Background(), "s1", "next message"); err != nil {
		t.Fatal(err)
	}

	mt := findEventByType(sink.snapshot(), events.TypeMessagesTruncated)
	if mt == nil {
		t.Fatal("messages_truncated event not emitted")
	}
	var p events.MessagesTruncatedPayload
	if err := json.Unmarshal(mt.Payload, &p); err != nil {
		t.Fatalf("unmarshal messages_truncated payload: %v", err)
	}
	want := []string{"kb_a", "kb_b"}
	if !reflect.DeepEqual(p.ReleasedKnowledgeIDs, want) {
		t.Errorf("ReleasedKnowledgeIDs = %v, want %v (dedup of kb_a + kb_b in dropped range)",
			p.ReleasedKnowledgeIDs, want)
	}
	// Kept slice has one KC-bearing message (kb_c); the current-turn
	// user message added by Run is plain — remaining = 1.
	if p.RemainingKnownKnowledgeCount != 1 {
		t.Errorf("RemainingKnownKnowledgeCount = %d, want 1 (kb_c in kept range)",
			p.RemainingKnownKnowledgeCount)
	}
}

func TestOrchestrator_MessagesTruncated_NotEmittedWhenWithinWindow(t *testing.T) {
	// Sliding window does not fire when len(messages) <= ContextMessages.
	// No messages_truncated event must reach the sink in that case.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")
	// Only one prior exchange — well below the window.
	_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "u"})
	_ = sessions.AddMessage("s1", provider.Message{Role: provider.RoleAssistant, Content: "a"})

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:       sink,
		ContextMessages: 10,
	})
	if _, err := orch.Run(context.Background(), "s1", "next"); err != nil {
		t.Fatal(err)
	}
	if mt := findEventByType(sink.snapshot(), events.TypeMessagesTruncated); mt != nil {
		t.Errorf("messages_truncated must not emit when within window; got payload: %s", mt.Payload)
	}
}

// --- RFC #249 Phase 4 (tool tiers) integration tests ---

// preparerJSONWithTools returns a fake preparer Capabilities body that
// surfaces tool_candidates so the tier decision has something to rank.
// The plugin name "rag-plugin" + action "prepare" is preparer-only;
// the candidates name a SEPARATE registry plugin's actions ("tools-
// plugin.t1" etc.) so they're profile-allowed, non-user-only, and
// non-preparer when the tier decision runs.
const preparerJSONForTierTests = `{
		"send_to_llm": true,
		"message": "user question",
		"tool_candidates": [
			{"tool_name": "tools-plugin__t1", "score": 0.95},
			{"tool_name": "tools-plugin__t2", "score": 0.85},
			{"tool_name": "tools-plugin__t3", "score": 0.75},
			{"tool_name": "tools-plugin__t4", "score": 0.55}
		]
	}`

// registerTierTestPlugins wires a preparer (rag-plugin.prepare) +
// five callable tools (tools-plugin__t1..t5) into the registry. The
// preparer JSON returns ranked candidates that name t1..t4 so a tier
// decision can split them across Tier 1 / Tier 2 / Tier 3.
func registerTierTestPlugins(t *testing.T, registry *ToolRegistry, prepJSON string) {
	t.Helper()
	if err := registry.Register(PluginCapability{
		Name: "rag-plugin", Description: "RAG preparer",
		Actions: []Action{{Name: "prepare", Description: "Prepare"}},
	}, &fixedResultExecutor{content: prepJSON}); err != nil {
		t.Fatalf("register rag-plugin: %v", err)
	}
	if err := registry.Register(PluginCapability{
		Name: "tools-plugin", Description: "Five business tools",
		Actions: []Action{
			{Name: "t1", Description: "Tool one"},
			{Name: "t2", Description: "Tool two"},
			{Name: "t3", Description: "Tool three"},
			{Name: "t4", Description: "Tool four"},
			{Name: "t5", Description: "Tool five"},
		},
	}, &fixedResultExecutor{content: "tool-result"}); err != nil {
		t.Fatalf("register tools-plugin: %v", err)
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersEnabledEmitsTieredBlock(t *testing.T) {
	// RFC #249 Phase 4: with tool_tiers.enabled + a wired
	// InjectionStateStore, the preparer_decision event's Tools block
	// reports Tier 0/1/2/3 splits AND the cap snapshot, instead of the
	// Phase-2 pass-through (all candidates in tier1_new).
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	tierStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, preparerJSONForTierTests)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:           ToolTiersConfig{Enabled: true, Tier1Cap: intPtr(2), Tier2Cap: intPtr(1)},
		InjectionStateStore: tierStore,
	})

	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	if pd == nil {
		t.Fatal("preparer_decision event missing")
	}
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tb := pdPayload.Tools
	// Tier1Cap=2 + Tier2Cap=1 + 4 candidates + 1 non-candidate (t5):
	//   Tier 1 = [t1, t2]; Tier 2 = [t3]; Tier 3 = [t4, t5].
	if tb.Tier1Cap != 2 {
		t.Errorf("Tier1Cap snapshot = %d, want 2", tb.Tier1Cap)
	}
	if tb.Tier1SizeAfter != 2 {
		t.Errorf("Tier1SizeAfter = %d, want 2", tb.Tier1SizeAfter)
	}
	wantT1New := []string{"tools-plugin__t1", "tools-plugin__t2"}
	gotT1New := append([]string(nil), tb.Tier1New...)
	sort.Strings(gotT1New)
	sort.Strings(wantT1New)
	if !reflect.DeepEqual(gotT1New, wantT1New) {
		t.Errorf("Tier1New = %v, want %v", gotT1New, wantT1New)
	}
	if tb.Tier3TotalVisible != 2 {
		t.Errorf("Tier3TotalVisible = %d, want 2 (t4 + t5)", tb.Tier3TotalVisible)
	}
	if tierStore.updateCalls != 1 {
		t.Errorf("UpdateInjectionState calls = %d, want 1", tierStore.updateCalls)
	}
	if len(tierStore.lastWritten.KnownTools) == 0 {
		t.Errorf("KnownTools must be persisted, got empty")
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersDisabledStaysPassThrough(t *testing.T) {
	// Regression guard: with the master switch off (and even when a
	// store is wired), preparer_decision must keep the Phase-2
	// pass-through Tools block — every candidate in tier1_new, no
	// tier1_cap snapshot, no store writes.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	tierStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, preparerJSONForTierTests)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:           ToolTiersConfig{Enabled: false},
		InjectionStateStore: tierStore,
	})

	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	pd := findEventByType(sink.snapshot(), events.TypePreparerDecision)
	var pdPayload events.PreparerDecisionPayload
	if err := json.Unmarshal(pd.Payload, &pdPayload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pdPayload.Tools.Tier1Cap != 0 {
		t.Errorf("Tier1Cap must stay zero when tier logic disabled, got %d", pdPayload.Tools.Tier1Cap)
	}
	if len(pdPayload.Tools.Tier1New) != 4 {
		t.Errorf("pass-through Tier1New len = %d, want 4 (all candidates)", len(pdPayload.Tools.Tier1New))
	}
	if tierStore.updateCalls != 0 {
		t.Errorf("disabled tier_tiers must not write state, got %d update calls", tierStore.updateCalls)
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersSecondTurnLRUCarriesOver(t *testing.T) {
	// Two-turn LRU sanity: turn 1 establishes t1/t2 as Tier 1. Turn 2
	// sees DIFFERENT candidates (t3/t4) and Tier1Cap=2 — but t1/t2's
	// LRURank from turn 1 still falls below currentTurn=2, so t3/t4
	// take Tier 1 and t1/t2 land in Tier1EvictedToTier3.
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final-1", "final-2"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	tierStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, `{
		"send_to_llm": true,
		"message": "q",
		"tool_candidates": [
			{"tool_name": "tools-plugin__t1", "score": 0.9},
			{"tool_name": "tools-plugin__t2", "score": 0.8}
		]
	}`)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:           ToolTiersConfig{Enabled: true, Tier1Cap: intPtr(2), Tier2Cap: intPtr(1)},
		InjectionStateStore: tierStore,
	})

	// Turn 1: t1/t2 enter Tier 1.
	if _, err := orch.Run(context.Background(), "s1", "first"); err != nil {
		t.Fatal(err)
	}

	// Swap the preparer to return t3/t4 instead.
	t1Caps := registry.ListCapabilities()
	for _, cap := range t1Caps {
		if cap.Name == "rag-plugin" {
			_, _ = registry.GetExecutor(cap.Name) // sanity: it's registered
		}
	}
	registry2 := NewToolRegistry()
	registerTierTestPlugins(t, registry2, `{
		"send_to_llm": true,
		"message": "q2",
		"tool_candidates": [
			{"tool_name": "tools-plugin__t3", "score": 0.9},
			{"tool_name": "tools-plugin__t4", "score": 0.8}
		]
	}`)
	// Re-create the orchestrator with the new registry but the SAME
	// store (so turn 2 reads turn 1's KnownTools).
	orch2 := NewWithRules(llm, parser, registry2, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:           ToolTiersConfig{Enabled: true, Tier1Cap: intPtr(2), Tier2Cap: intPtr(1)},
		InjectionStateStore: tierStore,
	})
	if _, err := orch2.Run(context.Background(), "s1", "second"); err != nil {
		t.Fatal(err)
	}

	// Find the LAST preparer_decision (turn 2's).
	evs := sink.snapshot()
	var turn2 *emit.Event
	for i := range evs {
		if evs[i].EventType == events.TypePreparerDecision {
			ev := evs[i]
			turn2 = &ev
		}
	}
	if turn2 == nil {
		t.Fatal("turn 2 preparer_decision missing")
	}
	var p events.PreparerDecisionPayload
	if err := json.Unmarshal(turn2.Payload, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantEvicted := []string{"tools-plugin__t1", "tools-plugin__t2"}
	gotEvicted := append([]string(nil), p.Tools.Tier1EvictedToTier3...)
	sort.Strings(gotEvicted)
	sort.Strings(wantEvicted)
	if !reflect.DeepEqual(gotEvicted, wantEvicted) {
		t.Errorf("turn 2 Tier1EvictedToTier3 = %v, want %v", gotEvicted, wantEvicted)
	}
	gotT1New := append([]string(nil), p.Tools.Tier1New...)
	sort.Strings(gotT1New)
	wantT1New := []string{"tools-plugin__t3", "tools-plugin__t4"}
	if !reflect.DeepEqual(gotT1New, wantT1New) {
		t.Errorf("turn 2 Tier1New = %v, want %v", gotT1New, wantT1New)
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersSystemPromptHasTier2AndTier3Sections(t *testing.T) {
	// RFC #249 Phase 4 D3: with tier decision active, the system
	// prompt sent to the LLM must include "## Available tools —
	// summary tier" (Tier 2 name + 1-liner) and "## Other available
	// tools" (Tier 3 names-only grouped). Tier 0+1 stay in the per-
	// plugin sections (the existing relevant_tools narrowing handles
	// them, now driven by Tier 0+1 names from the decision).
	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	tierStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, preparerJSONForTierTests)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:           ToolTiersConfig{Enabled: true, Tier1Cap: intPtr(2), Tier2Cap: intPtr(1)},
		InjectionStateStore: tierStore,
	})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	if len(llm.requests) == 0 {
		t.Fatal("LLM was not called")
	}
	var sysMsg string
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem {
			sysMsg = m.Content
		}
	}
	if sysMsg == "" {
		t.Fatal("system message missing in LLM request")
	}
	if !strings.Contains(sysMsg, "## Tool catalog — name + one-line summary") {
		t.Errorf("system prompt missing Tier 2 header, got:\n%s", sysMsg)
	}
	if !strings.Contains(sysMsg, "## Other available tools (request details before use)") {
		t.Errorf("system prompt missing Tier 3 header, got:\n%s", sysMsg)
	}
	// Tier 3 includes t4 (overflow of Tier 1+2 caps) and t5 (never a
	// candidate). Both must appear under the tools-plugin group.
	if !strings.Contains(sysMsg, "tools-plugin:") {
		t.Errorf("Tier 3 plugin group header missing, got:\n%s", sysMsg)
	}
	if !strings.Contains(sysMsg, "t4") || !strings.Contains(sysMsg, "t5") {
		t.Errorf("Tier 3 missing t4 / t5, got:\n%s", sysMsg)
	}
	// Tier 2 has exactly one entry (Tier2Cap=1): tools-plugin__t3.
	if !strings.Contains(sysMsg, "tools-plugin__t3") {
		t.Errorf("Tier 2 must list tools-plugin__t3, got:\n%s", sysMsg)
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersDisabledNoTierSectionsInPrompt(t *testing.T) {
	// Regression guard: with tier_tiers.enabled=false, the system
	// prompt must NOT contain the Phase-4 "Available tools — summary
	// tier" or "Other available tools (request details before use)"
	// sections. The pre-Phase-4 prompt structure is fully preserved.
	sink := &recordingEventSink{}
	llm := &capturingLLM{responses: []string{"answer"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, preparerJSONForTierTests)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:        sink,
		ContentPreparers: []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		ToolTiers:        ToolTiersConfig{Enabled: false},
	})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	var sysMsg string
	for _, m := range llm.requests[0].Messages {
		if m.Role == provider.RoleSystem {
			sysMsg = m.Content
		}
	}
	if strings.Contains(sysMsg, "## Tool catalog — name + one-line summary") {
		t.Errorf("Tier 2 section must NOT appear when tier logic off, got:\n%s", sysMsg)
	}
	if strings.Contains(sysMsg, "## Other available tools (request details before use)") {
		t.Errorf("Tier 3 section must NOT appear when tier logic off, got:\n%s", sysMsg)
	}
}

func TestOrchestrator_PreparerPhase_ToolTiersWithDedupSingleStateWrite(t *testing.T) {
	// When BOTH dedup and tier decisions run, the tier preparer merges
	// its KnownTools delta into the dedup decision's UpdatedState so a
	// single UpdateInjectionState call carries both — avoids one
	// clobbering the other and keeps writes to one round-trip.
	preparerJSON := `{
		"send_to_llm": true,
		"message": "q",
		"knowledge_candidates": [
			{"article_id": "kb_a", "content": "body", "content_sha256": "sha-a", "score": 0.9}
		],
		"tool_candidates": [
			{"tool_name": "tools-plugin__t1", "score": 0.95}
		]
	}`
	sink := &recordingEventSink{}
	llm := &fakeLLM{responses: []string{"final"}}
	parser := &fakeParser{parseFn: func(string) []ToolCall { return nil }}
	combinedStore := &fakeInjectionStateStore{}

	registry := NewToolRegistry()
	registerTierTestPlugins(t, registry, preparerJSON)
	memory := state.NewMemoryStore("")
	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "")

	orch := NewWithRules(llm, parser, registry, memory, sessions, OrchestratorOpts{
		EventSink:           sink,
		ContentPreparers:    []ContentPreparerEntry{{Plugin: "rag-plugin", Action: "prepare"}},
		KnowledgeDedup:      KnowledgeDedupConfig{Enabled: true},
		ToolTiers:           ToolTiersConfig{Enabled: true, Tier1Cap: intPtr(3), Tier2Cap: intPtr(2)},
		InjectionStateStore: combinedStore,
	})
	if _, err := orch.Run(context.Background(), "s1", "ask"); err != nil {
		t.Fatal(err)
	}

	if combinedStore.updateCalls != 1 {
		t.Errorf("UpdateInjectionState calls = %d, want 1 (dedup write carries tier delta)", combinedStore.updateCalls)
	}
	if len(combinedStore.lastWritten.KnownKnowledge) != 1 {
		t.Errorf("KnownKnowledge persisted len = %d, want 1", len(combinedStore.lastWritten.KnownKnowledge))
	}
	if len(combinedStore.lastWritten.KnownTools) == 0 {
		t.Errorf("KnownTools must also be persisted in the same write, got 0 entries")
	}
}
