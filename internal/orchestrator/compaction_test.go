package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

const (
	compactModelA = provider.ModelRef("p/a")
	compactModelB = provider.ModelRef("p/b")
	compactTurn   = "What is low on stock?"
)

// compactingLLM serves a chain of models with windows and answers chat rounds
// and summary requests (CompletionRequest.Compaction) apart: it refuses the
// first refusals chat rounds for size, then answers them as the chain's first
// model; a summary request gets summary, or summaryErr.
type compactingLLM struct {
	mu         sync.Mutex
	chain      []provider.ModelInfo
	refusals   int
	summary    string
	summaryErr error
	requests   []*provider.CompletionRequest
}

func (l *compactingLLM) ModelChain(provider.ModelRef) []provider.ModelInfo { return l.chain }

func (l *compactingLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, req)
	if req.Compaction {
		if l.summaryErr != nil {
			return nil, l.summaryErr
		}
		return &provider.CompletionResponse{Content: l.summary, ModelRef: compactModelA,
			Usage: provider.Usage{InputTokens: 100, OutputTokens: 10}}, nil
	}
	if l.refusals > 0 {
		l.refusals--
		return nil, errors.New("400: context_length_exceeded")
	}
	return &provider.CompletionResponse{Content: "Answer.", ModelRef: compactModelA,
		Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

func (l *compactingLLM) summaries() []*provider.CompletionRequest {
	var out []*provider.CompletionRequest
	for _, r := range l.requests {
		if r.Compaction {
			out = append(out, r)
		}
	}
	return out
}

func (l *compactingLLM) chats() []*provider.CompletionRequest {
	var out []*provider.CompletionRequest
	for _, r := range l.requests {
		if !r.Compaction {
			out = append(out, r)
		}
	}
	return out
}

// windowModel is a model of the chain whose input budget is budget estimate
// tokens (no max_tokens, so the budget is 90 % of the window).
func windowModel(ref provider.ModelRef, budget int) provider.ModelInfo {
	return provider.ModelInfo{ProviderID: ref.Provider(), ID: ref.Model(), ContextWindow: budget * 10 / 9}
}

type eventReader struct {
	payloads []json.RawMessage
	err      error
}

func (r eventReader) RecentPayloads(_ context.Context, _ string, limit int, accept func(json.RawMessage) bool) ([]json.RawMessage, error) {
	if r.err != nil {
		return nil, r.err
	}
	var out []json.RawMessage
	for _, p := range r.payloads {
		if accept(p) {
			if out = append(out, p); len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// compactionRun is a text-mode orchestrator over session s1, seeded with
// history, and raw: the uncalibrated estimate of the first request of a turn
// asking compactTurn, measured on an identical session.
type compactionRun struct {
	ctx      context.Context // carries a profile, so the turn's usage is recorded
	orch     *Orchestrator
	llm      *compactingLLM
	sessions *state.SessionStore
	sink     *recordingEventSink
	usage    *usageSpy
	frames   *[]map[string]string
	raw      int
}

func newCompactionRun(t *testing.T, history []provider.Message, opts OrchestratorOpts) compactionRun {
	t.Helper()
	llm := &compactingLLM{summary: "The user keeps a warehouse."}
	sessions := state.NewSessionStore("")
	for _, id := range []string{"probe", "s1"} {
		sessions.Create(state.SessionParams{ID: id})
		for _, m := range history {
			if err := sessions.AddMessage(id, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	run := compactionRun{ctx: profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1"}), llm: llm, sessions: sessions, sink: &recordingEventSink{}, usage: &usageSpy{}, frames: &[]map[string]string{}}
	opts.EventSink, opts.UsageRecorder = run.sink, run.usage
	var mu sync.Mutex
	opts.ChannelSender = func(_ context.Context, _ string, msg pkgchannel.OutboundMessage) error {
		mu.Lock()
		defer mu.Unlock()
		if msg.Metadata["type"] == "session.compaction" {
			*run.frames = append(*run.frames, msg.Metadata)
		}
		return nil
	}
	run.orch = NewWithRules(llm, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), sessions, opts)
	if _, err := run.orch.Run(run.ctx, "probe", compactTurn); err != nil {
		t.Fatal(err)
	}
	run.raw = estimateRequestTokens(llm.requests[0])
	llm.requests, run.sink.events, run.usage.calls = nil, nil, nil
	return run
}

// longHistory is a conversation whose assistant answers fill the window.
func longHistory() []provider.Message {
	long := strings.Repeat("stock ", 7000)
	return []provider.Message{
		{Role: provider.RoleUser, Content: "Which items do we have?"},
		{Role: provider.RoleAssistant, Content: long},
		{Role: provider.RoleUser, Content: "A note from the export.", Visibility: provider.VisibilityHidden},
		{Role: provider.RoleUser, Content: "And the containers?"},
		{Role: provider.RoleAssistant, Content: long},
	}
}

func eventPayloads[T any](t *testing.T, evs []emit.Event, eventType string) []T {
	t.Helper()
	var out []T
	for _, e := range evs {
		if e.EventType != eventType {
			continue
		}
		var p T
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("%s payload: %v", eventType, err)
		}
		out = append(out, p)
	}
	return out
}

func frameStates(frames []map[string]string) []string {
	var out []string
	for _, f := range frames {
		out = append(out, f["state"])
	}
	return out
}

// A request is compacted first when its estimate reaches 95 % of the input
// budget of any model of the chain, each with its own calibration; a model
// without a window sets no trigger.
func TestCompaction_TriggersPerChainModel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chain   func(raw int) []provider.ModelInfo
		factorA float64
		reached provider.ModelRef // "" when nothing is compacted
	}{
		{"under every trigger", func(raw int) []provider.ModelInfo {
			return []provider.ModelInfo{windowModel(compactModelA, 2*raw), windowModel(compactModelB, 2*raw)}
		}, 1, ""},
		{"the first model's trigger", func(raw int) []provider.ModelInfo {
			return []provider.ModelInfo{windowModel(compactModelA, raw*102/100)}
		}, 1, compactModelA},
		{"a fallback model's trigger", func(raw int) []provider.ModelInfo {
			return []provider.ModelInfo{windowModel(compactModelA, 2*raw), windowModel(compactModelB, raw*102/100)}
		}, 1, compactModelB},
		{"the first model's calibration", func(raw int) []provider.ModelInfo {
			return []provider.ModelInfo{windowModel(compactModelA, raw*3/2)}
		}, 1.6, compactModelA},
		{"no window", func(int) []provider.ModelInfo {
			return []provider.ModelInfo{{ProviderID: "p", ID: "a"}}
		}, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newCompactionRun(t, longHistory(), OrchestratorOpts{})
			run.llm.chain = tc.chain(run.raw)
			run.orch.calibrators.observe(string(compactModelA), 1000, int(1000*tc.factorA))

			if _, err := run.orch.Run(run.ctx, "s1", compactTurn); err != nil {
				t.Fatal(err)
			}
			started := eventPayloads[events.CompactionStartedPayload](t, run.sink.snapshot(), events.TypeCompactionStarted)
			if tc.reached == "" {
				if len(started) != 0 || len(run.llm.summaries()) != 0 {
					t.Fatalf("nothing may be compacted, got %d compaction(s)", len(started))
				}
				return
			}
			if len(started) != 1 || len(run.llm.summaries()) != 1 {
				t.Fatalf("want one compaction, got %d started and %d summary call(s)", len(started), len(run.llm.summaries()))
			}
			if p := started[0]; provider.ModelRef(p.Model) != tc.reached || p.EstimatedTokens < p.TriggerTokens {
				t.Errorf("compaction_started = %+v, want model %s at an estimate past its trigger", p, tc.reached)
			}
		})
	}
}

// A compaction stores one summary row; from it on a request carries the
// unchanged system text, the summary, the kept user messages in their order
// and the messages stored after it. The summary request continues the round's
// request, its usage counts, and the events and frames tell of it.
func TestCompaction_CompactsTheConversation(t *testing.T) {
	prior, _ := json.Marshal(events.CompactionFinishedPayload{SummaryModel: string(compactModelA), DurationMS: 1500})
	run := newCompactionRun(t, longHistory(), OrchestratorOpts{SessionEvents: eventReader{payloads: []json.RawMessage{prior}}})
	run.llm.chain = []provider.ModelInfo{windowModel(compactModelA, run.raw*102/100)}

	if _, err := run.orch.Run(run.ctx, "s1", compactTurn); err != nil {
		t.Fatal(err)
	}
	if _, err := run.orch.Run(run.ctx, "s1", "And today?"); err != nil {
		t.Fatal(err)
	}

	summaries, chats := run.llm.summaries(), run.llm.chats()
	if len(summaries) != 1 || len(chats) != 2 {
		t.Fatalf("want one summary call and two chat rounds, got %d and %d", len(summaries), len(chats))
	}
	summaryReq := summaries[0]
	words := summaryWords(int(float64(room(run.orchLimits(t), fixedTokens(summaryReq))) * summaryShare))
	if last := summaryReq.Messages[len(summaryReq.Messages)-1]; last.Role != provider.RoleUser ||
		last.Content != fmt.Sprintf(prompts.Compaction, words) {
		t.Errorf("the summary request must end on the instruction for %d words, ends on %+v", words, last)
	}
	if len(summaryReq.Messages) != 8 || summaryReq.Segments == nil {
		t.Errorf("the summary request must continue the round's request (system, history, turn), got %d messages", len(summaryReq.Messages))
	}

	sess, _ := run.sessions.Get("s1")
	at := lastCompaction(sess.Messages)
	if at != 6 || sess.Messages[at].Role != provider.RoleUser || sess.Messages[at].Content != run.llm.summary ||
		sess.Messages[at].KeptUserMessages != 3 {
		t.Fatalf("stored compaction row = %+v at %d, want the summary keeping 3 user messages after the turn's message", sess.Messages[at], at)
	}
	contents := func(req *provider.CompletionRequest) []string {
		var out []string
		for _, m := range req.Messages[1:] {
			out = append(out, m.Content)
		}
		return out
	}
	want := []string{run.llm.summary, "Which items do we have?", "And the containers?", compactTurn, "Answer.", "And today?"}
	if got := contents(chats[1]); !reflect.DeepEqual(got, want) {
		t.Errorf("the next turn's request carries %q, want %q", got, want)
	}
	if chats[0].Messages[0].Content != summaryReq.Messages[0].Content {
		t.Error("the compacted request must keep the system text of the request it replaced")
	}

	evs := run.sink.snapshot()
	started := eventPayloads[events.CompactionStartedPayload](t, evs, events.TypeCompactionStarted)
	finished := eventPayloads[events.CompactionFinishedPayload](t, evs, events.TypeCompactionFinished)
	if len(started) != 1 || started[0].ExpectedMS != 1500 {
		t.Errorf("compaction_started = %+v, want one expecting 1500 ms", started)
	}
	if len(finished) != 1 || finished[0].SummaryModel != string(compactModelA) || finished[0].Fallback ||
		finished[0].SummaryBytes != len(run.llm.summary) || finished[0].UserMessagesKept != 3 {
		t.Errorf("compaction_finished = %+v, want the summary of p/a keeping 3 user messages", finished)
	}
	if got := frameStates(*run.frames); !reflect.DeepEqual(got, []string{"started", "finished"}) || (*run.frames)[0]["expected_ms"] != "1500" {
		t.Errorf("frames = %v, want started (expected_ms 1500) and finished", *run.frames)
	}
	if calls := run.usage.calls; len(calls) != 2 || calls[0].in != 101 || calls[0].out != 11 {
		t.Errorf("usage = %+v, want the compacted turn to count the summary call (101 in, 11 out)", calls)
	}
}

// orchLimits are the limits of a turn on s1 as the run's chain sets them.
func (r compactionRun) orchLimits(t *testing.T) []chainLimit {
	t.Helper()
	req, err := r.orch.newRoundRequests(context.Background(), r.sessions, "s1", provider.SystemSegments{}, "")
	if err != nil {
		t.Fatal(err)
	}
	return req.limits()
}

// A request that still exceeds the input budget right after a compaction ends
// the turn with context_length_exceeded instead of compacting again; so does a
// summary call that fails while the request does not fit. A failed summary
// call on a request that fits leaves the turn going on uncompacted.
func TestCompaction_WhenItCannotMakeRoom(t *testing.T) {
	for _, tc := range []struct {
		name       string
		turn       string
		budget     func(raw int) int
		summary    string
		summaryErr error
		fails      bool
		frames     []string
	}{
		{"the kept turn alone exceeds the budget", strings.Repeat("why ", 40000), func(raw int) int { return raw / 2 },
			"Summary.", nil, true, []string{"started", "finished"}},
		{"the summary call fails, the request fits", compactTurn, func(raw int) int { return raw * 102 / 100 },
			"", errors.New("upstream 503"), false, []string{"started", "failed"}},
		{"the model writes no summary, the request fits", compactTurn, func(raw int) int { return raw * 102 / 100 },
			"  ", nil, false, []string{"started", "failed"}},
		{"the summary call fails, the request does not fit", compactTurn, func(raw int) int { return raw * 9 / 10 },
			"", errors.New("upstream 503"), true, []string{"started", "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newCompactionRun(t, longHistory(), OrchestratorOpts{})
			run.llm.chain = []provider.ModelInfo{windowModel(compactModelA, tc.budget(run.raw))}
			run.llm.summary, run.llm.summaryErr = tc.summary, tc.summaryErr

			_, err := run.orch.Run(run.ctx, "s1", tc.turn)
			if tc.fails != (err != nil) || (err != nil && !provider.IsContextOverflow(err)) {
				t.Fatalf("Run error = %v, want context_length_exceeded: %v", err, tc.fails)
			}
			if n := len(run.llm.summaries()); n != 1 {
				t.Errorf("summary calls = %d, want exactly one", n)
			}
			if got := frameStates(*run.frames); !reflect.DeepEqual(got, tc.frames) {
				t.Errorf("frames = %v, want %v", got, tc.frames)
			}
			failed := eventPayloads[events.CompactionFailedPayload](t, run.sink.snapshot(), events.TypeCompactionFailed)
			if wantFailed := tc.frames[1] == "failed"; wantFailed != (len(failed) == 1) {
				t.Errorf("compaction_failed events = %+v, want one: %v", failed, wantFailed)
			}
			if !tc.fails {
				chats := run.llm.chats()
				if len(chats) != 1 || len(chats[0].Messages) != 7 {
					t.Errorf("the turn must go on with the uncompacted request, got %d chat request(s)", len(chats))
				}
			}
		})
	}
}

// A refusal for size starts one compaction and one retry; a second refusal,
// one right after a compaction, a retry that still exceeds the budget the
// refusal raised, or a refusal without a window ends the turn with
// context_length_exceeded.
func TestCompaction_RefusalForSize(t *testing.T) {
	longTurn := strings.Repeat("why ", 80000)
	longRaw := func(raw int) int {
		return raw - estimateMessageTokens(provider.Message{Role: provider.RoleUser, Content: compactTurn}) +
			estimateMessageTokens(provider.Message{Role: provider.RoleUser, Content: longTurn})
	}
	for _, tc := range []struct {
		name      string
		turn      string
		budget    func(raw int) int
		refusals  int
		fails     bool
		summaries int
		chats     int
	}{
		{"one refusal", compactTurn, func(raw int) int { return 2 * raw }, 1, false, 1, 2},
		{"two refusals", compactTurn, func(raw int) int { return 2 * raw }, 2, true, 1, 2},
		{"a refusal right after a compaction", compactTurn, func(raw int) int { return raw * 102 / 100 }, 1, true, 1, 1},
		{"the retry exceeds the raised budget", longTurn, func(raw int) int { return longRaw(raw) * 10 / 9 }, 1, true, 1, 1},
		{"a refusal without a window", compactTurn, func(int) int { return 0 }, 1, true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newCompactionRun(t, longHistory(), OrchestratorOpts{})
			run.llm.chain = []provider.ModelInfo{windowModel(compactModelA, tc.budget(run.raw))}
			run.llm.refusals = tc.refusals

			res, err := run.orch.Run(run.ctx, "s1", tc.turn)
			if tc.fails {
				if !provider.IsContextOverflow(err) {
					t.Fatalf("Run error = %v, want context_length_exceeded", err)
				}
			} else if err != nil || res.Response != "Answer." {
				t.Fatalf("the retry must be answered, got %v, %+v", err, res)
			}
			if n, m := len(run.llm.summaries()), len(run.llm.chats()); n != tc.summaries || m != tc.chats {
				t.Errorf("summary calls = %d, chat requests = %d, want %d and %d", n, m, tc.summaries, tc.chats)
			}
			if got := run.orch.calibrators.factor(string(compactModelA)); got != rejectionStep {
				t.Errorf("calibration after the refusal = %v, want %v", got, rejectionStep)
			}
		})
	}
}

// The user messages a compaction keeps: the newest always, the turn's own
// even when hidden, then visible ones newest first while they fit; tool
// results and rows with a prompt type never.
func TestCompaction_KeptUserMessages(t *testing.T) {
	user := func(content string) provider.Message {
		return provider.Message{Role: provider.RoleUser, Content: content}
	}
	hidden := func(m provider.Message) provider.Message { m.Visibility = provider.VisibilityHidden; return m }
	typed := func(m provider.Message, pt string) provider.Message { m.PromptType = pt; return m }
	cost := estimateMessageTokens(user("0123456789"))
	for _, tc := range []struct {
		name   string
		before []provider.Message
		cap    int
		want   []int // indices kept, newest first
	}{
		{"visible ones newest first", []provider.Message{user("0123456789"), {Role: provider.RoleAssistant}, user("0123456789")},
			10 * cost, []int{2, 0}},
		{"the hidden newest, not an older hidden one", []provider.Message{hidden(user("0123456789")), user("0123456789"), hidden(user("0123456789"))},
			10 * cost, []int{2, 1}},
		{"no tool results or typed rows", []provider.Message{user("[plugin_output]x[/plugin_output]"), {Role: provider.RoleTool, Content: "r"},
			typed(user("summary"), provider.PromptTypeCompaction), typed(user("0123456789"), "confirmation"), user("0123456789")},
			10 * cost, []int{4}},
		{"the cap stops them, the newest always", []provider.Message{user("0123456789"), user(strings.Repeat("x", 4000))},
			cost, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := keptUserCount(tc.before, tc.cap)
			if got := keptUserMessages(tc.before)[:n]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

// The stored summary is the model's text cut at a rune boundary, and the line
// naming the tools called before it, natively or in text format, without the
// core's meta-tools.
func TestCompaction_SummaryText(t *testing.T) {
	stored := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "inv__list"}, {Name: "_meta__load_tools"}}},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "inv__add"}, {Name: "inv__list"}}},
		{Role: provider.RoleAssistant, Content: formatToolCallMessage(ToolCall{Plugin: "inv", Action: "count", Args: map[string]string{"q": "x"}})},
	}
	tools := toolsCalled(stored)
	if !reflect.DeepEqual(tools, []string{"inv__add", "inv__count", "inv__list"}) {
		t.Fatalf("tools called = %v", tools)
	}
	line := "\n\n" + fmt.Sprintf(prompts.CompactionTools, "inv__add, inv__count, inv__list")
	if got := toolsLine(tools); got != line {
		t.Errorf("toolsLine = %q, want %q", got, line)
	}
	if got := toolsLine(nil); got != "" {
		t.Errorf("toolsLine without tools = %q, want none", got)
	}
	for _, tc := range []struct {
		name, written, tools string
		limit                int
		want                 string
	}{
		{"with the tools line", " Done. ", line, 1000, "Done." + line},
		{"cut at a rune boundary", strings.Repeat("ä", 10), "", 9, strings.Repeat("ä", 4)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryText(tc.written, tc.tools, tc.limit); got != tc.want {
				t.Errorf("summaryText = %q, want %q", got, tc.want)
			}
		})
	}
}

