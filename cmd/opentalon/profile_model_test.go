package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

type recordingServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newRecordingServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	s := &recordingServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(b))
		s.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *recordingServer) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

type usageRow struct {
	model                 string
	inputCost, outputCost float64
	price                 *float64
}

type usageSpy struct{ rows []usageRow }

func (u *usageSpy) RecordUsage(_ context.Context, _, _, _, _, modelID, _, _ string, _, _, _ int, inCost, outCost float64, price *float64) {
	u.rows = append(u.rows, usageRow{modelID, inCost, outCost, price})
}

func TestProfileModelServedAtItsProvider(t *testing.T) {
	const (
		defaultAnswer = `{"id":"c1","model":"default-model","choices":[{"message":{"role":"assistant","content":"From the default."},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
		claudeAnswer = `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"From Claude."}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":10}}`
	)
	const adaptive = `{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`
	cases := []struct {
		name         string
		model        string
		effort       string
		anthStatus   int
		stream       bool
		claude       bool
		wantThinking string
		wantModel    string
		wantWarn     string
	}{
		{name: "selected model", model: "anthropic/claude-x", claude: true, wantThinking: adaptive + ` {"effort":"medium"}`},
		{name: "the turn's effort wins", model: "anthropic/claude-x", effort: "low", claude: true, wantThinking: adaptive + ` {"effort":"low"}`},
		{name: "no thinking budget from the default's reasoning flag", model: "anthropic/claude-plain", claude: true},
		{name: "a streaming round", model: "anthropic/claude-x", stream: true, claude: true, wantThinking: adaptive + ` {"effort":"medium"}`},
		{name: "unknown provider", model: "nope/claude-x", wantWarn: "nope/claude-x"},
		{name: "unknown model", model: "anthropic/claude-nope", wantWarn: "anthropic/claude-nope"},
		{name: "unknown bare id", model: "unknown-model", wantWarn: "unknown-model"},
		{name: "no provider part", model: "/missing", wantWarn: "/missing"},
		{name: "no model part", model: "anthropic/", wantWarn: "anthropic/"},
		{name: "known bare id", model: "other-model", wantModel: "other-model"},
		{name: "no profile model", model: ""},
		{name: "selected provider refuses", model: "anthropic/claude-x", anthStatus: http.StatusUnauthorized, wantWarn: "claude-x"},
	}
	for _, setup := range []struct{ name, fallbacks string }{
		{"with fallbacks", "[backup/backup-model]"},
		{"single provider without fallbacks", "[]"},
	} {
		for _, tc := range cases {
			t.Run(setup.name+"/"+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				prev := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(prev)

				status := tc.anthStatus
				if status == 0 {
					status = http.StatusOK
				}
				def := newRecordingServer(t, http.StatusOK, defaultAnswer)
				backup := newRecordingServer(t, http.StatusOK, defaultAnswer)
				anth := newRecordingServer(t, status, claudeAnswer)
				cfg, err := config.Parse([]byte(`
models:
  providers:
    primary:
      api: openai-completions
      base_url: ` + def.URL + `
      models:
        - id: default-model
          features: [reasoning]
        - id: other-model
    backup:
      api: openai-completions
      base_url: ` + backup.URL + `
      models:
        - id: backup-model
    anthropic:
      api: anthropic-messages
      base_url: ` + anth.URL + `
      api_key: "${OPENTALON_TEST_PROFILE_MODEL_UNSET_KEY}"
      prompt_cache: true
      models:
        - id: claude-x
          thinking: adaptive
          reasoning_effort: medium
          cost: {input: 2.00, output: 10.00, cache_write: 2.50, cache_read: 0.20}
        - id: claude-plain
routing:
  primary: primary/default-model
  fallbacks: ` + setup.fallbacks + `
`))
				if err != nil {
					t.Fatal(err)
				}
				prov, defaultModel, err := buildProvider(cfg, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				sessions := state.NewSessionStore("")
				sessions.Create(state.SessionParams{ID: "s1"})
				usage := &usageSpy{}
				opts := orchestrator.OrchestratorOpts{UsageRecorder: usage}
				if tc.stream {
					opts.OnStreamChunk = func(context.Context, string, bool) {}
				}
				orch := orchestrator.NewWithRules(&defaultModelClient{provider: prov, model: defaultModel},
					orchestrator.DefaultParser, orchestrator.NewToolRegistry(), state.NewMemoryStore(""), sessions, opts)
				ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: tc.model})
				if tc.effort != "" {
					ctx = actor.WithReasoningEffort(ctx, tc.effort)
				}

				res, err := orch.Run(ctx, "s1", "Hi")
				if err != nil {
					t.Fatal(err)
				}

				wantAnth := 0
				if tc.claude || tc.anthStatus != 0 {
					wantAnth = 1
				}
				if got := len(anth.received()); got != wantAnth {
					t.Fatalf("Anthropic received %d requests, want %d", got, wantAnth)
				}
				if tc.claude {
					var sent struct {
						Model        string          `json:"model"`
						Thinking     json.RawMessage `json:"thinking"`
						OutputConfig json.RawMessage `json:"output_config"`
					}
					if err := json.Unmarshal([]byte(anth.received()[0]), &sent); err != nil {
						t.Fatal(err)
					}
					if want := strings.TrimPrefix(tc.model, "anthropic/"); sent.Model != want {
						t.Errorf("Anthropic was sent model %q, want %q", sent.Model, want)
					}
					if got := strings.TrimSpace(string(sent.Thinking) + " " + string(sent.OutputConfig)); got != tc.wantThinking {
						t.Errorf("thinking and output_config = %q, want %q", got, tc.wantThinking)
					}
					if len(def.received())+len(backup.received()) != 0 {
						t.Errorf("the default chain received %d requests, want none", len(def.received())+len(backup.received()))
					}
					if res.Response != "From Claude." {
						t.Errorf("response = %q, want Claude's answer", res.Response)
					}
					if tc.model == "anthropic/claude-x" {
						want := usageRow{"claude-x", (100*2.00 + 1000*0.20) / 1e6, 10 * 10.00 / 1e6, nil}
						if len(usage.rows) != 1 || usage.rows[0].model != want.model ||
							math.Abs(usage.rows[0].inputCost-want.inputCost) > 1e-12 ||
							math.Abs(usage.rows[0].outputCost-want.outputCost) > 1e-12 {
							t.Errorf("usage rows = %+v, want [%+v]", usage.rows, want)
						}
					}
				} else {
					got := def.received()
					if len(got) != 1 {
						t.Fatalf("the default model received %d requests, want 1", len(got))
					}
					var sent struct {
						Model string `json:"model"`
					}
					if err := json.Unmarshal([]byte(got[0]), &sent); err != nil {
						t.Fatal(err)
					}
					want := tc.wantModel
					if want == "" {
						want = "default-model"
					}
					if sent.Model != want || res.Response != "From the default." {
						t.Errorf("default chain was sent %q and answered %q, want %s and its answer", sent.Model, res.Response, want)
					}
				}
				if tc.wantWarn != "" && !warnedAbout(logs.String(), tc.wantWarn) {
					t.Errorf("no warning naming %q in the log:\n%s", tc.wantWarn, logs.String())
				}
			})
		}
	}
}

