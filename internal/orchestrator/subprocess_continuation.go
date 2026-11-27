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

// maxContinuationBytes bounds a stored continuation: one result at the 64 KiB
// result cap for each of the 16 children one parallel call may hold. A larger
// one counts as one the session cannot store.
const maxContinuationBytes = 1 << 20

// subprocessContinuation is a model's _subprocess call in the agent loop whose
// sub-agent waits for the user's approval of one of its calls. The session's
// pending tool-call record holds it (pendingToolCallMeta.Subprocess), so it
// outlives the turn and reaches any pod. The sub-agents never touch the
// session: the executor fills Children and Asking, executeCall EventID, and
// Run the rest before it stores the continuation.
type subprocessContinuation struct {
	// Call is the parent's _subprocess call as the model issued it. It is not
	// in the parent's history yet.
	Call continuationCall `json:"call"`
	// EventID is the call's tool_call_extracted event, the parent of the
	// tool_call_result the call emits when it completes or is stopped.
	EventID string `json:"event_id,omitempty"`
	// ReplyText and the thinking are what of the parent's response no stored
	// message has taken, for the call's message when it is stored.
	ReplyText string `json:"reply_text,omitempty"`
	responseThinking
	// InputTokens and OutputTokens are the parent's response tokens
	// attributed to the call, for the plugin call observer when the call ends.
	InputTokens  int               `json:"input_tokens,omitempty"`
	OutputTokens int               `json:"output_tokens,omitempty"`
	Children     []subprocessChild `json:"children"`
	// Asking is the index in Children of the child whose call the stored
	// question is about.
	Asking int `json:"asking"`
}

// continuationCall is a model's call a continuation keeps, as issued.
type continuationCall struct {
	ID     string            `json:"id"`
	Plugin string            `json:"plugin"`
	Action string            `json:"action"`
	Args   map[string]string `json:"args,omitempty"`
}

func continuationCallOf(call ToolCall) continuationCall {
	return continuationCall{ID: call.ID, Plugin: call.Plugin, Action: call.Action, Args: call.Args}
}

// toolCall is the model's call c keeps.
func (c continuationCall) toolCall() ToolCall {
	return ToolCall{ID: c.ID, Plugin: c.Plugin, Action: c.Action, Args: c.Args, FromLLM: true}
}

// String renders the call as its tool with the arguments in key order.
func (c continuationCall) String() string {
	args := make([]string, 0, len(c.Args))
	for _, k := range slices.Sorted(maps.Keys(c.Args)) {
		args = append(args, k+"="+c.Args[k])
	}
	return toolFQN(c.Plugin, c.Action) + "(" + strings.Join(args, ", ") + ")"
}

// subprocessChild is one sub-agent where it paused: what its loop resumes from.
type subprocessChild struct {
	// Position is the child's place among a parallel call's tasks; 0 for run.
	Position int               `json:"position"`
	Request  subprocessRequest `json:"request"`
	Depth    int               `json:"depth"`
	// Messages are the sub-agent's messages so far, each response's thinking
	// on the first message stored for it. The waiting call is not among them.
	Messages []provider.Message `json:"messages"`
	// Iterations are the model rounds the sub-agent has used.
	Iterations int `json:"iterations"`
	// Remaining is what is left of its running time; the wait for the user's
	// answer does not count.
	Remaining time.Duration `json:"remaining"`
	// Ran are the calls it sent for execution, in order.
	Ran     []ranCall   `json:"ran,omitempty"`
	Waiting waitingCall `json:"waiting"`
}

// ranCall is a call a sub-agent sent for execution, with the error it got. A
// call refused before it reached its plugin gets one too, so a failed call is
// never reported as one that ran.
type ranCall struct {
	continuationCall
	Error string `json:"error,omitempty"`
}

