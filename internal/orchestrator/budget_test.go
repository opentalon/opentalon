package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

const (
	budgetDefault = "gpt-oss-120b"
	budgetPremium = "anthropic/claude-x"
)

var budgetSince = time.Date(2026, 11, 1, 0, 0, 0, 0, time.FixedZone("CET", 3600))

type budgetRead struct {
	group, entity string
	kinds         []string
	since         time.Time
}

// budgetStub answers every group sum with group and every entity sum with entity.
type budgetStub struct {
	group, entity float64
	err           error
	mu            sync.Mutex
	reads         []budgetRead
}

func (b *budgetStub) PriceSince(_ context.Context, groupID, entityID string, kinds []string, since time.Time) (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads = append(b.reads, budgetRead{groupID, entityID, kinds, since})
	if b.err != nil {
		return 0, b.err
	}
	if entityID != "" {
		return b.entity, nil
	}
	return b.group, nil
}

// refLLM answers every call and records the model ref each was sent; the
// first call waits for hold to close when hold is set.
type refLLM struct {
	price  float64
	hold   chan struct{}
	inside chan struct{}
	calls  atomic.Int32
	mu     sync.Mutex
	refs   []provider.ModelRef
}

func (l *refLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.mu.Lock()
	l.refs = append(l.refs, req.ModelRef)
	l.mu.Unlock()
	if l.calls.Add(1) == 1 && l.hold != nil {
		close(l.inside)
		select {
		case <-l.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	price := l.price
	return &provider.CompletionResponse{Content: "answer", Usage: provider.Usage{InputTokens: 10, OutputTokens: 5, Price: &price}}, nil
}

func (l *refLLM) sent() []provider.ModelRef {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.refs)
}

func budgetOrch(llm LLMClient, opts OrchestratorOpts) (*Orchestrator, *state.SessionStore) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	sessions.Create(state.SessionParams{ID: "s2"})
	opts.DefaultModel = budgetDefault
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, NewToolRegistry(),
		state.NewMemoryStore(""), sessions, opts)
	return orch, sessions
}

func budgetProfile(model string, tariffs map[string]profile.Tariff, limits ...profile.BudgetLimit) *profile.Profile {
	return &profile.Profile{EntityID: "e1", Group: "g1", Kind: profile.KindChat, Model: model, Tariffs: tariffs, Limits: limits}
}

