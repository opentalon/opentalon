package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

const parallelTasks = `[{"task":"` + pauseTask + `"},{"task":"update record 8"},{"task":"count records"}]`

var writeRecord8 = callOf("d1", "store__write", map[string]string{"id": "8"})

func parallelPaused(t *testing.T, sessions SessionStoreInterface, second provider.CompletionResponse, extra ...registeredPlugin) *pauseHarness {
	t.Helper()
	h := newPauseHarness(t, sessions, pauseTask)
	for _, p := range extra {
		if err := h.orch.registry.Register(p.cap, p.exec); err != nil {
			t.Fatalf("register %s: %v", p.cap.Name, err)
		}
	}
	h.orch = h.nextPod(t, subOn, nil)
	h.llm.parent = []provider.CompletionResponse{
		callOf("p1", toolFQN(metaPluginName, metaLoadTools), map[string]string{"names": "_subprocess__parallel"}),
		callOf("p2", "_subprocess__parallel", map[string]string{"tasks": parallelTasks}),
	}
	h.llm.children = map[string][]provider.CompletionResponse{
		pauseTask: {
			callOf("c0", "look__see", map[string]string{}),
			callOf("c1", "store__write", map[string]string{"id": "7"}),
			{Content: "record 7 updated"},
		},
		"update record 8": {second, {Content: "record 8 updated"}},
		"count records":   {{Content: "3 records"}},
	}
	if res, err := h.orch.Run(context.Background(), "s1", "fix records 7 and 8"); err != nil || res.Metadata["tool_call_id"] != "p2-0-c1" {
		t.Fatalf("Run: %v %q %v, want the question about c1", err, res.Response, res.Metadata)
	}
	if h.write.count != 0 {
		t.Fatalf("store__write ran %d time(s) before any approval", h.write.count)
	}
	return h
}

func (h *pauseHarness) pausedChildren(t *testing.T) (int, []string) {
	t.Helper()
	var rec struct {
		Subprocess struct {
			Children []struct {
				Position int `json:"position"`
				Waiting  struct {
					ID string `json:"id"`
				} `json:"waiting"`
				Result *struct {
					Response string `json:"response"`
				} `json:"result"`
			} `json:"children"`
			Asking int `json:"asking"`
		} `json:"subprocess"`
	}
	if err := json.Unmarshal([]byte(h.session(t).Metadata["pending_tool_call"]), &rec); err != nil {
		t.Fatalf("pending record: %v", err)
	}
	var children []string
	for _, c := range rec.Subprocess.Children {
		if c.Result != nil {
			children = append(children, fmt.Sprintf("%d answered %s", c.Position, c.Result.Response))
		} else {
			children = append(children, fmt.Sprintf("%d waits %s", c.Position, c.Waiting.ID))
		}
	}
	return rec.Subprocess.Asking, children
}

func (h *pauseHarness) outerCall(t *testing.T) (calls int, results []string) {
	t.Helper()
	for _, m := range h.session(t).Messages {
		if m.Role == provider.RoleAssistant && mentionsCall(m, "p2") {
			calls++
		}
		if m.ToolCallID == "p2" {
			results = append(results, m.Content)
		}
	}
	return calls, results
}

func TestSubprocessParallelAsksOneChildPerTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		h := parallelPaused(t, kind.open(t), writeRecord8)
		approve := actor.WithConfirmationDecision(context.Background(), "approve")
		ran := func(id string) int { return len(eventsOfCall(t, h.sink, events.TypeToolCallResult, id)) }
		asked := func(id string) int { return len(eventsOfCall(t, h.sink, events.TypeConfirmationRequested, id)) }
		if asking, children := h.pausedChildren(t); asking != 0 || !slices.Equal(children, []string{"0 waits c1", "1 waits d1", "2 answered 3 records"}) {
			t.Errorf("record asks about child %d of %q", asking, children)
		}

		res, err := h.nextPod(t, subOn, nil).Run(approve, "s1", "approve")
		if err != nil || res.Response != pauseQuestion || res.Metadata["tool_call_id"] != "p2-1-d1" {
			t.Fatalf("first approval: %v %q %v, want the question about d1", err, res.Response, res.Metadata)
		}
		if h.write.count != 1 || ran("c1") != 1 || ran("d1") != 0 || ran("p2") != 0 {
			t.Errorf("after the first approval: %d write(s), c1 %d, d1 %d, p2 %d; want c1 alone", h.write.count, ran("c1"), ran("d1"), ran("p2"))
		}
		if asking, children := h.pausedChildren(t); asking != 1 || !slices.Equal(children, []string{"0 answered record 7 updated", "1 waits d1", "2 answered 3 records"}) {
			t.Errorf("record asks about child %d of %q", asking, children)
		}
		if calls, results := h.outerCall(t); calls != 0 || len(results) != 0 {
			t.Errorf("parent history holds p2 %d time(s) and %d result(s) while a child waits", calls, len(results))
		}

		h.llm.parent = []provider.CompletionResponse{{Content: "Records 7 and 8 updated."}}
		if res, err = h.orch.Run(approve, "s1", "approve"); err != nil || res.Response != "Records 7 and 8 updated." {
			t.Fatalf("second approval: %v %+v", err, res)
		}
		if h.write.count != 2 || ran("c1") != 1 || ran("d1") != 1 || ran("p2") != 1 {
			t.Errorf("after the second approval: %d write(s), c1 %d, d1 %d, p2 %d; want one each", h.write.count, ran("c1"), ran("d1"), ran("p2"))
		}
		joined := "## Task 1: update record 7\nrecord 7 updated\n\n## Task 2: update record 8\nrecord 8 updated\n\n## Task 3: count records\n3 records" +
			"\n\n[structured]\n" + `[{"task":"update record 7","response":"record 7 updated","iterations":3},` +
			`{"task":"update record 8","response":"record 8 updated","iterations":2},{"task":"count records","response":"3 records","iterations":1}]` +
			"\n[/structured]"
		if calls, results := h.outerCall(t); calls != 1 || !slices.Equal(results, []string{joined}) {
			t.Errorf("parent history holds p2 %d time(s), results %q; want once, with\n%s", calls, results, joined)
		}
		if asked("c1") != 1 || asked("d1") != 1 || countEventType(h.sink.snapshot(), events.TypeConfirmationRequested) != 2 {
			t.Errorf("confirmation_requested: c1 %d, d1 %d, of %d; want one each and no other", asked("c1"), asked("d1"),
				countEventType(h.sink.snapshot(), events.TypeConfirmationRequested))
		}
		if h.orch.registry.IsActionReadOnly("_subprocess", "parallel") {
			t.Errorf("_subprocess__parallel is read-only; it must stay a write for the phantom-completion guard")
		}
		h.llm.assertTools(t, 6, pauseTools...)
	})
}

