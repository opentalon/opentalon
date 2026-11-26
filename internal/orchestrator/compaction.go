package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
	pkgchannel "github.com/opentalon/opentalon/pkg/channel"
)

// Compaction. Before every chat-loop request the core estimates it for each
// model of its chain (the session's model and the models it falls back to),
// each with that model's own calibration, and when one of them reaches 95 % of
// that model's input budget the session's model first writes a summary of the
// conversation. The summary is stored as one user-role row at the end of the
// history (metadata prompt_type=compaction); nothing before it changes. Every
// later request carries, after the system text, the summary, the user messages
// the compaction kept word for word, and the messages stored after it
// (compactedConversation). A provider refusal for size starts one compaction
// and one retry. A request right after a compaction is never compacted again.
//
// roundRequests.round owns the lifecycle; the rest of this file is its parts.

const (
	// compactionTrigger is the share of a model's input budget at which a
	// request is compacted first.
	compactionTrigger = 0.95
	// keptUserShare and summaryShare bound the user messages a compaction
	// keeps and its summary, as shares of the room (see room).
	keptUserShare = 0.25
	summaryShare  = 0.10
	// Expected compaction duration: the exponentially weighted mean of the
	// durations of the model's newest expectedDurationSamples compactions,
	// each older one weighing expectedDurationDecay times the next, read from
	// the newest expectedDurationScan compaction_finished events of all
	// models. Provisional values, to be set from measured compactions.
	expectedDurationSamples = 8
	expectedDurationDecay   = 0.7
	expectedDurationScan    = 200
	// shortenedResultMin is the length a tool result of the summary request
	// is never shortened below.
	shortenedResultMin = 2000
)

// shortenedMarker ends a tool result shortened for the summary request.
const shortenedMarker = " […]"

// compactionKeptKey is the metadata key of a compaction's row that holds how
// many user messages it keeps; compactionModelKey names the model that wrote it.
const (
	compactionKeptKey  = "kept"
	compactionModelKey = "model"
)

// errContextLengthExceeded ends a turn whose request cannot be made to fit:
// it exceeds the input budget, or was refused for size, right after a
// compaction or when the compaction failed. The channels answer it with
// their context_length_exceeded notice (provider.IsContextOverflow).
var errContextLengthExceeded = errors.New("context_length_exceeded: the conversation does not fit the model's input budget")

// SessionEventReader reads stored session events across sessions.
type SessionEventReader interface {
	// RecentPayloads returns the payloads of the newest events of eventType,
	// newest first, at most limit.
	RecentPayloads(ctx context.Context, eventType string, limit int) ([]json.RawMessage, error)
}

// ModelChainer is implemented by an LLM client that can name the models a
// request for ref may be answered by, in the order they are tried. Without it
// no model has a window and nothing is compacted.
type ModelChainer interface {
	ModelChain(ref provider.ModelRef) []provider.ModelInfo
}

func (o *Orchestrator) modelChain(ref provider.ModelRef) []provider.ModelInfo {
	if c, ok := o.llm.(ModelChainer); ok {
		return c.ModelChain(ref)
	}
	return nil
}

// roundResult is one round's response and the request that got it.
type roundResult struct {
	req         *provider.CompletionRequest
	resp        *provider.CompletionResponse
	blocked     *RunResult       // set when a guard plugin blocked the request
	compactions []provider.Usage // what the round's summary calls cost
}

// round builds the round's request, compacts first when it reaches a trigger,
// sends it, and answers a refusal for size with one compaction and one retry.
func (r *roundRequests) round(ctx context.Context, transient []provider.Message, stream bool) (roundResult, error) {
	r.spent = nil
	res, err := r.compactedRound(ctx, transient, stream)
	res.compactions = r.spent
	return res, err
}

