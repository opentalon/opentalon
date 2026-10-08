package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/decideprovider"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
)

type fakeDecider struct {
	name string
	dec  *decideprovider.Decision
	err  error
	got  *decideprovider.Request
}

func (f *fakeDecider) ID() string { return f.name }

func (f *fakeDecider) Decide(_ context.Context, req *decideprovider.Request) (*decideprovider.Decision, error) {
	f.got = req
	return f.dec, f.err
}

type usageCall struct {
	entityID, sessionID, modelID string
	in, out                      int
}

type usageSpy struct {
	mu    sync.Mutex
	calls []usageCall
}

func (u *usageSpy) RecordUsage(_ context.Context, entityID, _, _, sessionID, modelID, _, _ string, in, out, _ int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, usageCall{entityID: entityID, sessionID: sessionID, modelID: modelID, in: in, out: out})
}

func decideTestOrch(t *testing.T, d *fakeDecider, usage UsageRecorder) *Orchestrator {
	t.Helper()
	opts := OrchestratorOpts{UsageRecorder: usage, Deciders: decideprovider.NewRegistry(d)}
	return NewWithRules(&fakeLLM{}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""), opts)
}

// A tln `decide` callback arrives as RunAction(<decider>, "decide", {state,
// choices}); it must reach the decider and return the decision as structured
// JSON, metered against the caller's profile.
func TestDecideCallbackRoutesAndMeters(t *testing.T) {
	d := &fakeDecider{name: "jev-small", dec: &decideprovider.Decision{
		Chosen:        "match",
		Confidence:    0.95,
		Probabilities: map[string]float64{"match": 0.95, "mismatch": 0.05},
		Usage:         decideprovider.Usage{InputTokens: 120, OutputTokens: 1},
	}}
	usage := &usageSpy{}
	orch := decideTestOrch(t, d, usage)

	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
	ctx = actor.WithSessionID(ctx, "sess1")
	_, structured, err := orch.RunActionResult(ctx, "jev-small", "decide", map[string]string{
		"state":   "doc + diff",
		"choices": `["match","mismatch"]`,
	})
	if err != nil {
		t.Fatalf("RunActionResult: %v", err)
	}
	if d.got == nil || d.got.State != "doc + diff" || strings.Join(d.got.Choices, ",") != "match,mismatch" {
		t.Fatalf("decider got %+v", d.got)
	}

	var got decideprovider.Decision
	if err := json.Unmarshal([]byte(structured), &got); err != nil {
		t.Fatalf("decode %q: %v", structured, err)
	}
	if got.Chosen != "match" || got.Confidence != 0.95 {
		t.Errorf("decision = %+v", got)
	}

	want := []usageCall{{entityID: "ent1", sessionID: "sess1", modelID: "decide/jev-small", in: 120, out: 1}}
	if len(usage.calls) != 1 || usage.calls[0] != want[0] {
		t.Errorf("usage = %+v, want %+v", usage.calls, want)
	}
}

// A callback without identity (the plugin omitted the identity args, or the
// external gateway) is still metered — under the unattributed entity — never free.
func TestDecideWithoutProfileIsMeteredUnattributed(t *testing.T) {
	d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "a", Confidence: 1,
		Usage: decideprovider.Usage{InputTokens: 7, OutputTokens: 1}}}
	usage := &usageSpy{}
	orch := decideTestOrch(t, d, usage)

	if _, _, err := orch.RunActionResult(context.Background(), "jev", "decide", map[string]string{"state": "s", "choices": `["a"]`}); err != nil {
		t.Fatalf("RunActionResult: %v", err)
	}
	want := usageCall{entityID: unattributedEntity, modelID: "decide/jev", in: 7, out: 1}
	if len(usage.calls) != 1 || usage.calls[0] != want {
		t.Errorf("usage = %+v, want %+v", usage.calls, want)
	}
}

func TestDecideErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		state   string
		choices string
		nilDec  bool
		want    string
	}{
		{name: "backend error is returned", err: errors.New("jev down"), choices: `["a"]`, want: "jev down"},
		{name: "choices must be a JSON array", choices: "a,b", want: "choices must be a JSON array"},
		{name: "null choices", choices: "null", want: "at least one choice"},
		{name: "empty choices", choices: "[]", want: "at least one choice"},
		{name: "empty label", choices: `["a",""]`, want: "must not be empty"},
		{name: "duplicate label", choices: `["a","a"]`, want: "listed twice"},
		{name: "empty state", state: "  ", choices: `["a"]`, want: "non-empty state"},
		{name: "nil decision is an error, not a panic", nilDec: true, choices: `["a"]`, want: "returned no decision"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDecider{name: "jev", err: tt.err, dec: &decideprovider.Decision{Chosen: "a"}}
			if tt.nilDec {
				d.dec = nil
			}
			state := tt.state
			if state == "" {
				state = "s"
			}
			usage := &usageSpy{}
			orch := decideTestOrch(t, d, usage)
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
			_, _, err := orch.RunActionResult(ctx, "jev", "decide", map[string]string{"state": state, "choices": tt.choices})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if len(usage.calls) != 0 {
				t.Errorf("failed decision was metered: %+v", usage.calls)
			}
		})
	}
}