func TestSubprocessParallelStopsEveryWaitingChild(t *testing.T) {
	const first = "\n\nTask 1:\n- did not run: store__write(id=7)\n- ran: look__see()" +
		"\n\nTask 2:\n- did not run (not asked about yet): store__write(id=8)\n\nTask 3:\n- answer: 3 records"
	const second = "\n\nTask 1:\n- ran: look__see()\n- ran: store__write(id=7)\n- answer: record 7 updated" +
		"\n\nTask 2:\n- did not run: store__write(id=8)\n\nTask 3:\n- answer: 3 records"
	for _, tc := range []struct {
		name             string
		approved         bool
		decision, reply  string
		parent           []provider.CompletionResponse
		response, result string
		writes, requests int
	}{
		{"rejection of the first", false, "reject", "reject", nil, confirmationRejectedReply, "the user rejected its call." + first, 0, 4},
		{"rejection of the second", true, "reject", "reject", nil, confirmationRejectedReply, "the user rejected its call." + second, 1, 5},
		{"another reply to the second", true, "", "use record 9", []provider.CompletionResponse{
			{Content: `{"decision":"amend","reason":"another record"}`}, // the classifier's
			{Content: "Shall I update record 9?"},
		}, "Shall I update record 9?", "without approving the call." + second, 1, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := parallelPaused(t, state.NewSessionStore(""), writeRecord8)
			if tc.approved {
				res, err := h.orch.Run(actor.WithConfirmationDecision(context.Background(), "approve"), "s1", "approve")
				if err != nil || res.Metadata["tool_call_id"] != "p2-1-d1" {
					t.Fatalf("first approval: %v %q %v, want the question about d1", err, res.Response, res.Metadata)
				}
			}
			h.llm.parent = tc.parent
			ctx := context.Background()
			if tc.decision != "" {
				ctx = actor.WithConfirmationDecision(ctx, tc.decision)
			}

			res, err := h.orch.Run(ctx, "s1", tc.reply)
			if err != nil || res.Response != tc.response {
				t.Fatalf("Run: %v %q %v, want %q", err, res.Response, res.Metadata, tc.response)
			}
			if h.write.count != tc.writes {
				t.Errorf("store__write ran %d time(s), want %d", h.write.count, tc.writes)
			}
			if calls, results := h.outerCall(t); calls != 1 || len(results) != 1 || !strings.Contains(results[0], tc.result) {
				t.Errorf("parent history holds p2 %d time(s), results %q; want once, with %q", calls, results, tc.result)
			}
			if n := len(eventsOfCall(t, h.sink, events.TypeToolCallResult, "p2")); n != 1 {
				t.Errorf("tool_call_result events for p2 = %d, want 1", n)
			}
			if p := h.session(t).Metadata["pending_tool_call"]; p != "" {
				t.Errorf("pending record left: %.200s", p)
			}
			h.llm.assertTools(t, tc.requests, pauseTools...)
		})
	}
}

func TestSubprocessParallelDecidesTheNextChildsCallAgain(t *testing.T) {
	desk := &checkingExecutor{}
	desk.check = func(context.Context, ToolCall) (CheckResult, error) {
		if len(desk.checked()) > 1 {
			return CheckResult{Verdict: CheckRefused, Kind: "already_exists", Message: "desk 3 is taken"}, nil
		}
		return CheckResult{Verdict: CheckPassed}, nil
	}
	h := parallelPaused(t, state.NewSessionStore(""), callOf("d1", "desk__book", map[string]string{"id": "3"}),
		registeredPlugin{PluginCapability{Name: "desk", Actions: []Action{{Name: "book", Description: "Book a desk.", Check: true,
			Parameters: []Parameter{{Name: "id", Description: "Desk id."}}}}}, desk})
	h.llm.parent = []provider.CompletionResponse{{Content: "Record 7 updated; desk 3 is taken."}}

	res, err := h.orch.Run(actor.WithConfirmationDecision(context.Background(), "approve"), "s1", "approve")
	if err != nil || res.Response != "Record 7 updated; desk 3 is taken." {
		t.Fatalf("approval: %v %q %v, want the parent's answer", err, res.Response, res.Metadata)
	}
	if checks, runs := len(desk.checked()), len(desk.snapshot()); checks != 2 || runs != 0 || h.write.count != 1 {
		t.Errorf("desk__book checked %d time(s), ran %d, store__write ran %d; want checked again after the wait, never run, and c1 once",
			checks, runs, h.write.count)
	}
	if n := countEventType(h.sink.snapshot(), events.TypeConfirmationRequested); n != 1 {
		t.Errorf("confirmation_requested events = %d, want c1's alone", n)
	}
	var d1 string
	for _, req := range h.llm.requests {
		for _, m := range req.Messages {
			if m.ToolCallID == "d1" {
				d1 = m.Content
			}
		}
	}
	if d1 != "error: desk 3 is taken" {
		t.Errorf("the second child got %q for d1, want the check's refusal", d1)
	}
	if calls, results := h.outerCall(t); calls != 1 || len(results) != 1 || !strings.Contains(results[0], "## Task 2: update record 8\nrecord 8 updated") {
		t.Errorf("parent history holds p2 %d time(s), results %q; want once, with every answer", calls, results)
	}
	if p := h.session(t).Metadata["pending_tool_call"]; p != "" {
		t.Errorf("pending record left: %.200s", p)
	}
	h.llm.assertTools(t, 6, "conf__check", "desk__book", "look__see", "store__write")
}
