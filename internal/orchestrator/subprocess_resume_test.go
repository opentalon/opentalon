package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
)

var subOn = SubprocessConfig{Enabled: true, MaxDepth: 2}

var resumeThinking = []json.RawMessage{json.RawMessage(`{"type":"thinking","thinking":"delegate","signature":"sig"}`)}

func (h *pauseHarness) nextPod(t *testing.T, sub SubprocessConfig, slow map[string]time.Duration) *Orchestrator {
	t.Helper()
	registry := NewToolRegistry()
	for _, cap := range h.orch.registry.ListCapabilities() {
		if strings.HasPrefix(cap.Name, "_") {
			continue // the orchestrator's own, registered by NewWithRules
		}
		exec, _ := h.orch.registry.GetExecutor(cap.Name)
		if d := slow[cap.Name]; d > 0 {
			inner := exec
			exec = executorFunc(func(ctx context.Context, call ToolCall) ToolResult {
				select {
				case <-time.After(d):
					return inner.Execute(ctx, call)
				case <-ctx.Done():
					return ToolResult{CallID: call.ID, Error: ctx.Err().Error()}
				}
			})
		}
		if err := registry.Register(cap, exec); err != nil {
			t.Fatalf("register %s: %v", cap.Name, err)
		}
	}
	return NewWithRules(h.llm, DefaultParser, registry, state.NewMemoryStore(""), h.sessions, OrchestratorOpts{
		EventSink:                     h.sink,
		PluginCallObserver:            h.observer,
		ConfirmationPlugin:            "conf",
		ConfirmationAction:            "check",
		ConfirmationClassifierEnabled: true,
		Subprocess:                    sub,
	})
}

func pausedHarness(t *testing.T, sessions SessionStoreInterface, sub SubprocessConfig, slow map[string]time.Duration, more ...provider.CompletionResponse) *pauseHarness {
	t.Helper()
	h := newPauseHarness(t, sessions, pauseTask)
	h.orch = h.nextPod(t, sub, slow)
	h.llm.parent = []provider.CompletionResponse{
		callReply("p1", toolFQN(metaPluginName, metaLoadTools), map[string]string{"names": "_subprocess__run"}),
		{Content: "Delegating.", Thinking: resumeThinking, ThinkingFingerprint: "fp", Usage: provider.Usage{InputTokens: 6, OutputTokens: 2},
			ToolCalls: []provider.ToolCall{{ID: "p2", Name: "_subprocess__run", Arguments: map[string]string{"task": pauseTask}}}},
	}
	child := h.llm.children[pauseTask]
	child[1].Usage = provider.Usage{InputTokens: 10, OutputTokens: 4}
	h.llm.children[pauseTask] = append(child, more...)
	if res, err := h.orch.Run(context.Background(), "s1", "fix record 7"); err != nil || res.Response != pauseQuestion {
		t.Fatalf("Run: %v %+v, want the question", err, res)
	}
	if h.write.count != 0 {
		t.Fatalf("store__write ran %d time(s) before any approval", h.write.count)
	}
	return h
}