// decide is a callback-only surface: the model must never call it directly.
func TestDecideRejectsLLMCalls(t *testing.T) {
	d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "a"}}
	exec := &decideExecutor{orch: decideTestOrch(t, d, nil), provider: d}
	res := exec.Execute(context.Background(), ToolCall{ID: "c", Plugin: "jev", Action: "decide", FromLLM: true,
		Args: map[string]string{"state": "s", "choices": `["a"]`}})
	if res.Error == "" || d.got != nil {
		t.Fatalf("LLM-sourced decide was not refused: %+v", res)
	}
}

func TestUnknownDeciderIsNotFound(t *testing.T) {
	orch := decideTestOrch(t, &fakeDecider{name: "jev"}, nil)
	_, _, err := orch.RunActionResult(context.Background(), "laya", "decide", map[string]string{"state": "s", "choices": `["a"]`})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not found", err)
	}
}

// A decider plugin answers only `decide`.
func TestDecideRejectsOtherActions(t *testing.T) {
	d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "a"}}
	exec := &decideExecutor{orch: decideTestOrch(t, d, nil), provider: d}
	res := exec.Execute(context.Background(), ToolCall{ID: "c", Plugin: "jev", Action: "classify",
		Args: map[string]string{"state": "s", "choices": `["a"]`}})
	if !strings.Contains(res.Error, `has no action "classify"`) || d.got != nil {
		t.Fatalf("non-decide action was not refused: %+v", res)
	}
}

type limitStub struct{ used int }

func (l limitStub) TotalTokensSince(context.Context, string, time.Time) (int, error) {
	return l.used, nil
}

// Decide spend is gated, not just metered: a profile at its limit is refused
// before the backend is called.
func TestDecideGatedByTokenLimit(t *testing.T) {
	for _, tt := range []struct {
		name    string
		used    int
		wantErr bool
	}{
		{name: "under the limit", used: 99},
		{name: "at the limit", used: 100, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "a", Confidence: 1}}
			orch := NewWithRules(&fakeLLM{}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
				OrchestratorOpts{Deciders: decideprovider.NewRegistry(d), EscalationLimitChecker: limitStub{used: tt.used}})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1", Limit: 100, LimitWindow: time.Hour})
			_, _, err := orch.RunActionResult(ctx, "jev", "decide", map[string]string{"state": "s", "choices": `["a"]`})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "token limit reached") || d.got != nil {
					t.Fatalf("err = %v, backend called = %v; want a refusal before the backend", err, d.got != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunActionResult: %v", err)
			}
		})
	}
}

// Decider names share the plugin namespace: a clash with a loaded plugin, a
// configured-but-not-yet-loaded one, or a later built-in must fail startup.
func TestCheckDeciderNames(t *testing.T) {
	tools := NewToolRegistry()
	_ = tools.Register(PluginCapability{Name: "github", Actions: []Action{{Name: "list"}}}, &echoExecutor{})

	for _, tt := range []struct {
		decider string
		wantErr bool
	}{
		{decider: "jev"},
		{decider: "github", wantErr: true},    // already registered
		{decider: "weaviate", wantErr: true},  // configured, loads later
		{decider: "scheduler", wantErr: true}, // registered after the orchestrator
	} {
		reg := decideprovider.NewRegistry(&fakeDecider{name: tt.decider})
		err := CheckDeciderNames(reg, tools, "weaviate", "scheduler")
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.decider, err, tt.wantErr)
		}
	}
}

// The planner must not see a plugin whose every action is UserOnly (a decider,
// _notify, _escalate): it can plan nothing with it.
func TestPlannerSkipsUserOnlyPlugins(t *testing.T) {
	got := capabilitiesToPlannerInfo([]PluginCapability{
		{Name: "jev", Actions: []Action{{Name: "decide", UserOnly: true}}},
		{Name: "github", Actions: []Action{{Name: "list"}, {Name: "admin", UserOnly: true}}},
	})
	if len(got) != 1 || got[0].Name != "github" || len(got[0].Actions) != 1 {
		t.Errorf("planner capabilities = %+v, want only github with its one LLM-visible action", got)
	}
}
