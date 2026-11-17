package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

const (
	fromDefault     = "From the default."
	fromClaude      = "From Claude."
	defaultTextBody = `{"id":"c1","model":"default-model","choices":[{"message":{"role":"assistant","content":"From the default."},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	defaultToolCallBody = `{"id":"c2","model":"default-model","choices":[{"message":{"role":"assistant","content":"",` +
		`"tool_calls":[{"id":"call_2","type":"function","function":{"name":"inv__list","arguments":"{\"page\":\"2\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
	claudeTextBody = `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"From Claude."}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":10,"output_tokens":2}}`
	claudeToolUseBody = `{"id":"msg_2","model":"claude-x","content":[{"type":"thinking","thinking":"I should list them.","signature":"sig-1"},` +
		`{"type":"tool_use","id":"toolu_1","name":"inv__list","input":{}}],"stop_reason":"tool_use",` +
		`"usage":{"input_tokens":10,"output_tokens":2}}`
	anthAuthError      = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`
	anthSpendLimit     = `{"type":"error","error":{"type":"invalid_request_error","message":"You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC."}}`
	anthCreditTooLow   = `{"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits."}}`
	anthOverloaded     = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	anthInvalidRequest = `{"type":"error","error":{"type":"invalid_request_error","message":"messages.3.content.0: unexpected tool_use_id found in tool_result blocks"}}`
)

type fakeAnswer struct {
	status int
	body   string
}

type fakeLLMServer struct {
	*httptest.Server
	mu          sync.Mutex
	answers     []fakeAnswer
	hold        bool
	held        chan struct{}
	probeStatus int
	calls       []string
	probes      []probeSeen
}

type probeSeen struct {
	path   string
	header http.Header
	status int
}

func newFakeLLMServer(t *testing.T, answers ...fakeAnswer) *fakeLLMServer {
	t.Helper()
	f := &fakeLLMServer{answers: answers, held: make(chan struct{}, 8), probeStatus: http.StatusOK}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLLMServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	if r.Method == http.MethodGet {
		status := f.probeStatus
		f.probes = append(f.probes, probeSeen{r.URL.Path, r.Header.Clone(), status})
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"data":[]}`)
		return
	}
	f.calls = append(f.calls, string(body))
	a := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	hold := f.hold
	f.mu.Unlock()
	if hold {
		f.held <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		a = fakeAnswer{http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"held for 5s"}}`}
	}
	w.WriteHeader(a.status)
	_, _ = io.WriteString(w, a.body)
}

func (f *fakeLLMServer) set(a fakeAnswer, probeStatus int, hold bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers, f.probeStatus, f.hold = []fakeAnswer{a}, probeStatus, hold
}

func (f *fakeLLMServer) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLLMServer) probesSeen() []probeSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]probeSeen(nil), f.probes...)
}

var routingSetups = []struct{ name, fallbacks string }{
	{"with fallbacks", "[backup/backup-model]"},
	{"single provider without fallbacks", "[]"},
}

func selectedProviderConfig(t *testing.T, defURL, backupURL, anthURL, fallbacks, health string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(`
models:
  providers:
    primary:
      api: openai-completions
      base_url: ` + defURL + `
      models:
        - id: default-model
          features: [tools, reasoning]
    backup:
      api: openai-completions
      base_url: ` + backupURL + `
      models:
        - id: backup-model
          features: [tools]
    anthropic:
      api: anthropic-messages
      base_url: ` + anthURL + `
      api_key: sk-ant-test
      timeout: 500ms
      retry:
        max_attempts: 1
      models:
        - id: claude-x
          features: [tools]
          thinking: adaptive
          reasoning_effort: medium
routing:
  primary: primary/default-model
  fallbacks: ` + fallbacks + `
  health: ` + health + `
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newSelectedProviderOrch(t *testing.T, cfg *config.Config, registry *orchestrator.ToolRegistry) (*orchestrator.Orchestrator, *state.SessionStore) {
	t.Helper()
	prov, defaultModel, err := buildProvider(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil {
		registry = orchestrator.NewToolRegistry()
	}
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	orch := orchestrator.NewWithRules(&defaultModelClient{provider: prov, model: defaultModel},
		orchestrator.DefaultParser, registry, state.NewMemoryStore(""), sessions, orchestrator.OrchestratorOpts{})
	return orch, sessions
}

func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return logs
}

// syncBuffer is a bytes.Buffer that probe goroutines may log to while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func warnings(logs, s string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, s) {
			out = append(out, line)
		}
	}
	return out
}

func selectedProfile(ctx context.Context, model string) context.Context {
	return profile.WithProfile(ctx, &profile.Profile{EntityID: "e1", Model: model})
}

func TestSelectedProviderFailure(t *testing.T) {
	cases := []struct {
		name        string
		answer      fakeAnswer
		unreachable bool
		hold        bool
		failure     string
		orFailure   string // a second wording the warning may carry instead
		marks       bool
	}{
		{name: "the key is refused", answer: fakeAnswer{401, anthAuthError}, failure: "authentication_error", marks: true},
		{name: "the spend limit is reached", answer: fakeAnswer{400, anthSpendLimit},
			failure: "You have reached your specified API usage limits", marks: true},
		{name: "the credit balance is too low", answer: fakeAnswer{400, anthCreditTooLow},
			failure: "Your credit balance is too low", marks: true},
		{name: "overloaded", answer: fakeAnswer{529, anthOverloaded}, failure: "overloaded_error", marks: true},
		{name: "cannot be reached", unreachable: true, failure: "connection refused", marks: true},
		// net/http does not always add its "(Client.Timeout exceeded ...)"
		// suffix to a client timeout; without it the context error is left.
		{name: "reaches its time limit", answer: fakeAnswer{200, claudeTextBody}, hold: true,
			failure: "Client.Timeout exceeded", orFailure: "context deadline exceeded", marks: true},
		{name: "rejects the request as invalid", answer: fakeAnswer{400, anthInvalidRequest},
			failure: "unexpected tool_use_id", marks: false},
	}
	for _, setup := range routingSetups {
		for _, tc := range cases {
			t.Run(setup.name+"/"+tc.name, func(t *testing.T) {
				logs := captureLogs(t)
				def := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
				backup := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
				anth := newFakeLLMServer(t, tc.answer)
				anth.set(tc.answer, http.StatusOK, tc.hold)
				anthURL := anth.URL
				if tc.unreachable {
					anth.Close()
				}
				orch, _ := newSelectedProviderOrch(t, selectedProviderConfig(t, def.URL, backup.URL, anthURL, setup.fallbacks, "{}"), nil)
				ctx := selectedProfile(context.Background(), "anthropic/claude-x")

				for turn := 1; turn <= 2; turn++ {
					res, err := orch.Run(ctx, "s1", "Hi")
					if err != nil {
						t.Fatalf("turn %d: %v", turn, err)
					}
					if res.Response != fromDefault {
						t.Errorf("turn %d answered %q, want the default chain's answer", turn, res.Response)
					}
				}

				wantCalls := 2
				if tc.marks {
					wantCalls = 1
				}
				if got := len(anth.received()); !tc.unreachable && got != wantCalls {
					t.Errorf("Anthropic received %d calls, want %d", got, wantCalls)
				}
				warned := warnings(logs.String(), "model=claude-x")
				if len(warned) != wantCalls {
					t.Errorf("%d warnings name claude-x, want one per failed call (%d):\n%s", len(warned), wantCalls, logs.String())
				}
				for _, w := range warned {
					named := strings.Contains(w, tc.failure) || (tc.orFailure != "" && strings.Contains(w, tc.orFailure))
					if !strings.Contains(w, "provider=anthropic") || !named {
						t.Errorf("warning does not name provider anthropic and %q:\n%s", tc.failure, w)
					}
				}
				if w := warnings(logs.String(), "not served"); len(w) != 0 {
					t.Errorf("the default chain was sent another model's id:\n%s", strings.Join(w, "\n"))
				}
				for _, body := range def.received() {
					if model := sentModel(t, body); model != "default-model" {
						t.Errorf("the default model was sent model %q, want default-model", model)
					}
				}
				if got := len(def.received()); got != 2 {
					t.Errorf("the default model received %d calls, want 2", got)
				}
			})
		}
	}
}

func TestSelectedProviderReturnsAfterHealthyProbes(t *testing.T) {
	for _, setup := range routingSetups {
		t.Run(setup.name, func(t *testing.T) {
			captureLogs(t)
			def := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
			backup := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
			anth := newFakeLLMServer(t)
			anth.set(fakeAnswer{529, anthOverloaded}, http.StatusServiceUnavailable, false)
			cfg := selectedProviderConfig(t, def.URL, backup.URL, anth.URL, setup.fallbacks, "{interval: 20ms, recover_after: 2}")
			orch, _ := newSelectedProviderOrch(t, cfg, nil)
			ctx := selectedProfile(context.Background(), "anthropic/claude-x")
			turn := func(want string) {
				t.Helper()
				res, err := orch.Run(ctx, "s1", "Hi")
				if err != nil {
					t.Fatal(err)
				}
				if res.Response != want {
					t.Fatalf("answered %q, want %q", res.Response, want)
				}
			}

			turn(fromDefault)
			turn(fromDefault)
			if got := len(anth.received()); got != 1 {
				t.Fatalf("Anthropic received %d calls, want 1: the second turn must skip it while it is unhealthy", got)
			}

			anth.set(fakeAnswer{200, claudeTextBody}, http.StatusOK, false)
			// The probes run one after another, so a third healthy probe
			// arrives only after the second was counted.
			deadline := time.Now().Add(5 * time.Second)
			for healthyProbes(anth.probesSeen()) < 3 {
				if time.Now().After(deadline) {
					t.Fatalf("no three healthy probes within 5s; probes seen: %+v", anth.probesSeen())
				}
				time.Sleep(5 * time.Millisecond)
			}
			turn(fromClaude)

			for _, p := range anth.probesSeen() {
				if p.path != "/v1/models" || p.header.Get("x-api-key") != "sk-ant-test" || p.header.Get("anthropic-version") == "" {
					t.Errorf("probe on %s with x-api-key %q and anthropic-version %q, want /v1/models with the provider's key",
						p.path, p.header.Get("x-api-key"), p.header.Get("anthropic-version"))
				}
			}
		})
	}
}

func healthyProbes(probes []probeSeen) int {
	n := 0
	for _, p := range probes {
		if p.status == http.StatusOK {
			n++
		}
	}
	return n
}

func TestCancelledTurnMarksNothing(t *testing.T) {
	for _, setup := range routingSetups {
		t.Run(setup.name, func(t *testing.T) {
			logs := captureLogs(t)
			def := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
			backup := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
			anth := newFakeLLMServer(t)
			anth.set(fakeAnswer{200, claudeTextBody}, http.StatusOK, true)
			orch, _ := newSelectedProviderOrch(t, selectedProviderConfig(t, def.URL, backup.URL, anth.URL, setup.fallbacks, "{}"), nil)

			ctx, cancel := context.WithCancel(selectedProfile(context.Background(), "anthropic/claude-x"))
			go func() {
				<-anth.held
				cancel()
			}()
			_, _ = orch.Run(ctx, "s1", "Hi")
			cancel()

			anth.set(fakeAnswer{200, claudeTextBody}, http.StatusOK, false)
			res, err := orch.Run(selectedProfile(context.Background(), "anthropic/claude-x"), "s1", "Hi again")
			if err != nil || res.Response != fromClaude {
				t.Fatalf("the turn after the cancelled one answered %+v, %v; want Claude's answer", res, err)
			}
			res, err = orch.Run(selectedProfile(context.Background(), ""), "s1", "And without a model?")
			if err != nil || res.Response != fromDefault {
				t.Fatalf("a turn without a profile model answered %+v, %v; want the default chain's answer", res, err)
			}
			if got := len(def.received()); got != 1 {
				t.Errorf("the primary received %d calls, want 1: only the turn without a profile model", got)
			}
			if got := len(backup.received()); got != 0 {
				t.Errorf("the fallback received %d calls, want none: the primary must stay in rotation", got)
			}
			if w := warnings(logs.String(), "model=claude-x"); len(w) != 0 {
				t.Errorf("a cancelled turn was logged as a failure of the selected provider:\n%s", strings.Join(w, "\n"))
			}
		})
	}
}

type listExecutor struct{}

func (listExecutor) Execute(_ context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	return orchestrator.ToolResult{CallID: call.ID, Content: "3 items"}
}

func TestConversationChangesModels(t *testing.T) {
	for _, setup := range routingSetups {
		t.Run(setup.name, func(t *testing.T) {
			captureLogs(t)
			anth := newFakeLLMServer(t,
				fakeAnswer{200, claudeToolUseBody},
				fakeAnswer{400, anthInvalidRequest},
				fakeAnswer{200, claudeTextBody})
			def := newFakeLLMServer(t, fakeAnswer{200, defaultToolCallBody}, fakeAnswer{200, defaultTextBody})
			backup := newFakeLLMServer(t, fakeAnswer{200, defaultTextBody})
			registry := orchestrator.NewToolRegistry()
			if err := registry.Register(orchestrator.PluginCapability{
				Name:        "inv",
				Description: "Inventory",
				Actions: []orchestrator.Action{{Name: "list", Description: "List items", AlwaysInclude: true, ReadOnly: true,
					Parameters: []orchestrator.Parameter{{Name: "page", Description: "Page"}}}},
			}, listExecutor{}); err != nil {
				t.Fatal(err)
			}
			orch, sessions := newSelectedProviderOrch(t,
				selectedProviderConfig(t, def.URL, backup.URL, anth.URL, setup.fallbacks, "{}"), registry)
			run := func(model, message, want string) {
				t.Helper()
				res, err := orch.Run(selectedProfile(context.Background(), model), "s1", message)
				if err != nil {
					t.Fatalf("turn %q failed: %v", message, err)
				}
				if res.Response != want {
					t.Fatalf("turn %q answered %q, want %q", message, res.Response, want)
				}
			}

			run("anthropic/claude-x", "Hi", fromClaude)
			run("", "Hi without a model", fromDefault)
			if err := sessions.AddMessage("s1", provider.Message{Role: provider.RoleUser, Content: "A note posted without a reply."}); err != nil {
				t.Fatal(err)
			}
			run("anthropic/claude-x", "Hi again", fromClaude)

			anthCalls, defCalls := anth.received(), def.received()
			if len(anthCalls) != 4 || len(defCalls) != 2 {
				t.Fatalf("Anthropic received %d calls and the default model %d, want 4 and 2", len(anthCalls), len(defCalls))
			}
			for _, want := range []string{`"id":"toolu_1"`, `"tool_call_id":"toolu_1"`} {
				if !strings.Contains(defCalls[0], want) {
					t.Errorf("the default model's round lacks %s:\n%s", want, defCalls[0])
				}
			}
			for _, call := range anthCalls[2:] {
				for _, want := range []string{`"id":"toolu_1"`, `"id":"call_2"`, `"tool_use_id":"toolu_1"`, `"tool_use_id":"call_2"`} {
					if !strings.Contains(call, want) {
						t.Errorf("an Anthropic request after the change lacks %s:\n%s", want, call)
					}
				}
			}
			if !userRowsInARow(t, anthCalls[3], "A note posted without a reply.", "Hi again") {
				t.Errorf("the third turn's request does not carry the note and the message as two user rows in a row:\n%s", anthCalls[3])
			}
		})
	}
}

func userRowsInARow(t *testing.T, body, first, second string) bool {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	text := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var blocks []struct {
			Type, Text string
		}
		_ = json.Unmarshal(raw, &blocks)
		var b strings.Builder
		for _, bl := range blocks {
			b.WriteString(bl.Text)
		}
		return b.String()
	}
	for i := 0; i+1 < len(req.Messages); i++ {
		a, b := req.Messages[i], req.Messages[i+1]
		if a.Role == "user" && b.Role == "user" && strings.Contains(text(a.Content), first) && strings.Contains(text(b.Content), second) {
			return true
		}
	}
	return false
}

func sentModel(t *testing.T, body string) string {
	t.Helper()
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	return sent.Model
}
