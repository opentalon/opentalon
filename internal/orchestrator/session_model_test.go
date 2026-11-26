package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

const (
	callChat     = "chat"
	callPlanner  = "planner"
	callQuestion = "confirmation question"
	callClassify = "classifier"
	callTitle    = "title"
)

func callKind(req *provider.CompletionRequest) string {
	if req.Segments != nil {
		return callChat
	}
	first := req.Messages[0]
	switch {
	case len(req.Messages) == 1 && first.Role == provider.RoleUser:
		return callClassify
	case strings.HasPrefix(first.Content, prompts.PlannerPreamble):
		return callPlanner
	case strings.HasPrefix(first.Content, confirmationNarratePrompt):
		return callQuestion
	case first.Content == prompts.SessionTitle:
		return callTitle
	}
	return "unknown"
}

type modelAsked struct {
	ref   provider.ModelRef
	model string
}

// helperScript answers each kind of model call of a confirmation turn and
// records the model every call asked for.
type helperScript struct {
	mu    sync.Mutex
	chats int
	asked map[string][]modelAsked
}

func (s *helperScript) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind := callKind(req)
	if s.asked == nil {
		s.asked = map[string][]modelAsked{}
	}
	s.asked[kind] = append(s.asked[kind], modelAsked{req.ModelRef, req.Model})
	answer := map[string]string{
		callPlanner:  `{"type":"direct"}`,
		callQuestion: "Shall I analyze myrepo?",
		callClassify: `{"decision":"amend","requests_change":false,"reason":"asks which branch"}`,
		callTitle:    "Code analysis",
	}[kind]
	if kind == callChat {
		s.chats++
		answer = "Done."
		if s.chats == 1 {
			answer = "CALL"
		}
	}
	return &provider.CompletionResponse{Content: answer, Model: req.Model}, nil
}

func (s *helperScript) seen(kind string) []modelAsked {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]modelAsked(nil), s.asked[kind]...)
}

func confirmationTurnsOrch(llm LLMClient, opts OrchestratorOpts) *Orchestrator {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{Name: "gitlab", Actions: []Action{{Name: "analyze_code"}}}, &echoExecutor{})
	_ = registry.Register(PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{})
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	opts.PipelineEnabled = true
	opts.ConfirmationClassifierEnabled = true
	opts.ConfirmationPlugin, opts.ConfirmationAction = "conf", "check"
	parser := &fakeParser{parseFn: func(s string) []ToolCall {
		if s == "CALL" {
			return []ToolCall{{ID: "c1", Plugin: "gitlab", Action: "analyze_code"}}
		}
		return nil
	}}
	return NewWithRules(llm, parser, registry, state.NewMemoryStore(""), sessions, opts)
}

func runConfirmationTurns(t *testing.T, orch *Orchestrator, ctx context.Context) {
	t.Helper()
	asked, err := orch.Run(ctx, "s1", "analyze myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if asked.Metadata["type"] != "confirmation" {
		t.Fatalf("first turn = %q %+v, want a confirmation", asked.Response, asked.Metadata)
	}
	answered, err := orch.Run(ctx, "s1", "which branch would you analyze?")
	if err != nil {
		t.Fatal(err)
	}
	if answered.Response != "Done." {
		t.Fatalf("reply answered %q, want the main model's answer", answered.Response)
	}
}

// The calls a turn makes besides the chat loop ask for the model the chat
// loop asks for: the profile's, or none when there is none or a used-up
// budget moved the turn to the default model.
func TestATurnsHelperCallsAskForTheChatLoopsModel(t *testing.T) {
	usedUp := budgetProfile(budgetPremium, map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}, budgetDefault: {}},
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 50, Since: budgetSince, Kinds: []string{profile.KindChat}})
	cases := []struct {
		name    string
		profile *profile.Profile
		budget  BudgetReader
		want    provider.ModelRef
	}{
		{name: "the profile's model", profile: &profile.Profile{EntityID: "e1", Model: budgetPremium}, want: budgetPremium},
		{name: "a profile without a model", profile: &profile.Profile{EntityID: "e1"}},
		{name: "no profile"},
		{name: "a used-up budget", profile: usedUp, budget: &budgetStub{group: 50}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := &helperScript{}
			orch := confirmationTurnsOrch(script, OrchestratorOpts{BudgetReader: tc.budget, DefaultModel: budgetDefault})
			ctx := context.Background()
			if tc.profile != nil {
				ctx = profile.WithProfile(ctx, tc.profile)
			}

			runConfirmationTurns(t, orch, ctx)

			want := modelAsked{tc.want, tc.want.Model()}
			for _, kind := range []string{callChat, callPlanner, callQuestion, callClassify} {
				got := script.seen(kind)
				if len(got) == 0 {
					t.Errorf("no %s call", kind)
				}
				for _, g := range got {
					if g != want {
						t.Errorf("%s asked for %+v, want %+v", kind, g, want)
					}
				}
			}
		})
	}
}