func (r *roundRequests) compactedRound(ctx context.Context, transient []provider.Message, stream bool) (roundResult, error) {
	req, blocked, err := r.build(ctx, transient)
	if err != nil || blocked != nil {
		return roundResult{blocked: blocked}, err
	}
	limits := r.limits()
	raw := estimateRequestTokens(req)
	if reached, estimate, ok := reachedTrigger(limits, raw); ok {
		if req, blocked, err = r.compactAndRebuild(ctx, req, reached, estimate, transient); err != nil || blocked != nil {
			return roundResult{blocked: blocked}, err
		}
	}

	resp, err := r.send(ctx, req, stream)
	measured, refused := provider.ContextOverflow(err)
	if !refused {
		return r.answered(req, resp, err)
	}
	r.o.calibrators.observeRejection(r.first(), estimateRequestTokens(req), measured)
	logger.FromContext(ctx).Warn("prompt refused as too long; compacting and sending again",
		"provider_measured", measured, "calibration", r.o.calibrators.factor(r.first()))
	if r.justCompacted() {
		return roundResult{}, fmt.Errorf("%w: %w", errContextLengthExceeded, err)
	}
	limit := chainLimit{ref: provider.ModelRef(r.first())}
	if len(limits) > 0 {
		limit = limits[0]
	}
	if err := r.compact(ctx, req, limit, limit.tokens(estimateRequestTokens(req))); err != nil {
		return roundResult{}, fmt.Errorf("%w: %w", errContextLengthExceeded, err)
	}
	if req, blocked, err = r.build(ctx, transient); err != nil || blocked != nil {
		return roundResult{blocked: blocked}, err
	}
	resp, err = r.send(ctx, req, stream)
	if provider.IsContextOverflow(err) {
		return roundResult{}, fmt.Errorf("%w: %w", errContextLengthExceeded, err)
	}
	return r.answered(req, resp, err)
}

// compactAndRebuild handles a request that reached a trigger: unless the
// conversation was compacted right before it, or compacting cannot make
// room, it compacts and builds the request again. The request it returns
// must fit every model's input budget; one that does not ends the turn.
func (r *roundRequests) compactAndRebuild(ctx context.Context, req *provider.CompletionRequest, reached chainLimit,
	estimate int, transient []provider.Message) (*provider.CompletionRequest, *RunResult, error) {
	limits := r.limits()
	if !r.justCompacted() && room(limits, fixedTokens(req)) > 0 {
		if err := r.compact(ctx, req, reached, estimate); err == nil {
			rebuilt, blocked, err := r.build(ctx, transient)
			if err != nil || blocked != nil {
				return nil, blocked, err
			}
			req = rebuilt
		}
	}
	if overBudget(r.limits(), estimateRequestTokens(req)) {
		return nil, nil, errContextLengthExceeded
	}
	return req, nil, nil
}

// answered closes a round the provider answered: the provider's count of the
// request's input teaches the estimate of the model that answered.
func (r *roundRequests) answered(req *provider.CompletionRequest, resp *provider.CompletionResponse, err error) (roundResult, error) {
	if err != nil {
		return roundResult{}, fmt.Errorf("LLM completion: %w", err)
	}
	if resp.ModelRef != "" {
		r.o.calibrators.observe(string(resp.ModelRef), estimateRequestTokens(req), resp.Usage.InputTokens)
	}
	return roundResult{req: req, resp: resp}, nil
}

// first is the chain's first model, the one a request goes to.
func (r *roundRequests) first() string {
	if len(r.chain) == 0 {
		return string(r.ref)
	}
	return string(r.chain[0].Ref())
}

// justCompacted reports whether the newest stored message is a compaction:
// no message was stored since, so compacting again would summarise the
// summary.
func (r *roundRequests) justCompacted() bool {
	sess, err := r.stored()
	if err != nil || len(sess.Messages) == 0 {
		return false
	}
	return sess.Messages[len(sess.Messages)-1].PromptType == provider.PromptTypeCompaction
}