func TestSubprocessAnswerToItsQuestion(t *testing.T) {
	const stopped = "\n\nTask 1:\n- did not run: store__write(id=7)\n- ran: look__see()"
	for _, tc := range []struct {
		name, decision, reply string
		sub                   SubprocessConfig
		parent                []provider.CompletionResponse
		response, result      string
		writes, requests      int
		observed              []string
	}{
		{"approval", "approve", "approve", subOn, []provider.CompletionResponse{
			{ToolCalls: []provider.ToolCall{{ID: "p4", Name: "_subprocess__run", Arguments: map[string]string{"task": pauseTask}}}},
			{Content: "Record 7 updated."},
		}, "Record 7 updated.", "done", 1, 3, []string{"store false 5/2", "_subprocess false 6/2"}},
		{"rejection", "reject", "reject", subOn, nil,
			confirmationRejectedReply, "the user rejected its call." + stopped, 0, 2, []string{"_subprocess true 6/2"}},
		{"another reply", "", "use record 8", subOn, []provider.CompletionResponse{
			{Content: `{"decision":"amend","reason":"another record"}`}, // the classifier's
			{Content: "Shall I update record 8?"},
		}, "Shall I update record 8?", "without approving the call." + stopped, 0, 2, []string{"_subprocess true 6/2"}},
		{"approval with sub-agents switched off", "approve", "approve", SubprocessConfig{}, []provider.CompletionResponse{
			{Content: "Sub-agents are off."},
		}, "Sub-agents are off.", "sub-agents are switched off." + stopped, 0, 2, []string{"_subprocess true 6/2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, kind storeKind) {
				h := pausedHarness(t, kind.open(t), subOn, nil)
				h.llm.parent = tc.parent
				ctx := context.Background()
				if tc.decision != "" {
					ctx = actor.WithConfirmationDecision(ctx, tc.decision)
				}

				res, err := h.nextPod(t, tc.sub, nil).Run(ctx, "s1", tc.reply)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if res.Response != tc.response {
					t.Errorf("turn answered %q %v, want %q", res.Response, res.Metadata, tc.response)
				}
				if h.write.count != tc.writes {
					t.Errorf("store__write ran %d time(s), want %d", h.write.count, tc.writes)
				}
				if n := len(eventsOfCall(t, h.sink, events.TypeConfirmationResolved, "c1")); n != 1 {
					t.Errorf("confirmation_resolved for c1 = %d, want 1", n)
				}
				if n := len(eventsOfCall(t, h.sink, events.TypeToolCallResult, "p2")); n != 1 {
					t.Errorf("tool_call_result events for p2 = %d, want 1 over the pause and its end", n)
				}
				var observed []string
				for _, c := range h.observer.calls {
					if c.plugin == "_subprocess" || c.plugin == "store" {
						observed = append(observed, fmt.Sprintf("%s %v %d/%d", c.plugin, c.failed, c.inputTokens, c.outputTokens))
					}
				}
				if !slices.Equal(observed, tc.observed) {
					t.Errorf("observed %q, want %q", observed, tc.observed)
				}

				s := h.session(t)
				var calls, results int
				var result string
				for _, m := range s.Messages {
					if m.Role == provider.RoleAssistant && mentionsCall(m, "p2") {
						calls++
						if m.Content != "Delegating." || !reflect.DeepEqual(m.Thinking, resumeThinking) || m.ThinkingFingerprint != "fp" {
							t.Errorf("outer call message %+v, want the reply text and thinking of its response", m)
						}
					}
					if m.ToolCallID == "p2" {
						results++
						result = m.Content
					}
				}
				if calls != 1 || results != 1 || !strings.Contains(result, tc.result) {
					t.Errorf("parent history holds the outer call %d time(s) and its result %d time(s) (%q); want once each, with %q", calls, results, result, tc.result)
				}
				if p := s.Metadata["pending_tool_call"]; p != "" {
					t.Errorf("pending record left: %.200s", p)
				}

				if n := len(eventsOfCall(t, h.sink, events.TypeToolCallExtracted, "p4")); n != 0 {
					t.Errorf("the same _subprocess call right after the resume was not stopped as a repeat")
				}
				h.llm.assertTools(t, tc.requests, pauseTools...)
			})
		})
	}
}

