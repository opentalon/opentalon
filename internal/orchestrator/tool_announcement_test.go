package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

type announceRequest struct {
	model    string
	messages []provider.Message
	onDemand []string
}

type announceLLM struct {
	rounds []provider.CompletionResponse
	onCall func(call int, req *provider.CompletionRequest) error
	calls  int
	seen   []announceRequest
}

func (l *announceLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	seen := announceRequest{model: req.Model, messages: slices.Clone(req.Messages)}
	for i, m := range seen.messages {
		if a := m.Announcement; a != nil {
			seen.messages[i].Announcement = &provider.ToolAnnouncement{Added: slices.Clone(a.Added), Removed: slices.Clone(a.Removed)}
		}
	}
	for _, td := range req.Tools {
		if td.OnDemand {
			seen.onDemand = append(seen.onDemand, td.Name)
		}
	}
	slices.Sort(seen.onDemand)
	l.seen = append(l.seen, seen)
	call := l.calls
	l.calls++
	if l.onCall != nil {
		if err := l.onCall(call, req); err != nil {
			return nil, err
		}
	}
	if call < len(l.rounds) {
		r := l.rounds[call]
		return &r, nil
	}
	return &provider.CompletionResponse{Content: "done"}, nil
}

func (l *announceLLM) SupportsFeature(f provider.Feature) bool {
	return f == provider.FeatureTools
}

func announceActions() []Action {
	query := []Parameter{{Name: "q", Description: "Query."}}
	return []Action{
		{Name: "always", Description: "Always-include tool.", AlwaysInclude: true, ReadOnly: true},
		{Name: "catalog", Description: "Catalog tool.", ReadOnly: true, Parameters: query, Companions: []string{"lookup", "list"}},
		{Name: "other", Description: "Another catalog tool.", ReadOnly: true, Parameters: query},
		{Name: "lookup", Description: "The catalog tool's first companion.", ReadOnly: true, Parameters: query},
		{Name: "list", Description: "The catalog tool's second companion.", ReadOnly: true, Parameters: query},
	}
}

func announceOrch(t *testing.T, opts OrchestratorOpts, exec PluginExecutor, actions []Action, rounds ...provider.CompletionResponse) (
	*Orchestrator, *announceLLM, *state.SessionStore,
) {
	t.Helper()
	sessions := state.NewSessionStore("")
	orch, llm := announceOrchOn(t, sessions, opts, exec, actions, rounds...)
	return orch, llm, sessions
}

func announceOrchOn(t *testing.T, sessions SessionStoreInterface, opts OrchestratorOpts, exec PluginExecutor, actions []Action,
	rounds ...provider.CompletionResponse) (*Orchestrator, *announceLLM) {
	t.Helper()
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{Name: "p", Description: "announcement fixtures", Actions: actions}, exec); err != nil {
		t.Fatalf("register: %v", err)
	}
	sessions.Create(state.SessionParams{ID: "s1"})
	if opts.InjectionStateStore == nil {
		opts.InjectionStateStore = &fakeInjectionStateStore{}
	}
	llm := &announceLLM{rounds: rounds}
	orch := NewWithRules(llm, &fakeParser{parseFn: func(string) []ToolCall { return nil }}, registry,
		state.NewMemoryStore(""), sessions, opts)
	return orch, llm
}

func announceTurn(t *testing.T, ctx context.Context, orch *Orchestrator, message string) {
	t.Helper()
	if _, err := orch.Run(actor.WithSessionID(ctx, "s1"), "s1", message); err != nil {
		t.Fatalf("Run %q: %v", message, err)
	}
}

func recordsOf(msgs []provider.Message) []string {
	out := []string{}
	for _, m := range msgs {
		a := m.Announcement
		if a == nil {
			continue
		}
		label := m.Content
		if m.Role == provider.RoleTool {
			label = m.ToolCallID
		}
		r := string(m.Role) + " " + label
		for _, name := range a.Added {
			r += " +" + name
		}
		for _, name := range a.Removed {
			r += " -" + name
		}
		out = append(out, r)
	}
	return out
}