// compact writes the summary of the conversation req carries and stores it.
// On failure nothing is stored and the error is returned; either way the
// compaction's events and frames are out.
func (r *roundRequests) compact(ctx context.Context, req *provider.CompletionRequest, reached chainLimit, estimate int) error {
	sess, err := r.stored()
	if err != nil {
		return err
	}
	limits := r.limits()
	roomTokens := room(limits, fixedTokens(req))
	expected := r.o.expectedCompactionMS(ctx, provider.ModelRef(r.first()))
	startedID := emit.EmitCompactionStarted(ctx, r.o.eventSink, emit.CompactionStartedArgs{
		Model:           string(reached.ref),
		EstimatedTokens: estimate,
		TriggerTokens:   reached.trigger,
		ExpectedMS:      expected,
	})
	ctx = emit.WithParent(ctx, startedID)
	r.o.sendCompactionFrame(ctx, r.sessionID, "started", expected)
	start := time.Now()

	summaryCap := int(float64(roomTokens) * summaryShare)
	resp, err := r.o.llm.Complete(ctx, summaryRequest(req, limits, summaryCap))
	if resp != nil {
		r.spent = append(r.spent, resp.Usage)
	}
	if err == nil && strings.TrimSpace(resp.Content) == "" {
		err = errors.New("the model wrote no summary")
	}
	if err != nil {
		return r.compactionFailed(ctx, err)
	}
	summary := summaryText(resp.Content, toolsCalled(sess.Messages), summaryCap)
	kept := keptUserCount(sess.Messages, int(float64(roomTokens)*keptUserShare))
	metadata := map[string]string{
		"prompt_type":      provider.PromptTypeCompaction,
		compactionModelKey: string(resp.ModelRef),
		compactionKeptKey:  strconv.Itoa(kept),
	}
	row := provider.Message{Role: provider.RoleUser, Content: summary, PromptType: provider.PromptTypeCompaction, KeptUserMessages: kept}
	if err := r.sessions.AddMessageWithMetadata(r.sessionID, row, metadata); err != nil {
		return r.compactionFailed(ctx, fmt.Errorf("store the compaction: %w", err))
	}
	duration := time.Since(start).Milliseconds()
	emit.EmitCompactionFinished(ctx, r.o.eventSink, emit.CompactionFinishedArgs{
		SummaryModel:     string(resp.ModelRef),
		SummaryBytes:     len(summary),
		UserMessagesKept: kept,
		ToolsDropped:     sortedSet(r.tools.loaded),
		DurationMS:       duration,
		Fallback:         string(resp.ModelRef) != r.first(),
	})
	r.o.sendCompactionFrame(ctx, r.sessionID, "finished", 0)
	logger.FromContext(ctx).Info("conversation compacted", "model", resp.ModelRef, "duration_ms", duration,
		"summary_bytes", len(summary), "user_messages_kept", kept)
	return nil
}

func (r *roundRequests) compactionFailed(ctx context.Context, err error) error {
	emit.EmitCompactionFailed(ctx, r.o.eventSink, err)
	r.o.sendCompactionFrame(ctx, r.sessionID, "failed", 0)
	logger.FromContext(ctx).Warn("compaction failed; the turn goes on uncompacted", "error", err)
	return err
}

// sendCompactionFrame tells the session's chat clients about a compaction:
// state is started (with the expected duration in ms, when there is one),
// finished or failed.
func (o *Orchestrator) sendCompactionFrame(ctx context.Context, sessionID, state string, expectedMS int64) {
	if o.channelSender == nil {
		return
	}
	metadata := map[string]string{"type": "session.compaction", "state": state}
	if expectedMS > 0 {
		metadata["expected_ms"] = strconv.FormatInt(expectedMS, 10)
	}
	// ConversationID left empty: the sender splits sessionID itself (see maybeGenerateTitle).
	if err := o.channelSender(ctx, sessionID, pkgchannel.OutboundMessage{Metadata: metadata}); err != nil {
		slog.Warn("session compaction channel push failed", "session_id", sessionID, "state", state, "error", err)
	}
}

// summaryRequest is req continued by the instruction to summarise: the same
// system text, tools and history, so it reads the prompt cache req built. Its
// response is no chat-loop round. Tool results are shortened in it only when
// it would not fit a model's input budget.
func summaryRequest(req *provider.CompletionRequest, limits []chainLimit, summaryCap int) *provider.CompletionRequest {
	cp := *req
	cp.Messages = append(slices.Clone(req.Messages), provider.Message{
		Role:    provider.RoleUser,
		Content: fmt.Sprintf(prompts.Compaction, summaryWords(summaryCap)),
	})
	cp.Compaction = true
	shortenToolResults(&cp, limits)
	return &cp
}

// summaryWords turns a summary's cap in estimate tokens into the word count
// the instruction asks for.
func summaryWords(capTokens int) int {
	const charsPerWord = 6
	return max(100, int(float64(capTokens)*charsPerTokenProse/charsPerWord))
}