// waitingCall is a sub-agent's call that waits for the user's approval, with
// what of its response no stored message has taken and the tokens of the
// response attributed to it.
type waitingCall struct {
	continuationCall
	Text string `json:"text,omitempty"`
	responseThinking
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// questionID is the id the stored question carries in its frame and in the
// record: unique per child, because children number their calls alike. The
// ids in the messages stay the provider's.
func (c *subprocessContinuation) questionID() string {
	if c.Asking < 0 || c.Asking >= len(c.Children) {
		return c.Call.ID
	}
	child := c.Children[c.Asking]
	return fmt.Sprintf("%s-%d-%s", c.Call.ID, child.Position, child.Waiting.ID)
}

// stoppedResult is the outer call's result when its sub-agents are stopped
// before they finish: why, and per child the waiting call that did not run and
// the calls already sent for execution, each as ran or failed with its error,
// so the parent model never repeats work it cannot see.
func (c *subprocessContinuation) stoppedResult(why string) ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "the sub-agent was stopped before it finished: %s.", why)
	for _, child := range c.Children {
		fmt.Fprintf(&sb, "\n\nTask %d:\n- did not run: %s", child.Position+1, child.Waiting.continuationCall)
		for _, ran := range child.Ran {
			if ran.Error != "" {
				fmt.Fprintf(&sb, "\n- failed: %s: %s", ran.continuationCall, ran.Error)
			} else {
				fmt.Fprintf(&sb, "\n- ran: %s", ran.continuationCall)
			}
		}
	}
	return ToolResult{CallID: c.Call.ID, Error: sb.String()}
}

