package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

const pauseTask = "update record 7"

var pauseTools = []string{"conf__check", "look__see", "store__write"}

func pauseChild(task string) map[string][]provider.CompletionResponse {
	return map[string][]provider.CompletionResponse{task: {
		callReply("c0", "look__see", map[string]string{}),
		{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "store__write", Arguments: map[string]string{"id": "7"}},
			{ID: "c2", Name: "look__see", Arguments: map[string]string{}},
		}},
	}}
}

func pauseParent(task string) []provider.CompletionResponse {
	return []provider.CompletionResponse{
		{ToolCalls: []provider.ToolCall{
			{ID: "p1", Name: toolFQN(metaPluginName, metaLoadTools), Arguments: map[string]string{"names": "_subprocess__run"}},
			{ID: "p0", Name: "look__see", Arguments: map[string]string{}},
		}},
		{Content: "Delegating.", ToolCalls: []provider.ToolCall{
			{ID: "p2", Name: "_subprocess__run", Arguments: map[string]string{"task": task}},
			{ID: "p3", Name: "look__see", Arguments: map[string]string{}},
		}},
		{Content: "The sub-agent was stopped."},
	}
}

type narratingLLM struct {
	*subagentLLM
	mu         sync.Mutex
	narrations []string
	fail       bool
}

const pauseQuestion = "A sub-agent working on updating record 7 asks: may it write the record?"

func (l *narratingLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	if len(req.Messages) == 2 && req.Messages[0].Role == provider.RoleSystem &&
		strings.HasPrefix(req.Messages[0].Content, confirmationNarratePrompt) {
		l.mu.Lock()
		l.narrations = append(l.narrations, req.Messages[1].Content)
		l.mu.Unlock()
		if l.fail {
			return nil, errors.New("narration failed")
		}
		return &provider.CompletionResponse{Content: pauseQuestion}, nil
	}
	return l.subagentLLM.Complete(ctx, req)
}

type pendingWriteCounter struct {
	SessionStoreInterface
	writes atomic.Int32
}

func (s *pendingWriteCounter) SetMetadata(id, key, value string) error {
	if key == "pending_tool_call" && value != "" {
		s.writes.Add(1)
	}
	return s.SessionStoreInterface.SetMetadata(id, key, value)
}

type pendingFailingStore struct{ SessionStoreInterface }

func (s pendingFailingStore) SetMetadata(id, key, value string) error {
	if key == "pending_tool_call" && value != "" {
		return errors.New("metadata write failed")
	}
	return s.SessionStoreInterface.SetMetadata(id, key, value)
}

type pauseHarness struct {
	orch     *Orchestrator
	llm      *narratingLLM
	sessions SessionStoreInterface
	write    *countingExecutor
	looks    *countingExecutor
	sink     *recordingEventSink
	observer *fakeObserver
}

func newPauseHarness(t *testing.T, sessions SessionStoreInterface, task string) *pauseHarness {
	t.Helper()
	h := &pauseHarness{
		llm:      &narratingLLM{subagentLLM: &subagentLLM{parent: pauseParent(task), children: pauseChild(task)}},
		sessions: sessions,
		write:    &countingExecutor{},
		looks:    &countingExecutor{},
		sink:     &recordingEventSink{},
		observer: &fakeObserver{},
	}
	look := executorFunc(func(ctx context.Context, call ToolCall) ToolResult {
		h.looks.Execute(ctx, call)
		return ToolResult{CallID: call.ID, Content: "looked " + call.ID}
	})
	registry := NewToolRegistry()
	for _, p := range []registeredPlugin{
		{PluginCapability{Name: "store", Actions: []Action{{Name: "write", Description: "Write a record.",
			Parameters: []Parameter{{Name: "id", Description: "Record id."}}}}}, h.write},
		{PluginCapability{Name: "look", Actions: []Action{{Name: "see", Description: "Look.", ReadOnly: true, AlwaysInclude: true}}}, look},
		{PluginCapability{Name: "conf", Actions: []Action{{Name: "check"}}}, confirmingExecutor{}},
	} {
		if err := registry.Register(p.cap, p.exec); err != nil {
			t.Fatalf("register %s: %v", p.cap.Name, err)
		}
	}
	sessions.Create(state.SessionParams{ID: "s1"})
	h.orch = NewWithRules(h.llm, DefaultParser, registry, state.NewMemoryStore(""), sessions, OrchestratorOpts{
		EventSink:          h.sink,
		PluginCallObserver: h.observer,
		ConfirmationPlugin: "conf",
		ConfirmationAction: "check",
		Subprocess:         SubprocessConfig{Enabled: true, MaxDepth: 2},
	})
	return h
}