// shortenToolResults cuts the longest tool results of req until its estimate
// fits every model's input budget, or no result is left to shorten.
func shortenToolResults(req *provider.CompletionRequest, limits []chainLimit) {
	for {
		excess := excessTokens(limits, estimateRequestTokens(req))
		if excess <= 0 {
			return
		}
		longest := -1
		for i, m := range req.Messages {
			if provider.IsToolResult(m) && (longest < 0 || len(m.Content) > len(req.Messages[longest].Content)) {
				longest = i
			}
		}
		if longest < 0 {
			return
		}
		content := req.Messages[longest].Content
		keep := max(shortenedResultMin, len(content)-int(float64(excess)*charsPerTokenJSON)-len(shortenedMarker))
		if keep >= len(content) {
			return
		}
		req.Messages[longest].Content = cutUTF8(content, keep) + shortenedMarker
	}
}

// summaryText is the stored summary: the model's text, cut to the summary's
// share of the room, and the line naming the tools called before it.
func summaryText(written string, tools []string, capTokens int) string {
	var toolsLine string
	if len(tools) > 0 {
		toolsLine = "\n\n" + fmt.Sprintf(prompts.CompactionTools, strings.Join(tools, ", "))
	}
	summary := strings.TrimSpace(written)
	if limit := int(float64(capTokens)*charsPerTokenProse) - len(toolsLine); limit > 0 && len(summary) > limit {
		summary = strings.TrimSpace(cutUTF8(summary, limit))
	}
	return summary + toolsLine
}

