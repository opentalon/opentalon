package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"

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
	if _, ok := got["gitlab.analyze_code"]; !ok {
		t.Errorf("gitlab.analyze_code missing from AvailableTools; got: %v", got)
	}
	if _, ok := got["jira.create_issue"]; !ok {
		t.Errorf("jira.create_issue missing from AvailableTools; got: %v", got)
	}
	wantDesc := sha256.Sum256([]byte("Analyze code for issues"))
	if got["gitlab.analyze_code"] != hex.EncodeToString(wantDesc[:]) {
		t.Errorf("gitlab.analyze_code desc_sha256 = %s, want %s", got["gitlab.analyze_code"], hex.EncodeToString(wantDesc[:]))
	}
}

func TestOrchestrator_TurnStartFiresOnce_AcrossMultiRoundAgentLoop(t *testing.T) {
	// Three LLM rounds in one Run. turn_start fires once at agent-loop
	// entry, NOT per round; user_message fires once at Run entry. The
	// surrounding model_id/system_prompt/tool catalogue don't change
	// mid-turn, so re-emitting would be noise.
	llm := &fakeLLM{responses: []string{
		"[tool] gitlab.analyze_code",
		"[tool] jira.create_issue",
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
	// representation of "user posted with no text". Phase 2 does not yet
	// capture file metadata; that's intentional out-of-scope.
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
	// have produced). Phase 2 captures *intent* — what the orchestrator
	// asked the provider for — not the resolved model the provider may
	// substitute downstream (that lands in llm_response, Phase 1 territory).
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
// Phase 3 — tool dispatcher instrumentation tests
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
			Name: "gitlab.analyze_code",
			// gitlab.analyze_code declares no Parameters in the test fixture,
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
		t.Errorf("Plugin/Action = %q/%q, want gitlab/analyze_code", got.Plugin, got.Action)
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
				// gitlab.analyze_code declares no Parameters; args would
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
	if p.RequestedName != "no-such-plugin.anything" {
		t.Errorf("RequestedName = %q, want no-such-plugin.anything", p.RequestedName)
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
// Phase 4 — text-parser instrumentation tests
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
	// would bypass the marker-vs-empty-plugin signal that drives Phase 4.
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
		`[tool_call]{"tool": "gitlab.analyze_code", "args": {}}[/tool_call]`,
		"summary after tool call",
	}}
	orch, sessID := setupOrchestratorWithSink(llm, DefaultParser, sink)
	if _, err := orch.Run(context.Background(), sessID, "do it"); err != nil {
		t.Fatal(err)
	}
	if pf := findParseFailedPayloads(t, sink.snapshot()); len(pf) != 0 {
		t.Errorf("tool_call_parse_failed fired on successful parse (%d events); should not", len(pf))
	}
	// Sanity: Phase 3 events still fire for the successful path.
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
	// Phase 4 must catch this — without a closing marker, the response
	// is incoherent and analytics needs to see the failure.
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
	// NoOpSink default path coverage for the Phase 4 emit site.
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
	// Mirrors the NoOpSink default-path coverage from Phase 2 — must also
	// hold for the dispatcher path.
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
