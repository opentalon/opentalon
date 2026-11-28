package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// 16 tasks per parallel call (maxParallelTasks), each with one result at the 64 KiB result cap.
const maxContinuationBytes = 1 << 20

// Sub-agents never write the session: Run stores the continuation after the executor and executeCall filled it.
type subprocessContinuation struct {
	Call      continuationCall `json:"call"`
	EventID   string           `json:"event_id,omitempty"`
	ReplyText string           `json:"reply_text,omitempty"`
	responseThinking
	InputTokens  int               `json:"input_tokens,omitempty"`
	OutputTokens int               `json:"output_tokens,omitempty"`
	Children     []subprocessChild `json:"children"`
	Asking       int               `json:"asking"`
}

type continuationCall struct {
	ID     string            `json:"id"`
	Plugin string            `json:"plugin"`
	Action string            `json:"action"`
	Args   map[string]string `json:"args,omitempty"`
}

func continuationCallOf(call ToolCall) continuationCall {
	return continuationCall{ID: call.ID, Plugin: call.Plugin, Action: call.Action, Args: call.Args}
}

func (c continuationCall) toolCall() ToolCall {
	return ToolCall{ID: c.ID, Plugin: c.Plugin, Action: c.Action, Args: c.Args, FromLLM: true}
}

func (c continuationCall) String() string {
	args := make([]string, 0, len(c.Args))
	for _, k := range slices.Sorted(maps.Keys(c.Args)) {
		args = append(args, k+"="+c.Args[k])
	}
	return toolFQN(c.Plugin, c.Action) + "(" + strings.Join(args, ", ") + ")"
}

type subprocessChild struct {
	Position   int                 `json:"position"`
	Request    subprocessRequest   `json:"request"`
	Depth      int                 `json:"depth"`
	Messages   []provider.Message  `json:"messages"`
	Iterations int                 `json:"iterations"`
	Remaining  time.Duration       `json:"remaining"`
	Ran        []ranCall           `json:"ran,omitempty"`
	Waiting    waitingCall         `json:"waiting"`
	Result     *parallelTaskResult `json:"result,omitempty"`
}

func (child *subprocessChild) finish(res *subprocessResult, err error) {
	child.Result = &parallelTaskResult{Task: child.Request.Task}
	if res != nil {
		child.Ran = res.Ran
	}
	if err != nil {
		child.Result.Error = fmt.Sprintf("subprocess failed: %v", err)
	} else {
		child.Result.Response, child.Result.Iterations = res.Response, res.Iterations
	}
	child.Messages, child.Waiting = nil, waitingCall{}
}

func (c *subprocessContinuation) waitingChild() int {
	return slices.IndexFunc(c.Children, func(child subprocessChild) bool { return child.Result == nil })
}

func (c *subprocessContinuation) result() ToolResult {
	if c.Call.Action == "parallel" {
		return joinParallel(c.Call.ID, c.Children)
	}
	r := c.Children[0].Result
	if r.Error != "" {
		return ToolResult{CallID: c.Call.ID, Error: r.Error}
	}
	return ToolResult{CallID: c.Call.ID, Content: r.Response}
}

type ranCall struct {
	continuationCall
	Error string `json:"error,omitempty"`
}

type waitingCall struct {
	continuationCall
	Text string `json:"text,omitempty"`
	responseThinking
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// Children of one parallel call number their calls alike, so the id names the child too.
func (c *subprocessContinuation) questionID() string {
	if c.Asking < 0 || c.Asking >= len(c.Children) {
		return c.Call.ID
	}
	child := c.Children[c.Asking]
	return fmt.Sprintf("%s-%d-%s", c.Call.ID, child.Position, child.Waiting.ID)
}

func (c *subprocessContinuation) stoppedResult(why string) ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "the sub-agent was stopped before it finished: %s.", why)
	for i, child := range c.Children {
		fmt.Fprintf(&sb, "\n\nTask %d:", child.Position+1)
		switch {
		case child.Result != nil:
		case i == c.Asking:
			fmt.Fprintf(&sb, "\n- did not run: %s", child.Waiting.continuationCall)
		default:
			fmt.Fprintf(&sb, "\n- did not run (not asked about yet): %s", child.Waiting.continuationCall)
		}
		for _, ran := range child.Ran {
			if ran.Error != "" {
				fmt.Fprintf(&sb, "\n- failed: %s: %s", ran.continuationCall, ran.Error)
			} else {
				fmt.Fprintf(&sb, "\n- ran: %s", ran.continuationCall)
			}
		}
		switch r := child.Result; {
		case r == nil:
		case r.Error != "":
			fmt.Fprintf(&sb, "\n- error: %s", r.Error)
		default:
			fmt.Fprintf(&sb, "\n- answer: %s", r.Response)
		}
	}
	return ToolResult{CallID: c.Call.ID, Error: sb.String()}
}