func storedRecords(t *testing.T, sessions SessionStoreInterface) []string {
	t.Helper()
	sess, err := sessions.Get("s1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return recordsOf(sess.Messages)
}

func announcedSet(msgs []provider.Message) []string {
	set := map[string]bool{}
	for _, m := range msgs {
		if m.Announcement == nil {
			continue
		}
		for _, name := range m.Announcement.Added {
			set[name] = true
		}
		for _, name := range m.Announcement.Removed {
			delete(set, name)
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func checkAnnounced(t *testing.T, seen []announceRequest, unrecorded ...int) {
	t.Helper()
	for i, r := range seen {
		if slices.Contains(unrecorded, i) {
			continue
		}
		if got := announcedSet(r.messages); !slices.Equal(got, r.onDemand) {
			t.Errorf("request %d: announced %v, callable on demand %v", i+1, got, r.onDemand)
		}
	}
}

func checkRecordsKept(t *testing.T, seen []announceRequest) {
	t.Helper()
	type identity struct {
		role        provider.Role
		id, content string
	}
	carried := map[identity]*provider.ToolAnnouncement{}
	for k, r := range seen {
		for _, m := range r.messages {
			id := identity{m.Role, m.ToolCallID, m.Content}
			if before, ok := carried[id]; ok && !reflect.DeepEqual(before, m.Announcement) {
				t.Errorf("request %d changed a carried record: %v, before %v", k+1,
					recordsOf([]provider.Message{m}), recordsOf([]provider.Message{{Role: m.Role, ToolCallID: m.ToolCallID, Content: m.Content, Announcement: before}}))
			}
			carried[id] = m.Announcement
		}
	}
}

func loadCall(id, names string) provider.CompletionResponse {
	return callOf(id, "_meta__load_tools", map[string]string{"names": names})
}

func TestRun_ToolLoadIsRecordedOnTheRoundsLastResult(t *testing.T) {
	loadOther := provider.ToolCall{ID: "l1", Name: "_meta__load_tools", Arguments: map[string]string{"names": "p__other"}}
	always := provider.ToolCall{ID: "a1", Name: "p__always", Arguments: map[string]string{}}
	for _, tc := range []struct {
		name   string
		rounds []provider.CompletionResponse
		want   []string
	}{
		{"load_tools with companions", []provider.CompletionResponse{loadCall("l1", "p__catalog")},
			[]string{"tool l1 +p__catalog +p__list +p__lookup"}},
		{"load on call with companions", []provider.CompletionResponse{callOf("r1", "p__catalog", map[string]string{"q": "x"})},
			[]string{"tool r1 +p__catalog +p__list +p__lookup"}},
		{"a load in a batch with a fixed tool", []provider.CompletionResponse{{ToolCalls: []provider.ToolCall{loadOther, always}}},
			[]string{"tool a1 +p__other"}},
		{"a load of a tool already announced", []provider.CompletionResponse{loadCall("l1", "p__other"), loadCall("l2", "p__other")},
			[]string{"tool l1 +p__other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, llm, sessions := announceOrch(t, OrchestratorOpts{}, &countingExecutor{}, announceActions(), tc.rounds...)
			announceTurn(t, context.Background(), orch, "turn 1")
			if len(llm.seen) != len(tc.rounds)+1 {
				t.Fatalf("requests = %d, want %d", len(llm.seen), len(tc.rounds)+1)
			}
			checkAnnounced(t, llm.seen)
			checkRecordsKept(t, llm.seen)
			if got := recordsOf(llm.seen[len(llm.seen)-1].messages); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("records in the last request = %q, want %q", got, tc.want)
			}
			if got := storedRecords(t, sessions); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stored records = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRun_ToolsLeavingTheLoadedSetAreWithdrawnOnTheNextTurn(t *testing.T) {
	var capped []Action
	var names []string
	for i := range maxStickyTools + 5 {
		capped = append(capped, Action{Name: fmt.Sprintf("t%02d", i), Description: "Capped tool.", ReadOnly: true})
		names = append(names, fmt.Sprintf("p__t%02d", i))
	}
	for _, tc := range []struct {
		name    string
		opts    OrchestratorOpts
		exec    PluginExecutor
		actions []Action
		rounds  []provider.CompletionResponse
		want    []string
	}{
		{"more tools loaded in one turn than the loaded set holds", OrchestratorOpts{}, &countingExecutor{}, capped,
			[]provider.CompletionResponse{loadCall("l1", strings.Join(names, ","))},
			[]string{"tool l1 +" + strings.Join(names, " +"), "user turn 2 -" + strings.Join(names[maxStickyTools:], " -")}},
		{"a tool demoted in the turn that loaded it",
			OrchestratorOpts{ToolErrorHandling: ToolErrorHandlingConfig{StickyDemotionThreshold: 1}},
			&errorExecutor{msg: "boom"}, announceActions(),
			[]provider.CompletionResponse{loadCall("l1", "p__other"), callOf("o1", "p__other", map[string]string{"q": "x"})},
			[]string{"tool l1 +p__other", "user turn 2 -p__other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, llm, sessions := announceOrch(t, tc.opts, tc.exec, tc.actions, tc.rounds...)
			announceTurn(t, context.Background(), orch, "turn 1")
			announceTurn(t, context.Background(), orch, "turn 2")
			checkAnnounced(t, llm.seen)
			checkRecordsKept(t, llm.seen)
			if got := storedRecords(t, sessions); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stored records = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRun_NoRecordWhileTheLoadedToolsCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing int
		unread  []int
		want    []string
	}{
		{"read fails in the turn after the load", 2, []int{2}, []string{"tool l1 +p__other"}},
		{"read fails in the turn of the load", 1, []int{0, 1}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeInjectionStateStore{}
			orch, llm, sessions := announceOrch(t, OrchestratorOpts{InjectionStateStore: store}, &countingExecutor{},
				announceActions(), loadCall("l1", "p__other"))
			for turn := 1; turn <= 3; turn++ {
				store.failGetErr = nil
				if turn == tc.failing {
					store.failGetErr = errors.New("simulated db read failure")
				}
				announceTurn(t, context.Background(), orch, fmt.Sprintf("turn %d", turn))
			}
			if len(llm.seen) != 4 {
				t.Fatalf("requests = %d, want 4 (load, answer, then one per turn)", len(llm.seen))
			}
			checkAnnounced(t, llm.seen, tc.unread...)
			checkRecordsKept(t, llm.seen)
			for _, i := range tc.unread {
				if got := recordsOf(llm.seen[i].messages); !reflect.DeepEqual(got, tc.want) {
					t.Errorf("request %d without a read carries records %q", i+1, got)
				}
			}
			if got := storedRecords(t, sessions); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("stored records = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRun_ShortenedHistoryAnnouncesTheCallableToolsAgain(t *testing.T) {
	loaded := " +p__catalog +p__list +p__lookup"
	t.Run("sliding window", func(t *testing.T) {
		orch, llm, sessions := announceOrch(t, OrchestratorOpts{ContextMessages: 2}, &countingExecutor{}, announceActions(),
			loadCall("l1", "p__catalog"))
		announceTurn(t, context.Background(), orch, "turn 1")
		announceTurn(t, context.Background(), orch, "turn 2")
		checkAnnounced(t, llm.seen)
		if got, want := storedRecords(t, sessions), []string{"tool l1" + loaded, "user turn 2" + loaded}; !reflect.DeepEqual(got, want) {
			t.Errorf("stored records = %q, want %q", got, want)
		}
	})
	t.Run("re-fit after the provider refused the request as too long", func(t *testing.T) {
		orch, llm, sessions := announceOrch(t, OrchestratorOpts{}, &countingExecutor{}, announceActions(),
			loadCall("l1", "p__catalog"), provider.CompletionResponse{Content: strings.Repeat("filler ", 20000)})
		llm.onCall = func(call int, req *provider.CompletionRequest) error {
			if call != 2 {
				return nil
			}
			whole := fitRequestToWindow(context.Background(), &provider.CompletionRequest{Messages: req.Messages, Tools: req.Tools}, 0, 0, 1)
			orch.contextWindow = whole*10/9 + 10
			return errors.New("anthropic api error (status 400): prompt is too long: 250000 tokens > 200000 maximum")
		}
		announceTurn(t, context.Background(), orch, "turn 1")
		announceTurn(t, context.Background(), orch, "turn 2")
		if len(llm.seen) != 4 || len(llm.seen[3].messages) >= len(llm.seen[2].messages) {
			t.Fatalf("want turn 2 sent twice, the second time shorter; got %d requests", len(llm.seen))
		}
		checkAnnounced(t, llm.seen)
		for i, want := range map[int][]string{2: {"tool l1" + loaded}, 3: {"user turn 2" + loaded}} {
			if got := recordsOf(llm.seen[i].messages); !reflect.DeepEqual(got, want) {
				t.Errorf("request %d records = %q, want %q", i+1, got, want)
			}
		}
		if got, want := storedRecords(t, sessions), []string{"tool l1" + loaded, "user turn 2" + loaded}; !reflect.DeepEqual(got, want) {
			t.Errorf("stored records = %q, want %q", got, want)
		}
	})
	t.Run("re-fit recomputes the record the refused request set", func(t *testing.T) {
		orch, llm, sessions := announceOrch(t, OrchestratorOpts{ToolErrorHandling: ToolErrorHandlingConfig{StickyDemotionThreshold: 1}},
			&errorExecutor{msg: "boom"}, announceActions(), loadCall("l1", "p__catalog"),
			callOf("k1", "p__lookup", map[string]string{"q": "x"}), provider.CompletionResponse{Content: strings.Repeat("filler ", 20000)})
		llm.onCall = func(call int, req *provider.CompletionRequest) error {
			if call != 3 {
				return nil
			}
			whole := fitRequestToWindow(context.Background(), &provider.CompletionRequest{Messages: req.Messages, Tools: req.Tools}, 0, 0, 1)
			orch.contextWindow = whole*10/9 + 10
			return errors.New("anthropic api error (status 400): prompt is too long: 250000 tokens > 200000 maximum")
		}
		announceTurn(t, context.Background(), orch, "turn 1")
		announceTurn(t, context.Background(), orch, "turn 2")
		if len(llm.seen) != 5 || len(llm.seen[4].messages) >= len(llm.seen[3].messages) {
			t.Fatalf("want turn 2 sent twice, the second time shorter; got %d requests", len(llm.seen))
		}
		checkAnnounced(t, llm.seen)
		for i, want := range map[int][]string{3: {"tool l1" + loaded, "user turn 2 -p__lookup"}, 4: {"user turn 2 +p__catalog +p__list"}} {
			if got := recordsOf(llm.seen[i].messages); !reflect.DeepEqual(got, want) {
				t.Errorf("request %d records = %q, want %q", i+1, got, want)
			}
		}
		if got, want := storedRecords(t, sessions), []string{"tool l1" + loaded, "user turn 2 +p__catalog +p__list"}; !reflect.DeepEqual(got, want) {
			t.Errorf("stored records = %q, want %q", got, want)
		}
	})
}

func TestRun_ModelSwitchKeepsOneRecordHistory(t *testing.T) {
	orch, llm, sessions := announceOrch(t, OrchestratorOpts{}, &countingExecutor{}, announceActions(),
		loadCall("l1", "p__catalog"), provider.CompletionResponse{Content: "one"},
		loadCall("l2", "p__other"), provider.CompletionResponse{Content: "two"})
	inline := profile.WithProfile(context.Background(), &profile.Profile{Model: "anthropic/claude-inline"})
	for i, ctx := range []context.Context{context.Background(), inline, context.Background()} {
		announceTurn(t, ctx, orch, fmt.Sprintf("turn %d", i+1))
	}
	if len(llm.seen) != 5 {
		t.Fatalf("requests = %d, want 5", len(llm.seen))
	}
	var models []string
	for _, r := range llm.seen {
		models = append(models, r.model)
	}
	if m := models[2]; m == "" || m != models[3] || models[0] != "" || models[1] != "" || models[4] != "" {
		t.Fatalf("models per request = %q, want the default, the profile's model in turn 2, the default", models)
	}
	checkAnnounced(t, llm.seen)
	checkRecordsKept(t, llm.seen)
	want := []string{"tool l1 +p__catalog +p__list +p__lookup", "tool l2 +p__other"}
	if got := storedRecords(t, sessions); !reflect.DeepEqual(got, want) {
		t.Errorf("stored records = %q, want %q", got, want)
	}
}

func TestAppendStrippingHistoricalKC_KeepsTheRecord(t *testing.T) {
	knowledge := "[plugin_output]\n## Knowledge Articles\nbody\n[/plugin_output]"
	large := "[plugin_output]\n" + strings.Repeat("row ", 400) + "\n[/plugin_output]"
	record := func(name string) *provider.ToolAnnouncement { return &provider.ToolAnnouncement{Added: []string{name}} }
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: "[knowledge_context]\nold\n[/knowledge_context]\nold question", Announcement: record("p__a")},
		{Role: provider.RoleUser, Content: knowledge, Announcement: record("p__b")},
		{Role: provider.RoleUser, Content: knowledge, Announcement: record("p__c")},
		{Role: provider.RoleUser, Content: large, Announcement: record("p__d")},
		{Role: provider.RoleUser, Content: large, Announcement: record("p__e")},
		{Role: provider.RoleUser, Content: "current question"},
	}
	out := appendStrippingHistoricalKC(nil, msgs)
	if len(out) != len(msgs) {
		t.Fatalf("messages = %d, want %d", len(out), len(msgs))
	}
	for _, i := range []int{0, 2, 4} {
		if out[i].Content == msgs[i].Content {
			t.Errorf("message %d was not rebuilt: %q", i+1, out[i].Content)
		}
	}
	for i := range msgs {
		if out[i].Announcement != msgs[i].Announcement {
			t.Errorf("message %d record = %v, want %v", i+1, out[i].Announcement, msgs[i].Announcement)
		}
	}
}

func TestRun_RetryMessageCarriesTheRecordWhenTheFitDropsIt(t *testing.T) {
	loaded := " +p__catalog +p__list +p__lookup"
	bigAnswer := strings.Repeat("filler ", 20000)
	for _, tc := range []struct {
		name   string
		window func(req *provider.CompletionRequest) int
		kept   []string
	}{
		{"the fit keeps only the retry message", func(*provider.CompletionRequest) int { return 1 }, nil},
		{"the fit keeps the turn's user message", func(req *provider.CompletionRequest) int {
			whole := fitRequestToWindow(context.Background(), &provider.CompletionRequest{Messages: req.Messages, Tools: req.Tools}, 0, 0, 1)
			big := estimateMessageTokens(provider.Message{Role: provider.RoleAssistant, Content: bigAnswer})
			return (whole - big/2) * 10 / 9
		}, []string{"turn 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orch, llm, sessions := announceOrch(t, OrchestratorOpts{}, &countingExecutor{}, announceActions(),
				loadCall("l1", "p__catalog"), provider.CompletionResponse{Content: bigAnswer},
				provider.CompletionResponse{Content: "There are {{plugin_output.total}} items."})
			llm.onCall = func(call int, req *provider.CompletionRequest) error {
				switch call {
				case 2:
					orch.contextWindow = tc.window(req)
				case 3:
					orch.contextWindow = 0
				}
				return nil
			}
			for turn := 1; turn <= 3; turn++ {
				announceTurn(t, context.Background(), orch, fmt.Sprintf("turn %d", turn))
			}
			if len(llm.seen) != 5 {
				t.Fatalf("requests = %d, want 5 (load, answer, the turn 2 request, its retry, turn 3)", len(llm.seen))
			}
			retry := llm.seen[3].messages
			var kept []string
			for _, m := range retry[1 : len(retry)-1] {
				if m.Role == provider.RoleUser {
					kept = append(kept, m.Content)
				}
			}
			hint := retry[len(retry)-1]
			if !slices.Equal(kept, tc.kept) || hint.Role != provider.RoleUser || !strings.HasPrefix(hint.Content, "[system]") {
				t.Fatalf("retry request keeps user messages %q and ends with %s %q; want %q and the retry hint", kept, hint.Role, hint.Content, tc.kept)
			}
			checkAnnounced(t, llm.seen)
			checkRecordsKept(t, llm.seen)
			if got, want := recordsOf(retry), []string{"user " + hint.Content + loaded}; !reflect.DeepEqual(got, want) {
				t.Errorf("records in the retry = %q, want %q", got, want)
			}
			if got, want := storedRecords(t, sessions), []string{"tool l1" + loaded}; !reflect.DeepEqual(got, want) {
				t.Errorf("stored records = %q, want %q", got, want)
			}
		})
	}
}

type lostCallStore struct {
	SessionStoreInterface
	lose func(provider.Message) bool
}

func (s *lostCallStore) AddMessage(id string, msg provider.Message) error {
	return s.AddMessageWithMetadata(id, msg, nil)
}

func (s *lostCallStore) AddMessageWithMetadata(id string, msg provider.Message, metadata map[string]string) error {
	if s.lose(msg) {
		return errors.New("simulated insert failure")
	}
	return s.SessionStoreInterface.AddMessageWithMetadata(id, msg, metadata)
}

func TestRun_RecordGoesToTheStoredMessageTheRequestCarries(t *testing.T) {
	sessions := &lostCallStore{SessionStoreInterface: state.NewSessionStore(""), lose: func(m provider.Message) bool {
		return len(m.ToolCalls) > 0 && m.ToolCalls[0].Arguments["names"] == "p__other"
	}}
	orch, llm := announceOrchOn(t, sessions, OrchestratorOpts{}, &countingExecutor{}, announceActions(),
		loadCall("c1", "p__catalog"), loadCall("c1", "p__other"))
	announceTurn(t, context.Background(), orch, "turn 1")
	announceTurn(t, context.Background(), orch, "turn 2")
	if len(llm.seen) != 4 {
		t.Fatalf("requests = %d, want 4", len(llm.seen))
	}
	sess, err := sessions.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, m := range sess.Messages {
		if m.Role == provider.RoleTool && m.ToolCallID == "c1" {
			results++
		}
	}
	if last := llm.seen[2].messages; results != 2 || len(last) != 4 || last[3].Role != provider.RoleTool {
		t.Fatalf("want two stored c1 results and the later one dropped from request 3; got %d results, request 3 %v", results, recordsOf(last))
	}
	checkAnnounced(t, llm.seen, 2)
	checkRecordsKept(t, llm.seen)
	loaded := "tool c1 +p__catalog +p__list +p__lookup"
	if got, want := recordsOf(llm.seen[2].messages), []string{loaded}; !reflect.DeepEqual(got, want) {
		t.Errorf("records in request 3 = %q, want %q", got, want)
	}
	want := []string{loaded, "user turn 2 +p__other"}
	if got := recordsOf(llm.seen[3].messages); !reflect.DeepEqual(got, want) {
		t.Errorf("records in request 4 = %q, want %q", got, want)
	}
	if got := storedRecords(t, sessions); !reflect.DeepEqual(got, want) {
		t.Errorf("stored records = %q, want %q", got, want)
	}
}