func TestSubprocessResumesOnAnotherPodAndAsksAgain(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		h := pausedHarness(t, kind.open(t), subOn, nil, callReply("c3", "store__write", map[string]string{"id": "8"}))
		first, second := h.orch, h.nextPod(t, subOn, nil)
		approve := actor.WithConfirmationDecision(context.Background(), "approve")
		ran := func(id string) int { return len(eventsOfCall(t, h.sink, events.TypeToolCallResult, id)) }

		res, err := second.Run(approve, "s1", "approve")
		if err != nil || res.Response != pauseQuestion || res.Metadata["tool_call_id"] != "p2-0-c3" {
			t.Fatalf("first approval: %v %q %v, want the question about c3", err, res.Response, res.Metadata)
		}
		if h.write.count != 1 || ran("c1") != 1 || ran("c3") != 0 || ran("p2") != 0 {
			t.Errorf("after the first approval: %d write(s), c1 %d, c3 %d, p2 %d; want c1 alone", h.write.count, ran("c1"), ran("c3"), ran("p2"))
		}

		h.llm.parent = []provider.CompletionResponse{{Content: "Records 7 and 8 updated."}}
		if res, err = first.Run(approve, "s1", "approve"); err != nil || res.Response != "Records 7 and 8 updated." {
			t.Fatalf("second approval: %v %+v", err, res)
		}
		if h.write.count != 2 || ran("c1") != 1 || ran("c3") != 1 || ran("p2") != 1 {
			t.Errorf("after the second approval: %d write(s), c1 %d, c3 %d, p2 %d; want one each", h.write.count, ran("c1"), ran("c3"), ran("p2"))
		}

		messages := len(h.session(t).Messages)
		if res, err = second.Run(approve, "s1", "approve"); err != nil || res.Metadata["action"] != "confirmation_expired" {
			t.Fatalf("late approval: %v %+v, want confirmation_expired", err, res)
		}
		if h.write.count != 2 || len(h.session(t).Messages) != messages {
			t.Errorf("late approval changed something: %d write(s), %d messages (was %d)", h.write.count, len(h.session(t).Messages), messages)
		}
		h.llm.assertTools(t, 4, pauseTools...)
	})
}

// c0 uses 70 ms of the 120 ms, so about 50 ms are left at the pause: the approved 70 ms call runs, the next 70 ms call is cut.
func TestSubprocessResumeKeepsItsRunningTime(t *testing.T) {
	sub := SubprocessConfig{Enabled: true, MaxDepth: 2, DefaultTimeout: 120 * time.Millisecond}
	slow := map[string]time.Duration{"look": 70 * time.Millisecond, "store": 70 * time.Millisecond}
	h := pausedHarness(t, state.NewSessionStore(""), sub, slow, callReply("c3", "look__see", map[string]string{}))

	if _, err := h.orch.Run(actor.WithConfirmationDecision(context.Background(), "approve"), "s1", "approve"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.write.count != 1 {
		t.Errorf("the approved store__write ran %d time(s), want once, under its own deadline", h.write.count)
	}
	var c3 string
	for _, m := range h.llm.requests[len(h.llm.requests)-1].Messages {
		if m.ToolCallID == "c3" {
			c3 = m.Content
		}
	}
	if !strings.Contains(c3, "timed out") {
		t.Errorf("c3 after the resume got %q, want it cut by the remaining running time", c3)
	}
	h.llm.assertTools(t, 4, pauseTools...)
}

type clearFailingStore struct {
	SessionStoreInterface
	n int
}

func (s *clearFailingStore) SetMetadata(id, key, value string) error {
	if key == "pending_tool_call" && value == "" && s.n > 0 {
		s.n--
		return errors.New("metadata clear failed")
	}
	return s.SessionStoreInterface.SetMetadata(id, key, value)
}

func TestSubprocessAnswerThatCannotClearTheRecordLeavesItPaused(t *testing.T) {
	store := &clearFailingStore{SessionStoreInterface: state.NewSessionStore("")}
	h := pausedHarness(t, store, subOn, nil)
	reject := actor.WithConfirmationDecision(context.Background(), "reject")
	ended := func() (results, calls int) {
		for _, m := range h.session(t).Messages {
			if m.Role == provider.RoleAssistant && mentionsCall(m, "p2") {
				calls++
			}
		}
		return len(eventsOfCall(t, h.sink, events.TypeToolCallResult, "p2")), calls
	}

	store.n = 2
	if _, err := h.orch.Run(reject, "s1", "reject"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results, calls := ended(); results != 0 || calls != 0 || h.session(t).Metadata["pending_tool_call"] == "" || h.write.count != 0 {
		t.Errorf("uncleared answer: %d result event(s), %d outer call(s) in history, %d write(s); want none, and the record kept",
			results, calls, h.write.count)
	}
	if _, err := h.orch.Run(reject, "s1", "reject"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if results, calls := ended(); results != 1 || calls != 1 || h.write.count != 0 {
		t.Errorf("after the answer that clears the record: %d result event(s), %d outer call(s), %d write(s); want one each and no write",
			results, calls, h.write.count)
	}
	h.llm.assertTools(t, 2, pauseTools...)
}