// The record is stored before anything is published, so a sub-agent the session cannot store asks nothing.
func (o *Orchestrator) pauseSubprocess(
	ctx context.Context, sessions SessionStoreInterface, sessionID, userMessage string,
	c *subprocessContinuation, later []ToolCall, laterNative bool, thinking *responseThinking, dispatchStart time.Time,
) (*RunResult, ToolResult) {
	log := logger.FromContext(ctx)
	// The response's thinking goes on its first stored message: a later call's, else the outer call's when it ends.
	if len(later) == 0 {
		c.responseThinking = *thinking
	}
	if data, err := json.Marshal(c); err != nil || len(data) > maxContinuationBytes {
		log.Warn("paused sub-agent cannot be stored; stopping it", "bytes", len(data), "error", err)
		why := fmt.Sprintf("it waited for the user's approval, and its state is larger than the %d bytes a waiting sub-agent may hold", maxContinuationBytes)
		if err != nil {
			why = "it waited for the user's approval, and its state could not be stored"
		}
		return nil, o.stopSubprocess(ctx, c, why, dispatchStart)
	}

	child := c.Children[c.Asking]
	waiting := child.Waiting.toolCall()
	var recent []provider.Message
	for _, m := range child.Messages {
		if m.Role != provider.RoleSystem {
			recent = append(recent, m)
		}
	}
	spoken, question := o.wordConfirmation(ctx, recent, waiting, userMessage, child.Request.Task)
	confID, err := emit.NewEventID()
	if err != nil {
		log.Warn("confirmation_requested id not generated; the answer will not link to the question", "error", err)
	}
	meta := pendingToolCallMeta{Prompt: question, Spoken: actor.SpokenReply(ctx), Subprocess: c, ConfirmationRequestedID: confID}
	if err := writePendingToolCall(sessions, sessionID, meta); err != nil {
		log.Warn("paused sub-agent cannot be stored; stopping it", "error", err)
		return nil, o.stopSubprocess(ctx, c, "it waited for the user's approval, and the session could not store it", dispatchStart)
	}

	o.recordConfirmationInterruptedCalls(sessionID, sessions, c.Call.toolCall(), later, laterNative, thinking)
	frameMeta, _ := o.publishConfirmation(ctx, sessions, sessionID, spoken, question, c.questionID(), waiting.ID, confID)
	return &RunResult{Response: question, Metadata: frameMeta}, ToolResult{}
}

func (o *Orchestrator) stopSubprocess(ctx context.Context, c *subprocessContinuation, why string, dispatchStart time.Time) ToolResult {
	return o.endSubprocess(ctx, c, c.stoppedResult(why), dispatchStart)
}

func (o *Orchestrator) endSubprocess(ctx context.Context, c *subprocessContinuation, result ToolResult, dispatchStart time.Time) ToolResult {
	result = o.guard.Sanitize(result)
	result.EventID = o.emitCallResult(emit.WithParent(ctx, c.EventID), c.Call.ID, result, dispatchStart)
	return result
}

func (o *Orchestrator) resumeSubprocess(ctx context.Context, sessions SessionStoreInterface, sessionID, userMessage string, c *subprocessContinuation) (*RunResult, ToolResult) {
	start := time.Now()
	subCtx := emit.WithParent(ctx, c.EventID)
	for approved := true; ; approved = false {
		child := &c.Children[c.Asking]
		if why := o.subprocessUnavailable(ctx, child.Request); why != "" {
			return nil, o.stopSubprocess(ctx, c, why, start)
		}
		res, err := o.resumeChild(subCtx, *child, approved)
		if err == nil && res.Paused != nil {
			*child = *res.Paused
			thinking := c.responseThinking
			return o.pauseSubprocess(ctx, sessions, sessionID, userMessage, c, nil, true, &thinking, start)
		}
		child.finish(res, err)
		if c.Asking = c.waitingChild(); c.Asking < 0 {
			return nil, o.endSubprocess(ctx, c, c.result(), start)
		}
	}
}

// A call not yet approved is decided again: what the decision reads may have changed while the child waited.
func (o *Orchestrator) resumeChild(ctx context.Context, child subprocessChild, approved bool) (*subprocessResult, error) {
	callCtx, _ := subprocessContext(ctx, child.Request, child.Depth)
	waiting := child.Waiting
	result, outcome := ToolResult{}, gateExecute
	if approved {
		result = o.executeSubprocessCall(callCtx, waiting.toolCall(), waiting.InputTokens, waiting.OutputTokens)
	} else {
		permitted, _ := o.subprocessTools(callCtx, child.Request)
		var err error
		result, outcome, err = o.subprocessCall(callCtx, permitted, waiting.toolCall(), true, waiting.InputTokens, waiting.OutputTokens)
		if err != nil {
			return nil, fmt.Errorf("subprocess call check: %w", err)
		}
		if outcome == gateAwaitApproval {
			return &subprocessResult{Paused: &child}, nil
		}
	}
	if outcome == gateExecute {
		child.Ran = append(child.Ran, ranCall{continuationCall: waiting.continuationCall, Error: result.Error})
	}
	assistant, tool := nativeCallMessages(waiting.toolCall(), waiting.Text, result)
	child.Messages = append(child.Messages, waiting.attach(assistant), tool)
	// The resume runs inside the turn's lock, so the sub-agent may ask again.
	return o.continueSubprocess(withInteractiveTurn(ctx), child)
}

func (o *Orchestrator) subprocessUnavailable(ctx context.Context, req subprocessRequest) string {
	if !o.subprocessConfig.Enabled {
		return "sub-agents are switched off"
	}
	if !o.supportsNativeTools() {
		return subprocessNeedsNativeTools
	}
	if _, err := o.resolveSubprocessModel(ctx, ToolCall{}, req.Model); err != nil {
		return err.Error()
	}
	return ""
}

type endedSubprocess struct {
	call   ToolCall
	result ToolResult
}

func (o *Orchestrator) settleSubprocess(sessions SessionStoreInterface, sessionID string, c *subprocessContinuation, result ToolResult) *endedSubprocess {
	call := c.Call.toolCall()
	assistant, tool := nativeCallMessages(call, c.ReplyText, result)
	_ = sessions.AddMessage(sessionID, c.attach(assistant))
	_ = sessions.AddMessage(sessionID, tool)
	if o.pluginCallObserver != nil {
		o.pluginCallObserver.ObservePluginCall(call.Plugin, call.Action, result.Error != "", c.InputTokens, c.OutputTokens)
	}
	return &endedSubprocess{call: call, result: result}
}