func warnedAbout(logs, s string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, s) {
			return true
		}
	}
	return false
}

type eventRecorder struct {
	mu  sync.Mutex
	evs []emit.Event
}

func (r *eventRecorder) Emit(_ context.Context, e emit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *eventRecorder) modelIDs(eventType string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, e := range r.evs {
		if e.EventType != eventType {
			continue
		}
		var p struct {
			ModelID string `json:"model_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			panic(err)
		}
		ids = append(ids, p.ModelID)
	}
	return ids
}

func sentModels(bodies []string) []string {
	models := make([]string, 0, len(bodies))
	for _, b := range bodies {
		var sent struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(b), &sent); err != nil {
			panic(err)
		}
		models = append(models, sent.Model)
	}
	return models
}

// The session title, written in the background after the turn, is served
// like the turn: at the profile's provider, by the default chain without a
// profile model or when that provider refuses, and its events name the model
// that served it.
func TestSessionTitleServedLikeTheTurn(t *testing.T) {
	const (
		defaultAnswer = `{"id":"c1","model":"default-model","choices":[{"message":{"role":"assistant","content":"From the default"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
		claudeAnswer = `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"From Claude"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":100,"output_tokens":10}}`
	)
	cases := []struct {
		name         string
		model        string
		anthStatus   int
		wantAnth     []string
		wantDefault  []string
		wantRequests []string
		wantTitle    string
	}{
		{name: "the profile's provider serves", model: "anthropic/claude-x", anthStatus: http.StatusOK,
			wantAnth: []string{"claude-x", "claude-x"}, wantRequests: []string{"claude-x", "claude-x"}, wantTitle: "claude-x"},
		{name: "the profile's provider refuses the key and is marked", model: "anthropic/claude-x", anthStatus: http.StatusUnauthorized,
			wantAnth: []string{"claude-x"}, wantDefault: []string{"default-model", "default-model"},
			wantRequests: []string{"claude-x", "default-model", "default-model"}, wantTitle: "default-model"},
		{name: "no profile model", anthStatus: http.StatusOK,
			wantDefault: []string{"default-model", "default-model"}, wantRequests: []string{"default-model", "default-model"}, wantTitle: "default-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captureLogs(t)
			def := newRecordingServer(t, http.StatusOK, defaultAnswer)
			anth := newRecordingServer(t, tc.anthStatus, claudeAnswer)
			cfg, err := config.Parse([]byte(`
models:
  providers:
    primary:
      api: openai-completions
      base_url: ` + def.URL + `
      models:
        - id: default-model
    anthropic:
      api: anthropic-messages
      base_url: ` + anth.URL + `
      api_key: "${OPENTALON_TEST_PROFILE_MODEL_UNSET_KEY}"
      models:
        - id: claude-x
routing:
  primary: primary/default-model
`))
			if err != nil {
				t.Fatal(err)
			}
			sink := &eventRecorder{}
			prov, defaultModel, err := buildProvider(cfg, nil, nil, sink)
			if err != nil {
				t.Fatal(err)
			}
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			orch := orchestrator.NewWithRules(&defaultModelClient{provider: prov, model: defaultModel},
				orchestrator.DefaultParser, orchestrator.NewToolRegistry(), state.NewMemoryStore(""), sessions,
				orchestrator.OrchestratorOpts{EventSink: sink, SessionTitlesEnabled: true})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: tc.model})

			if _, err := orch.Run(ctx, "s1", "Hi"); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for len(sink.modelIDs(events.TypeSessionTitleGenerated)) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}

			if got := sentModels(anth.received()); !slices.Equal(got, tc.wantAnth) {
				t.Errorf("Anthropic was sent %q, want %q", got, tc.wantAnth)
			}
			if got := sentModels(def.received()); !slices.Equal(got, tc.wantDefault) {
				t.Errorf("the default provider was sent %q, want %q", got, tc.wantDefault)
			}
			if got := sink.modelIDs(events.TypeLLMRequest); !slices.Equal(got, tc.wantRequests) {
				t.Errorf("llm_request events name %q, want %q", got, tc.wantRequests)
			}
			if got := sink.modelIDs(events.TypeSessionTitleGenerated); !slices.Equal(got, []string{tc.wantTitle}) {
				t.Errorf("session_title_generated names %q, want %q", got, tc.wantTitle)
			}
		})
	}
}