// A compaction whose line naming the tools called fills the summary's share
// of the room alone fails before the summary call; the turn goes on.
func TestCompaction_ToolsLineFillsTheSummaryShare(t *testing.T) {
	var calls []provider.ToolCall
	for i := range 400 {
		calls = append(calls, provider.ToolCall{ID: fmt.Sprint("c", i), Name: fmt.Sprintf("inventory__%s_%03d", strings.Repeat("x", 100), i)})
	}
	history := append(longHistory(), provider.Message{Role: provider.RoleAssistant, ToolCalls: calls})
	run := newCompactionRun(t, history, OrchestratorOpts{})
	run.llm.chain = []provider.ModelInfo{windowModel(compactModelA, run.raw*102/100)}

	if _, err := run.orch.Run(run.ctx, "s1", compactTurn); err != nil {
		t.Fatal(err)
	}
	if n := len(run.llm.summaries()); n != 0 {
		t.Errorf("summary calls = %d, want none", n)
	}
	if got := frameStates(*run.frames); !reflect.DeepEqual(got, []string{"started", "failed"}) {
		t.Errorf("frames = %v, want started and failed", got)
	}
	if failed := eventPayloads[events.CompactionFailedPayload](t, run.sink.snapshot(), events.TypeCompactionFailed); len(failed) != 1 {
		t.Errorf("compaction_failed events = %+v, want one", failed)
	}
}