func TestBudgetDecidesTheTurnsModel(t *testing.T) {
	priced := profile.Tariff{Input: 3, Output: 15}
	free := profile.Tariff{}
	group := profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 50, Since: budgetSince, Kinds: []string{profile.KindChat}}
	entity := profile.BudgetLimit{Scope: profile.ScopeEntity, Max: 10, Since: budgetSince, Kinds: []string{profile.KindChat}}
	cases := []struct {
		name          string
		model         string
		tariffs       map[string]profile.Tariff
		groupUsed     float64
		entityUsed    float64
		wantRef       provider.ModelRef
		wantNotice    string
		wantRefused   string
		wantNoSumRead bool
	}{
		{name: "the account's budget reached", model: budgetPremium, groupUsed: 50,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: free}, wantNotice: profile.ScopeGroup},
		{name: "the user's budget used up", model: budgetPremium, groupUsed: 49, entityUsed: 12,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: free}, wantNotice: profile.ScopeEntity},
		{name: "both used up", model: budgetPremium, groupUsed: 60, entityUsed: 12,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: free}, wantNotice: profile.ScopeGroup},
		{name: "a model without a tariff", model: budgetPremium, groupUsed: 50,
			tariffs: map[string]profile.Tariff{budgetDefault: free}, wantNotice: profile.ScopeGroup},
		{name: "a default model without a tariff is not refused", model: budgetPremium, groupUsed: 50,
			tariffs: map[string]profile.Tariff{"claude-x": priced}, wantNotice: profile.ScopeGroup},
		{name: "no budget used up", model: budgetPremium, groupUsed: 49.99, entityUsed: 9.99,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: free}, wantRef: budgetPremium},
		{name: "a model with tariff 0", model: budgetPremium, groupUsed: 50,
			tariffs: map[string]profile.Tariff{"claude-x": free, budgetDefault: free}, wantRef: budgetPremium, wantNoSumRead: true},
		{name: "on the default anyway", model: "", groupUsed: 50,
			tariffs: map[string]profile.Tariff{"claude-x": priced}, wantNoSumRead: true},
		{name: "the default as the profile's model", model: "ovh/" + budgetDefault, groupUsed: 50,
			tariffs: map[string]profile.Tariff{budgetDefault: free}, wantRef: "ovh/" + budgetDefault, wantNoSumRead: true},
		{name: "the default priced as well", model: budgetPremium, groupUsed: 50,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: {Output: 0.1}}, wantRefused: profile.ScopeGroup},
		{name: "the user's budget, the default priced", model: budgetPremium, entityUsed: 10,
			tariffs: map[string]profile.Tariff{"claude-x": priced, budgetDefault: {Input: 0.1}}, wantRefused: profile.ScopeEntity},
		{name: "on the default anyway, priced", model: "", groupUsed: 50,
			tariffs: map[string]profile.Tariff{budgetDefault: {Input: 0, CachedInput: ptrFloat(0.01)}}, wantRefused: profile.ScopeGroup},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			llm := &refLLM{}
			stub := &budgetStub{group: tc.groupUsed, entity: tc.entityUsed}
			orch, sessions := budgetOrch(llm, OrchestratorOpts{BudgetReader: stub})
			p := budgetProfile(tc.model, tc.tariffs, group, entity)

			res, err := orch.Run(profile.WithProfile(context.Background(), p), "s1", "Hi")

			if tc.wantRefused != "" {
				var refused *BudgetUsedUpError
				if !errors.As(err, &refused) || refused.Scope != tc.wantRefused {
					t.Fatalf("Run = %+v, %v; want a refusal naming %s", res, err, tc.wantRefused)
				}
				if s, _ := sessions.Get("s1"); len(s.Messages) != 0 || len(llm.sent()) != 0 {
					t.Errorf("a refused turn stored %d messages and made %d model calls, want none", len(s.Messages), len(llm.sent()))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := llm.sent(); len(got) != 1 || got[0] != tc.wantRef {
				t.Errorf("model refs sent = %q, want [%q]", got, tc.wantRef)
			}
			if got, ok := res.Metadata[BudgetMetadataKey]; got != tc.wantNotice || ok != (tc.wantNotice != "") {
				t.Errorf("%s = %q (set %t), want %q", BudgetMetadataKey, got, ok, tc.wantNotice)
			}
			if tc.wantNoSumRead && len(stub.reads) != 0 {
				t.Errorf("sums read = %+v, want none", stub.reads)
			}
			if p.Model != tc.model {
				t.Errorf("the shared profile's model = %q, want %q", p.Model, tc.model)
			}
		})
	}
}

func ptrFloat(f float64) *float64 { return &f }

func TestBudgetReadsTheRowsOfItsScope(t *testing.T) {
	stub := &budgetStub{}
	orch, _ := budgetOrch(&refLLM{}, OrchestratorOpts{BudgetReader: stub})
	kinds := []string{profile.KindChat, profile.KindSystem}
	p := budgetProfile(budgetPremium, nil,
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 50, Since: budgetSince, Kinds: kinds},
		profile.BudgetLimit{Scope: profile.ScopeEntity, Max: 10, Since: budgetSince, Kinds: kinds})

	if _, err := orch.Run(profile.WithProfile(context.Background(), p), "s1", "Hi"); err != nil {
		t.Fatal(err)
	}
	want := []budgetRead{{"g1", "", kinds, budgetSince}, {"g1", "e1", kinds, budgetSince}}
	if len(stub.reads) != len(want) {
		t.Fatalf("sums read = %+v, want %+v", stub.reads, want)
	}
	for i, r := range stub.reads {
		if r.group != want[i].group || r.entity != want[i].entity || !slices.Equal(r.kinds, kinds) || !r.since.Equal(budgetSince) {
			t.Errorf("sum read %d = %+v, want %+v", i, r, want[i])
		}
	}
}

func TestBudgetCoversOnlyTheKindsItNames(t *testing.T) {
	stub := &budgetStub{group: 100}
	llm := &refLLM{}
	orch, _ := budgetOrch(llm, OrchestratorOpts{BudgetReader: stub})
	p := budgetProfile(budgetPremium, map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}},
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 50, Since: budgetSince, Kinds: []string{profile.KindChat}})
	p.Kind = profile.KindSystem

	res, err := orch.Run(profile.WithProfile(context.Background(), p), "s1", "Job done")
	if err != nil {
		t.Fatal(err)
	}
	if got := llm.sent(); len(got) != 1 || got[0] != budgetPremium || res.Metadata[BudgetMetadataKey] != "" || len(stub.reads) != 0 {
		t.Errorf("system run: refs %q, notice %q, sums read %+v; want the profile's model, no notice, no sum",
			got, res.Metadata[BudgetMetadataKey], stub.reads)
	}
}

