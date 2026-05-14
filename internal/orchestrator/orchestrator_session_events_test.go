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