// Tool results are shortened in a summary request only while it exceeds a
// model's input budget: the longest first, never below shortenedResultMin.
func TestCompaction_ShortenToolResults(t *testing.T) {
	result := func(n int) provider.Message {
		return provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: strings.Repeat("{}", n/2)}
	}
	req := func() *provider.CompletionRequest {
		return &provider.CompletionRequest{Messages: []provider.Message{result(3000), result(60000), result(2400)}}
	}
	fits := []chainLimit{{factor: 1, budget: estimateRequestTokens(req())}}
	r := req()
	shortenToolResults(r, fits)
	if !reflect.DeepEqual(r, req()) {
		t.Error("a request that fits must stay as it is")
	}
	tight := []chainLimit{{factor: 1, budget: estimateRequestTokens(req()) / 4}}
	r = req()
	shortenToolResults(r, tight)
	if got := r.Messages[1].Content; len(got) >= 60000 || !strings.HasSuffix(got, shortenedMarker) {
		t.Errorf("the longest result must be shortened with the marker, has %d bytes", len(got))
	}
	for i, m := range r.Messages {
		if len(m.Content) < shortenedResultMin && m.Content != req().Messages[i].Content {
			t.Errorf("result %d shortened below %d: %d bytes", i, shortenedResultMin, len(m.Content))
		}
	}
	r = req()
	shortenToolResults(r, []chainLimit{{factor: 1, budget: 1}})
	for i, m := range r.Messages {
		if len(m.Content) > shortenedResultMin+len(shortenedMarker) {
			t.Errorf("a request that cannot fit must leave every result at its minimum, result %d has %d bytes", i, len(m.Content))
		}
	}
}