func TestBudgetSumErrorRunsTheTurnOnItsModel(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	llm := &refLLM{}
	orch, _ := budgetOrch(llm, OrchestratorOpts{BudgetReader: &budgetStub{err: errors.New("db down")}})
	p := budgetProfile(budgetPremium, map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}},
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 0, Since: budgetSince, Kinds: []string{profile.KindChat}})

	res, err := orch.Run(profile.WithProfile(context.Background(), p), "s1", "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if got := llm.sent(); len(got) != 1 || got[0] != budgetPremium || res.Metadata[BudgetMetadataKey] != "" {
		t.Errorf("refs %q, notice %q; want the profile's model and no notice", got, res.Metadata[BudgetMetadataKey])
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "db down") {
		t.Errorf("no warning naming the sum error in the log:\n%s", logs.String())
	}
}

// A switched turn leaves the profile it shares with other turns as it is: a
// later turn of another conversation with the same profile, its budget no
// longer used up, runs on the profile's model.
func TestBudgetSwitchLeavesTheSharedProfile(t *testing.T) {
	stub := &budgetStub{group: 50}
	llm := &refLLM{}
	orch, _ := budgetOrch(llm, OrchestratorOpts{BudgetReader: stub})
	p := budgetProfile(budgetPremium, map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}},
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 50, Since: budgetSince, Kinds: []string{profile.KindChat}})
	ctx := profile.WithProfile(context.Background(), p)

	if _, err := orch.Run(ctx, "s1", "Hi"); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	stub.group = 0
	stub.mu.Unlock()
	res, err := orch.Run(ctx, "s2", "Hi")
	if err != nil {
		t.Fatal(err)
	}
	if got := llm.sent(); !slices.Equal(got, []provider.ModelRef{"", budgetPremium}) || res.Metadata[BudgetMetadataKey] != "" {
		t.Errorf("refs %q, second notice %q; want the default chain, then the profile's model without a notice",
			got, res.Metadata[BudgetMetadataKey])
	}
}

// budgetLedger records each run's price and sums it back, as the usage store does.
type budgetLedger struct {
	mu    sync.Mutex
	total float64
}

func (l *budgetLedger) RecordUsage(_ context.Context, _, _, _, _, _, _, _ string, _, _, _ int, _, _ float64, price *float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if price != nil {
		l.total += *price
	}
}

func (l *budgetLedger) PriceSince(context.Context, string, string, []string, time.Time) (float64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, nil
}

// Two messages sent at once to one conversation, the first using up the
// budget: the second turn waits for the turn lock and sees the first turn's
// price, written before the lock is released.
func TestBudgetQueuedTurnSeesThePreviousTurnsPrice(t *testing.T) {
	ledger := &budgetLedger{}
	llm := &refLLM{price: 1, hold: make(chan struct{}), inside: make(chan struct{})}
	orch, _ := budgetOrch(llm, OrchestratorOpts{BudgetReader: ledger, UsageRecorder: ledger, MaxConcurrentSessions: 2})
	p := budgetProfile(budgetPremium, map[string]profile.Tariff{"claude-x": {Input: 3, Output: 15}},
		profile.BudgetLimit{Scope: profile.ScopeGroup, Max: 1, Since: budgetSince, Kinds: []string{profile.KindChat}})
	ctx := profile.WithProfile(context.Background(), p)

	results := make([]*RunResult, 2)
	var wg sync.WaitGroup
	run := func(i int, msg string) {
		defer wg.Done()
		res, err := orch.Run(ctx, "s1", msg)
		if err != nil {
			t.Errorf("turn %d: %v", i, err)
		}
		results[i] = res
	}
	wg.Add(2)
	go run(0, "first")
	<-llm.inside
	go run(1, "second")
	time.Sleep(50 * time.Millisecond) // the second turn is queued on the turn lock
	close(llm.hold)
	wg.Wait()

	if got := llm.sent(); !slices.Equal(got, []provider.ModelRef{budgetPremium, ""}) {
		t.Errorf("refs sent = %q, want the profile's model, then the default chain", got)
	}
	if results[0] == nil || results[1] == nil {
		t.Fatal("a turn returned no result")
	}
	if results[0].Metadata[BudgetMetadataKey] != "" || results[1].Metadata[BudgetMetadataKey] != profile.ScopeGroup {
		t.Errorf("notices = %q, %q; want none, then %q",
			results[0].Metadata[BudgetMetadataKey], results[1].Metadata[BudgetMetadataKey], profile.ScopeGroup)
	}
}