func (h *pauseHarness) session(t *testing.T) *state.Session {
	t.Helper()
	s, err := h.sessions.Get("s1")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return s
}

type storedPause struct {
	ID                      string            `json:"id"`
	Plugin                  string            `json:"plugin"`
	Action                  string            `json:"action"`
	Args                    map[string]string `json:"args"`
	Prompt                  string            `json:"prompt"`
	ConfirmationRequestedID string            `json:"confirmation_requested_id"`
	Subprocess              *struct {
		Call      continuationCallJSON `json:"call"`
		ReplyText string               `json:"reply_text"`
		Children  []struct {
			Position int `json:"position"`
			Request  struct {
				Task string `json:"task"`
			} `json:"request"`
			Depth      int                    `json:"depth"`
			Messages   []provider.Message     `json:"messages"`
			Iterations int                    `json:"iterations"`
			Remaining  time.Duration          `json:"remaining"`
			Ran        []continuationCallJSON `json:"ran"`
			Waiting    continuationCallJSON   `json:"waiting"`
		} `json:"children"`
		Asking int `json:"asking"`
	} `json:"subprocess"`
}

type continuationCallJSON struct {
	ID     string            `json:"id"`
	Plugin string            `json:"plugin"`
	Action string            `json:"action"`
	Args   map[string]string `json:"args"`
}