// cutUTF8 returns s cut to at most n bytes, at a rune boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// toolsCalled names every tool the stored history called, except the core's
// own meta-tools, sorted.
func toolsCalled(stored []provider.Message) []string {
	called := map[string]bool{}
	for _, m := range stored {
		for _, tc := range m.ToolCalls {
			if !strings.HasPrefix(tc.Name, metaPluginName+"__") {
				called[tc.Name] = true
			}
		}
	}
	return sortedSet(called)
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// lastCompaction returns the index of the newest compaction row in stored, -1
// when there is none.
func lastCompaction(stored []provider.Message) int {
	for i := len(stored) - 1; i >= 0; i-- {
		if stored[i].PromptType == provider.PromptTypeCompaction {
			return i
		}
	}
	return -1
}

// compactedConversation is the conversation a request carries from the
// session's newest compaction on: its summary, the user messages it kept word
// for word in their order, and every message stored after it. Without a
// compaction it is stored as it is.
func compactedConversation(stored []provider.Message) []provider.Message {
	at := lastCompaction(stored)
	if at < 0 {
		return stored
	}
	kept := keptUserMessages(stored[:at])
	if n := stored[at].KeptUserMessages; n < len(kept) {
		kept = kept[:n]
	}
	out := make([]provider.Message, 0, 1+len(kept)+len(stored)-at-1)
	out = append(out, stored[at])
	for i := len(kept) - 1; i >= 0; i-- {
		out = append(out, stored[kept[i]])
	}
	return append(out, stored[at+1:]...)
}

// keptUserMessages returns the indices of the user messages in before a
// compaction may keep word for word, newest first: the newest user message,
// the turn's own even when hidden, then every visible one before it. Tool
// results and rows of the confirmation flow or an earlier compaction are none.
func keptUserMessages(before []provider.Message) []int {
	var out []int
	for i := len(before) - 1; i >= 0; i-- {
		m := before[i]
		if m.Role != provider.RoleUser || provider.IsToolResult(m) || m.PromptType != "" {
			continue
		}
		if len(out) > 0 && m.Visibility == provider.VisibilityHidden {
			continue
		}
		out = append(out, i)
	}
	return out
}

// keptUserCount is how many of the user messages before the compaction it
// keeps: newest first while they fit capTokens, the newest one always, so
// the ones that do not fit are left to the summary.
func keptUserCount(before []provider.Message, capTokens int) int {
	n, total := 0, 0
	for _, i := range keptUserMessages(before) {
		cost := estimateMessageTokens(before[i])
		if n > 0 && total+cost > capTokens {
			break
		}
		total += cost
		n++
	}
	return n
}

// chainLimit is the trigger and input budget of one model of the chain with a
// window, in that model's tokens, and the calibration that turns an estimate
// into them.
type chainLimit struct {
	ref     provider.ModelRef
	factor  float64
	budget  int
	trigger int
}

func (l chainLimit) tokens(raw int) int { return int(float64(raw) * l.factor) }

// limits are the chain's models that have a window. A model without one sets
// no trigger.
func (r *roundRequests) limits() []chainLimit {
	var out []chainLimit
	for _, m := range r.chain {
		budget := inputTokenBudget(m.ContextWindow, m.MaxTokens)
		if budget <= 0 {
			continue
		}
		out = append(out, chainLimit{
			ref:     m.Ref(),
			factor:  r.o.calibrators.factor(string(m.Ref())),
			budget:  budget,
			trigger: int(float64(budget) * compactionTrigger),
		})
	}
	return out
}

// reachedTrigger returns the first model whose trigger the estimate raw
// reaches, and the estimate in its tokens.
func reachedTrigger(limits []chainLimit, raw int) (chainLimit, int, bool) {
	for _, l := range limits {
		if est := l.tokens(raw); est >= l.trigger {
			return l, est, true
		}
	}
	return chainLimit{}, 0, false
}

// overBudget reports whether the estimate raw exceeds a model's input budget.
func overBudget(limits []chainLimit, raw int) bool {
	return excessTokens(limits, raw) > 0
}

// excessTokens is by how many estimate tokens raw exceeds the input budget of
// the model it exceeds most; 0 or less when it fits them all.
func excessTokens(limits []chainLimit, raw int) int {
	excess := math.MinInt
	for _, l := range limits {
		excess = max(excess, raw-int(float64(l.budget)/l.factor))
	}
	if excess == math.MinInt {
		return 0
	}
	return excess
}

// room is what a request leaves the conversation, in estimate tokens: each
// model's trigger less the request's fixed part (system text and tools), for
// the model with the least; 0 when the fixed part alone reaches a trigger.
func room(limits []chainLimit, fixed int) int {
	least := math.MaxInt
	for _, l := range limits {
		least = min(least, int(float64(l.trigger)/l.factor)-fixed)
	}
	if least == math.MaxInt || least < 0 {
		return 0
	}
	return least
}

// fixedTokens estimates req's fixed part: the system text and the tools.
func fixedTokens(req *provider.CompletionRequest) int {
	n := estimateToolTokens(req.Tools)
	if len(req.Messages) > 0 && req.Messages[0].Role == provider.RoleSystem {
		n += estimateMessageTokens(req.Messages[0])
	}
	return n
}

// expectedCompactionMS is the expected duration of a compaction by model: the
// exponentially weighted mean of its newest compactions that finished without
// a fallback, each counted for the model that wrote its summary. 0 when it has
// none, or the events cannot be read.
func (o *Orchestrator) expectedCompactionMS(ctx context.Context, model provider.ModelRef) int64 {
	if o.sessionEvents == nil || model == "" {
		return 0
	}
	payloads, err := o.sessionEvents.RecentPayloads(ctx, events.TypeCompactionFinished, expectedDurationScan)
	if err != nil {
		slog.Warn("compaction durations not read; no expected duration", "error", err)
		return 0
	}
	var durations []int64
	for _, raw := range payloads {
		var p events.CompactionFinishedPayload
		if json.Unmarshal(raw, &p) != nil || p.Fallback || p.SummaryModel != string(model) || p.DurationMS <= 0 {
			continue
		}
		if durations = append(durations, p.DurationMS); len(durations) == expectedDurationSamples {
			break
		}
	}
	return weightedMean(durations)
}

// weightedMean is the exponentially weighted mean of durations, newest first;
// 0 without any.
func weightedMean(durations []int64) int64 {
	var sum, weights float64
	w := 1.0
	for _, d := range durations {
		sum += w * float64(d)
		weights += w
		w *= expectedDurationDecay
	}
	if weights == 0 {
		return 0
	}
	return int64(math.Round(sum / weights))
}

// estimateRequestTokens is the uncalibrated estimate of req's input: its tools
// and messages.
func estimateRequestTokens(req *provider.CompletionRequest) int {
	n := estimateToolTokens(req.Tools)
	for _, m := range req.Messages {
		n += estimateMessageTokens(m)
	}
	return n
}
