package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

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
		"state":           "doc + diff",
		"choices":         `["match","mismatch"]`,
		"__ot_cb_dry_run": "true", // unknown callback keys pass through and are ignored
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

func TestDecideWithoutProfileIsNotMetered(t *testing.T) {
	d := &fakeDecider{name: "jev", dec: &decideprovider.Decision{Chosen: "a", Confidence: 1}}
	usage := &usageSpy{}
	orch := decideTestOrch(t, d, usage)

	if _, _, err := orch.RunActionResult(context.Background(), "jev", "decide", map[string]string{"state": "s", "choices": `["a"]`}); err != nil {
		t.Fatalf("RunActionResult: %v", err)
	}
	if len(usage.calls) != 0 {
		t.Errorf("usage recorded without a profile: %+v", usage.calls)
	}
}

func TestDecideErrors(t *testing.T) {
	for _, tt := range []struct {
		name    string
		err     error
		choices string
		want    string
	}{
		{name: "backend error is returned", err: errors.New("jev down"), choices: `["a"]`, want: "jev down"},
		{name: "choices must be a JSON array", choices: "a,b", want: "choices must be a JSON array"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := &fakeDecider{name: "jev", err: tt.err, dec: &decideprovider.Decision{Chosen: "a"}}
			usage := &usageSpy{}
			orch := decideTestOrch(t, d, usage)
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
			_, _, err := orch.RunActionResult(ctx, "jev", "decide", map[string]string{"state": "s", "choices": tt.choices})
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