func eventsOfCall(t *testing.T, sink *recordingEventSink, typ, callID string) []emit.Event {
	t.Helper()
	var out []emit.Event
	for _, e := range sink.snapshot() {
		if e.EventType != typ {
			continue
		}
		var p struct {
			CallID     string `json:"call_id"`
			ToolCallID string `json:"tool_call_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", typ, err)
		}
		if p.CallID == callID || p.ToolCallID == callID {
			out = append(out, e)
		}
	}
	return out
}

func mentionsCall(m provider.Message, id string) bool {
	if m.ToolCallID == id {
		return true
	}
	for _, tc := range m.ToolCalls {
		if tc.ID == id {
			return true
		}
	}
	return false
}

func TestSubprocessGatedCallPausesTheTurn(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		sessions := &pendingWriteCounter{SessionStoreInterface: kind.open(t)}
		h := newPauseHarness(t, sessions, pauseTask)

		res, err := h.orch.Run(context.Background(), "s1", "fix record 7")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Response != pauseQuestion || res.Metadata["prompt_type"] != provider.PromptTypeToolConfirmation ||
			res.Metadata["tool_call_id"] != "p2-0-c1" {
			t.Fatalf("turn ended with %q %v, want the question with frame id p2-0-c1", res.Response, res.Metadata)
		}
		if h.write.count != 0 || h.looks.count != 2 {
			t.Errorf("store__write ran %d time(s), look__see %d; want 0 and 2 (p0, c0)", h.write.count, h.looks.count)
		}

		if len(h.llm.narrations) != 1 {
			t.Fatalf("narrations = %d, want 1", len(h.llm.narrations))
		}
		n := h.llm.narrations[0]
		for _, want := range []string{pauseTask, "sub-agent", `"write"`, "looked c0"} {
			if !strings.Contains(n, want) {
				t.Errorf("narration request lacks %q:\n%s", want, n)
			}
		}
		if strings.Contains(n, "looked p0") {
			t.Errorf("narration request quotes the parent's messages:\n%s", n)
		}

		s := h.session(t)
		var rec storedPause
		if err := json.Unmarshal([]byte(s.Metadata["pending_tool_call"]), &rec); err != nil {
			t.Fatalf("pending record %q: %v", s.Metadata["pending_tool_call"], err)
		}
		requested := eventsOfCall(t, h.sink, events.TypeConfirmationRequested, "c1")
		if len(requested) != 1 || countEventType(h.sink.snapshot(), events.TypeConfirmationRequested) != 1 {
			t.Fatalf("confirmation_requested for c1 = %d, want the turn's only one", len(requested))
		}
		if h.orch.registry.IsActionReadOnly("_subprocess", "run") {
			t.Errorf("_subprocess__run is read-only; it must stay a write for the phantom-completion guard")
		}
		if rec.ID != "" || rec.Plugin != "" || rec.Action != "" || rec.Args != nil {
			t.Errorf("record holds a runnable call: %+v", rec)
		}
		if rec.Prompt != pauseQuestion || rec.ConfirmationRequestedID == "" || rec.ConfirmationRequestedID != requested[0].ID {
			t.Errorf("record prompt %q, event id %q; want the question and %q", rec.Prompt, rec.ConfirmationRequestedID, requested[0].ID)
		}
		if n := sessions.writes.Load(); n != 1 {
			t.Errorf("pending record written %d time(s), want once, with the event's id", n)
		}
		c := rec.Subprocess
		if c == nil || len(c.Children) != 1 {
			t.Fatalf("record holds no sub-agent: %s", s.Metadata["pending_tool_call"])
		}
		if c.Call.ID != "p2" || c.Call.Plugin != "_subprocess" || c.Call.Action != "run" || c.Call.Args["task"] != pauseTask ||
			c.ReplyText != "Delegating." || c.Asking != 0 {
			t.Errorf("outer call %+v, reply %q, asking %d", c.Call, c.ReplyText, c.Asking)
		}
		child := c.Children[0]
		if child.Position != 0 || child.Request.Task != pauseTask || child.Depth != 1 || child.Iterations != 2 {
			t.Errorf("child position %d task %q depth %d iterations %d", child.Position, child.Request.Task, child.Depth, child.Iterations)
		}
		if child.Remaining <= 0 || child.Remaining > 60*time.Second {
			t.Errorf("remaining running time = %s, want within the 60s timeout", child.Remaining)
		}
		if child.Waiting.ID != "c1" || child.Waiting.Plugin != "store" || child.Waiting.Action != "write" || child.Waiting.Args["id"] != "7" {
			t.Errorf("waiting call = %+v, want c1 store__write id=7", child.Waiting)
		}
		if len(child.Ran) != 1 || child.Ran[0].ID != "c0" {
			t.Errorf("ran = %+v, want [c0]", child.Ran)
		}
		var later bool
		for _, m := range child.Messages {
			if mentionsCall(m, "c1") {
				t.Errorf("child messages hold the waiting call: %+v", m)
			}
			if m.ToolCallID == "c2" && strings.Contains(m.Content, "NOT EXECUTED") {
				later = true
			}
		}
		if !later {
			t.Errorf("c2 not recorded as not executed in the child's messages: %+v", child.Messages)
		}

		var p3, question bool
		for _, m := range s.Messages {
			for _, id := range []string{"p2", "c0", "c1", "c2"} {
				if mentionsCall(m, id) {
					t.Errorf("parent history holds %s: %+v", id, m)
				}
			}
			if m.Content == "Delegating." {
				t.Errorf("parent history holds the outer call's reply text")
			}
			if m.ToolCallID == "p3" && strings.Contains(m.Content, "NOT EXECUTED") {
				p3 = true
			}
			question = question || (m.PromptType == provider.PromptTypeToolConfirmation && m.Content == pauseQuestion)
		}
		if !p3 || !question {
			t.Errorf("p3 recorded as not executed: %v, question stored: %v", p3, question)
		}

		if n := len(eventsOfCall(t, h.sink, events.TypeToolCallResult, "p2")); n != 0 {
			t.Errorf("tool_call_result events for p2 = %d, want none at the pause", n)
		}
		for _, call := range h.observer.calls {
			if call.plugin == "_subprocess" {
				t.Errorf("outer call observed: %+v", call)
			}
		}

		content, meta, ok := h.orch.PendingConfirmationFrame("s1")
		if !ok || content != pauseQuestion || meta["tool_call_id"] != "p2-0-c1" {
			t.Errorf("pending frame = %q %v %v, want the question with id p2-0-c1", content, meta, ok)
		}
		h.llm.assertTools(t, 2, pauseTools...)
	})
}

func TestSubprocessPauseFallbackQuestionNamesTheTask(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		h := newPauseHarness(t, kind.open(t), pauseTask)
		h.llm.fail = true

		res, err := h.orch.Run(context.Background(), "s1", "fix record 7")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Metadata["tool_call_id"] != "p2-0-c1" {
			t.Fatalf("turn ended with %q %v, want the question with frame id p2-0-c1", res.Response, res.Metadata)
		}
		for _, want := range []string{"sub-agent", pauseTask} {
			if !strings.Contains(res.Response, want) {
				t.Errorf("fallback question lacks %q: %q", want, res.Response)
			}
		}
		if h.write.count != 0 {
			t.Errorf("store__write ran %d time(s), want 0", h.write.count)
		}
		h.llm.assertTools(t, 2, pauseTools...)
	})
}

func TestTaskPreviewKeepsUTF8Valid(t *testing.T) {
	got := taskPreview(strings.Repeat("a", 99) + "äbc")
	if !utf8.ValidString(got) || got != strings.Repeat("a", 99)+"..." {
		t.Errorf("taskPreview = %q, want 99 a and ...", got)
	}
}

func TestSubprocessPauseThatCannotBeStored(t *testing.T) {
	for _, tc := range []struct {
		name string
		task string
		wrap func(SessionStoreInterface) SessionStoreInterface
	}{
		{"store write fails", pauseTask, func(s SessionStoreInterface) SessionStoreInterface { return pendingFailingStore{s} }},
		{"over 1 MiB", pauseTask + strings.Repeat(" and more", 70_000), func(s SessionStoreInterface) SessionStoreInterface { return s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, func(t *testing.T, kind storeKind) {
				h := newPauseHarness(t, tc.wrap(kind.open(t)), tc.task)

				res, err := h.orch.Run(context.Background(), "s1", "fix record 7")
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if res.Response != "The sub-agent was stopped." {
					t.Errorf("turn ended with %q %v, want the parent's answer", res.Response, res.Metadata)
				}
				if h.write.count != 0 {
					t.Errorf("store__write ran %d time(s), want never", h.write.count)
				}
				if n := countEventType(h.sink.snapshot(), events.TypeConfirmationRequested); n != 0 {
					t.Errorf("confirmation_requested events = %d, want none", n)
				}
				s := h.session(t)
				if p := s.Metadata["pending_tool_call"]; p != "" {
					t.Errorf("pending record stored: %.200s", p)
				}
				var stopped string
				for _, m := range s.Messages {
					if m.PromptType == provider.PromptTypeToolConfirmation {
						t.Errorf("question stored: %q", m.Content)
					}
					if m.ToolCallID == "p2" {
						stopped = m.Content
					}
				}
				for _, want := range []string{"error: ", "store__write", "did not run", "look__see"} {
					if !strings.Contains(stopped, want) {
						t.Errorf("outer call result lacks %q: %q", want, stopped)
					}
				}
				results := eventsOfCall(t, h.sink, events.TypeToolCallResult, "p2")
				extracted := eventsOfCall(t, h.sink, events.TypeToolCallExtracted, "p2")
				if len(results) != 1 || len(extracted) != 1 || results[0].ParentID != extracted[0].ID {
					t.Errorf("p2: %d tool_call_result, %d tool_call_extracted; want one result parented to the extracted event", len(results), len(extracted))
				}
				h.llm.assertTools(t, 2, pauseTools...)
			})
		})
	}
}

func TestSubprocessStoppedResultReportsARefusedCallAsFailed(t *testing.T) {
	h := newPauseHarness(t, pendingFailingStore{state.NewSessionStore("")}, pauseTask)
	child := pauseChild(pauseTask)
	child[pauseTask][0] = callReply("c0", "look__see", map[string]string{"x": "1"})
	h.llm.children = child

	if _, err := h.orch.Run(context.Background(), "s1", "fix record 7"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if h.looks.count != 2 || h.write.count != 0 {
		t.Errorf("look__see ran %d time(s), store__write %d; want 2 (p0, p3; c0 is refused) and never", h.looks.count, h.write.count)
	}
	var stopped string
	for _, m := range h.session(t).Messages {
		if m.ToolCallID == "p2" {
			stopped = m.Content
		}
	}
	if !strings.Contains(stopped, "- failed: look__see(x=1): unknown argument(s)") || strings.Contains(stopped, "ran: look__see") {
		t.Errorf("outer call result = %q, want c0 reported as failed with its error", stopped)
	}
	h.llm.assertTools(t, 2, pauseTools...)
}

func TestUnreadablePendingRecordIsCleared(t *testing.T) {
	eachStore(t, func(t *testing.T, kind storeKind) {
		h := newPauseHarness(t, kind.open(t), pauseTask)
		h.llm.parent = []provider.CompletionResponse{{Content: "Hello."}}
		if err := h.sessions.SetMetadata("s1", "pending_tool_call", `{"id":`); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}

		if _, err := h.orch.Run(context.Background(), "s1", "hello"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if p, ok := h.session(t).Metadata["pending_tool_call"]; ok {
			t.Errorf("unreadable record kept: %q", p)
		}
	})
}
