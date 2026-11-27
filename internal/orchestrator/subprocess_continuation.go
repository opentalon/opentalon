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
	Children []subprocessChild `json:"children"`
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
	// Ran are the calls it executed, in order.
	Ran     []continuationCall `json:"ran,omitempty"`
	Waiting waitingCall        `json:"waiting"`
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
// the calls that already ran, so the parent model never repeats work it cannot
// see.
func (c *subprocessContinuation) stoppedResult(why string) ToolResult {
	var sb strings.Builder
	fmt.Fprintf(&sb, "the sub-agent was stopped before it finished: %s.", why)
	for _, child := range c.Children {
		fmt.Fprintf(&sb, "\n\nTask %d:\n- did not run: %s", child.Position+1, child.Waiting.continuationCall)
		for _, ran := range child.Ran {
			fmt.Fprintf(&sb, "\n- ran: %s", ran)
		}
	}
	return ToolResult{CallID: c.Call.ID, Error: sb.String()}
}

// pauseSubprocess ends the turn with the question of a sub-agent that waits for
// the user's approval of one of its calls. c is the outer call's continuation;
// later are the parent's calls of the same response after it, recorded as
// laterNative says; thinking holds the response's thinking no stored message
// has taken. c is stored with the pending call before anything is published;
// then the later calls are recorded as not executed, the question is stored and
// confirmation_requested is emitted, with the waiting call's id. When c cannot
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
	meta := pendingToolCallMeta{Prompt: question, Spoken: actor.SpokenReply(ctx), Subprocess: c}
	if err := writePendingToolCall(sessions, sessionID, meta); err != nil {
		log.Warn("paused sub-agent cannot be stored; stopping it", "error", err)
		return nil, o.stopSubprocess(ctx, c, "it waited for the user's approval, and the session could not store it", dispatchStart)
	}

	o.recordConfirmationInterruptedCalls(sessionID, sessions, c.Call.toolCall(), later, laterNative, thinking)
	frameMeta, confID := o.publishConfirmation(ctx, sessions, sessionID, spoken, question, c.questionID(), waiting.ID)
	// The event's id exists only once it is emitted; a record without it only
	// loses the link of the answer's confirmation_resolved event.
	if confID != "" {
		meta.ConfirmationRequestedID = confID
		if err := writePendingToolCall(sessions, sessionID, meta); err != nil {
			log.Warn("confirmation_requested id not stored with the paused sub-agent", "error", err)
		}
	}
	return &RunResult{Response: question, Metadata: frameMeta}, ToolResult{}
}

// stopSubprocess gives c's outer call its stopped-call result and emits it as
// the call's tool_call_result, under the call's extracted event.
func (o *Orchestrator) stopSubprocess(ctx context.Context, c *subprocessContinuation, why string, dispatchStart time.Time) ToolResult {
	result := o.guard.Sanitize(c.stoppedResult(why))
	result.EventID = emit.EmitToolCallResult(emit.WithParent(ctx, c.EventID), o.eventSink, emit.ToolCallResultArgs{
		CallID:    c.Call.ID,
		Status:    "error",
		Response:  result.Error,
		LatencyMS: time.Since(dispatchStart).Milliseconds(),
	})
	return result
}