// pauseSubprocess ends the turn with the question of a sub-agent that waits for
// the user's approval of one of its calls. c is the outer call's continuation;
// later are the parent's calls of the same response after it, recorded as
// laterNative says; thinking holds the response's thinking no stored message
// has taken. c is stored with the pending call, and with the id of the
// confirmation_requested event to come, before anything is published; then
// the later calls are recorded as not executed, the question is stored and
// confirmation_requested is emitted under that id, naming the waiting call. When c cannot
// be stored, over maxContinuationBytes or because the write fails, nothing is
// asked or recorded: there is no question, and the outer call gets its
// stopped-call result instead.
func (o *Orchestrator) pauseSubprocess(
	ctx context.Context, sessions SessionStoreInterface, sessionID, userMessage string,
	c *subprocessContinuation, later []ToolCall, laterNative bool, thinking *responseThinking, dispatchStart time.Time,
) (*RunResult, ToolResult) {
	log := logger.FromContext(ctx)
	// The first message stored for the response takes its thinking: a later
	// call's, recorded below, or else the outer call's, stored when it ends.
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

// stopSubprocess ends c's outer call with its stopped-call result
// (endSubprocess).
func (o *Orchestrator) stopSubprocess(ctx context.Context, c *subprocessContinuation, why string, dispatchStart time.Time) ToolResult {
	return o.endSubprocess(ctx, c, c.stoppedResult(why), dispatchStart)
}

// endSubprocess gives c's outer call result, sanitized, and emits it as the
// call's tool_call_result under the call's extracted event: the one result
// event the call has, when it completes or is stopped.
func (o *Orchestrator) endSubprocess(ctx context.Context, c *subprocessContinuation, result ToolResult, dispatchStart time.Time) ToolResult {
	result = o.guard.Sanitize(result)
	status, response, structured := "ok", result.Content, result.StructuredContent
	if result.Error != "" {
		status, response, structured = "error", result.Error, ""
	}
	result.EventID = emit.EmitToolCallResult(emit.WithParent(ctx, c.EventID), o.eventSink, emit.ToolCallResultArgs{
		CallID:     c.Call.ID,
		Status:     status,
		Response:   response,
		Structured: structured,
		LatencyMS:  time.Since(dispatchStart).Milliseconds(),
	})
	return result
}

// resumeSubprocess runs the call c's sub-agent waits with, which the user
// approved, and lets the sub-agent go on where it paused. The call runs once,
// under the deadline every call gets and without repair, and its result enters
// the sub-agent's messages, not the parent's history. The sub-agent then runs
// for the running time it had left, with the tools the caller's profile
// permits now, and may ask again: its next call that needs approval is asked
// about as at a pause, and the question returned. Otherwise the outer call
// ends (endSubprocess) with the sub-agent's answer, or with the stopped-call
// result when this orchestrator cannot run the sub-agent any more; the waiting
// call does not run then.
func (o *Orchestrator) resumeSubprocess(ctx context.Context, sessions SessionStoreInterface, sessionID, userMessage string, c *subprocessContinuation) (*RunResult, ToolResult) {
	start := time.Now()
	child := c.Children[c.Asking]
	if why := o.subprocessUnavailable(ctx, child.Request); why != "" {
		return nil, o.stopSubprocess(ctx, c, why, start)
	}
	// The sub-agent's calls emit under the outer call, as in the outer call's turn.
	subCtx := emit.WithParent(ctx, c.EventID)
	callCtx, _ := subprocessContext(subCtx, child.Request, child.Depth)
	waiting := child.Waiting
	result := o.executeSubprocessCall(callCtx, waiting.toolCall(), waiting.InputTokens, waiting.OutputTokens)
	child.Ran = append(child.Ran, ranCall{continuationCall: waiting.continuationCall, Error: result.Error})
	child.Messages = append(child.Messages,
		waiting.attach(provider.Message{
			Role:      provider.RoleAssistant,
			Content:   waiting.Text,
			ToolCalls: []provider.ToolCall{{ID: waiting.ID, Name: toolFQN(waiting.Plugin, waiting.Action), Arguments: waiting.Args}},
		}),
		provider.Message{Role: provider.RoleTool, Content: nativeToolContent(result), ToolCallID: waiting.ID},
	)

	// The resume runs inside the turn's lock, so the sub-agent may ask again.
	res, err := o.continueSubprocess(withInteractiveTurn(subCtx, sessions, userMessage), child)
	switch {
	case err != nil:
		return nil, o.endSubprocess(ctx, c, ToolResult{CallID: c.Call.ID, Error: fmt.Sprintf("subprocess failed: %v", err)}, start)
	case res.Paused != nil:
		c.Children[c.Asking] = *res.Paused
		thinking := c.responseThinking
		return o.pauseSubprocess(ctx, sessions, sessionID, userMessage, c, nil, true, &thinking, start)
	}
	return nil, o.endSubprocess(ctx, c, ToolResult{CallID: c.Call.ID, Content: res.Response}, start)
}

// subprocessUnavailable says why this orchestrator cannot run a sub-agent of
// req, empty when it can: sub-agents are switched off, its models lack native
// tool calling, or req's model is not one a sub-agent may run on.
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

// endedSubprocess is the outer call of a paused sub-agent that a later turn
// ended, with its result; resumed when the user approved the waiting call.
type endedSubprocess struct {
	call    ToolCall
	result  ToolResult
	resumed bool
}

// settleSubprocess records c's outer call, ended with result in a turn after
// its pause: the call, with the reply text and thinking of the parent's
// response c kept, and its result enter the parent's history in the native
// shapes, and the plugin call observer gets the call with the parent's tokens
// attributed to it.
func (o *Orchestrator) settleSubprocess(sessions SessionStoreInterface, sessionID string, c *subprocessContinuation, result ToolResult, resumed bool) *endedSubprocess {
	call := c.Call.toolCall()
	_ = sessions.AddMessage(sessionID, c.attach(provider.Message{
		Role:      provider.RoleAssistant,
		Content:   c.ReplyText,
		ToolCalls: []provider.ToolCall{{ID: call.ID, Name: toolFQN(call.Plugin, call.Action), Arguments: call.Args}},
	}))
	_ = sessions.AddMessage(sessionID, provider.Message{Role: provider.RoleTool, Content: nativeToolContent(result), ToolCallID: call.ID})
	if o.pluginCallObserver != nil {
		o.pluginCallObserver.ObservePluginCall(call.Plugin, call.Action, result.Error != "", c.InputTokens, c.OutputTokens)
	}
	return &endedSubprocess{call: call, result: result, resumed: resumed}
}