// A compaction drops the tools the conversation before it loaded.
func TestCompaction_DropsLoadedTools(t *testing.T) {
	orch, _, _ := loadOnCallOrch(t)
	ctx := sentCtx("p__always")
	stored := []provider.Message{
		{Role: provider.RoleUser, Content: "load it"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
			{ID: "l1", Name: "_meta__load_tools", Arguments: map[string]string{"names": "p__other"}}}},
		{Role: provider.RoleTool, ToolCallID: "l1", Content: "loaded"},
	}
	if got := sortedSet(orch.loadedTools(ctx, requestConversation(stored))); !reflect.DeepEqual(got, []string{"p__other"}) {
		t.Fatalf("loaded before the compaction = %v, want [p__other]", got)
	}
	stored = append(stored, provider.Message{Role: provider.RoleUser, Content: "Summary.", PromptType: provider.PromptTypeCompaction, KeptUserMessages: 1})
	if got := orch.loadedTools(ctx, requestConversation(stored)); len(got) != 0 {
		t.Errorf("loaded after the compaction = %v, want none", got)
	}
}

// The expected duration is the weighted mean of the model's newest
// compactions without a fallback, newest weighing most; none without such
// compactions or a readable event store.
func TestExpectedCompactionMS(t *testing.T) {
	finished := func(model string, ms int64, fallback bool) json.RawMessage {
		b, _ := json.Marshal(events.CompactionFinishedPayload{SummaryModel: model, DurationMS: ms, Fallback: fallback})
		return b
	}
	many := make([]json.RawMessage, 0, expectedDurationSamples+2)
	for range expectedDurationSamples {
		many = append(many, finished("p/a", 1000, false))
	}
	many = append(many, finished("p/a", 90000, false))
	for _, tc := range []struct {
		name   string
		reader SessionEventReader
		want   int64
	}{
		{"newest weighs most", eventReader{payloads: []json.RawMessage{finished("p/a", 1000, false), finished("p/a", 2000, false)}},
			weightedMean([]int64{1000, 2000})},
		{"other models and fallbacks do not count", eventReader{payloads: []json.RawMessage{
			finished("p/b", 9000, false), finished("p/a", 9000, true), finished("p/a", 0, false), finished("p/a", 1200, false)}}, 1200},
		{"only the newest samples", eventReader{payloads: many}, 1000},
		{"no compaction yet", eventReader{}, 0},
		{"unreadable events", eventReader{err: errors.New("db down")}, 0},
		{"no event store", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch := NewWithRules(&fakeLLM{}, noToolCalls(), NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
				OrchestratorOpts{SessionEvents: tc.reader})
			if got := orch.expectedCompactionMS(context.Background(), compactModelA); got != tc.want {
				t.Errorf("expected = %d, want %d", got, tc.want)
			}
		})
	}
	if got := weightedMean([]int64{1000, 2000}); got != 1412 {
		t.Errorf("weightedMean(1000, 2000) = %d, want 1412 ((1000 + 0.7×2000) / 1.7)", got)
	}
}