// The session title, written after the turn, asks for the model the turn
// asked for.
func TestTitleAsksForTheTurnsModel(t *testing.T) {
	for kind, opts := range map[string]OrchestratorOpts{
		callTitle: {SessionTitlesEnabled: true},
	} {
		for _, want := range []provider.ModelRef{budgetPremium, ""} {
			t.Run(kind+"/"+string(want), func(t *testing.T) {
				script := &helperScript{}
				sessions := state.NewSessionStore("")
				sessions.Create(state.SessionParams{ID: "s1"})
				orch := NewWithRules(script, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, opts)
				ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: string(want)})

				if _, err := orch.Run(ctx, "s1", "Hi"); err != nil {
					t.Fatal(err)
				}

				deadline := time.Now().Add(5 * time.Second)
				for len(script.seen(kind)) == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if got := script.seen(kind); len(got) != 1 || got[0] != (modelAsked{want, want.Model()}) {
					t.Errorf("%s calls asked for %+v, want one for %q", kind, got, want)
				}
			})
		}
	}
}

// A corrector model the operator configured is kept; without one the repair
// call asks for the turn's model.
func TestRepairAsksForItsOwnModelElseTheTurnsModel(t *testing.T) {
	for _, tc := range []struct {
		name, corrector string
		want            modelAsked
	}{
		{"no corrector model", "", modelAsked{budgetPremium, "claude-x"}},
		{"a corrector model", "corrector-model", modelAsked{"", "corrector-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			llm := &stepLLM{steps: []stepAnswer{
				{content: "[tool_call] update the item"},
				{content: `{"repaired_args": {"item_id": "42", "responsible_user_id": "131349"}}`},
				{content: "Done."},
			}}
			parser := misnamedArgParser(map[string]string{"item_id": "42", "user_id": "131349"})
			orch, sessID := setupRepairOrchestrator(llm, parser, &recordingEventSink{}, &recordingExecutor{}, OrchestratorOpts{
				Repair: RepairConfig{Enabled: true, Model: tc.corrector},
			})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: budgetPremium})

			if _, err := orch.Run(ctx, sessID, "set user 131349 on item 42"); err != nil {
				t.Fatal(err)
			}

			if len(llm.requests) != 3 {
				t.Fatalf("model calls = %d, want the chat round, the corrector and the chat round", len(llm.requests))
			}
			if got := (modelAsked{llm.requests[1].ModelRef, llm.requests[1].Model}); got != tc.want {
				t.Errorf("the corrector asked for %+v, want %+v", got, tc.want)
			}
		})
	}
}

// scriptedProvider is a provider of the selecting provider that answers from
// a shared script, or fails every call with err.
type scriptedProvider struct {
	id     string
	model  string
	err    error
	script *helperScript
	mu     sync.Mutex
	sent   map[string][]string
}

func (p *scriptedProvider) ID() string { return p.id }

func (p *scriptedProvider) Models() []provider.ModelInfo {
	return []provider.ModelInfo{{ID: p.model, ProviderID: p.id}}
}

func (p *scriptedProvider) SupportsFeature(provider.Feature) bool { return false }

func (p *scriptedProvider) Stream(context.Context, *provider.CompletionRequest) (provider.ResponseStream, error) {
	return nil, provider.ErrStreamingNotImplemented
}

func (p *scriptedProvider) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	p.mu.Lock()
	if p.sent == nil {
		p.sent = map[string][]string{}
	}
	p.sent[callKind(req)] = append(p.sent[callKind(req)], req.Model)
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return p.script.Complete(ctx, req)
}

func (p *scriptedProvider) received(kind string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.sent[kind]...)
}

// Through the selecting provider, the confirmation question and the
// classifier are served by the profile's model, and by the default model when
// that one fails, as the chat loop is.
func TestConfirmationCallsAreServedLikeTheChatLoop(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		fallback bool
	}{
		{"the selected model serves", nil, false},
		{"the selected provider refuses the key", &provider.StatusError{StatusCode: http.StatusUnauthorized, Body: "invalid x-api-key"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := &helperScript{}
			def := &scriptedProvider{id: "primary", model: "default-model", script: script}
			sel := &scriptedProvider{id: "anthropic", model: "claude-x", script: script, err: tc.err}
			llm := provider.NewSelectingProvider(context.Background(), def, "default-model",
				map[string]provider.SelectableProvider{"anthropic": {Prov: sel}}, provider.HealthGateConfig{},
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			orch := confirmationTurnsOrch(llm, OrchestratorOpts{})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: budgetPremium})

			runConfirmationTurns(t, orch, ctx)

			for _, kind := range []string{callChat, callQuestion, callClassify} {
				tried, served := sel.received(kind), def.received(kind)
				if len(tried) == 0 || tried[0] != "claude-x" {
					t.Errorf("%s: the selected provider was sent %q, want claude-x", kind, tried)
				}
				if tc.fallback != (len(served) > 0) {
					t.Errorf("%s: the default provider was sent %q, want it only after a failure", kind, served)
				}
				for _, m := range served {
					if m != "default-model" {
						t.Errorf("%s: the default provider was sent %q, want default-model", kind, m)
					}
				}
			}
		})
	}
}
