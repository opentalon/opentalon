package orchestrator

import (
	"context"
	"fmt"
	"maps"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/logger"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// roundRequests builds and sends the chat-loop requests of one turn. A request
// is the turn's system text with this round's parts, the session's
// conversation from its last compaction on (compaction.go), the tools that
// conversation has loaded (loadedTools) and the model settings. Every round
// builds it afresh from the session as stored, so a request always shows what
// the earlier rounds of the turn stored.
type roundRequests struct {
	o           *Orchestrator
	sessions    SessionStoreInterface
	sessionID   string
	sysPrompt   provider.SystemSegments
	nativeMode  bool
	ref         provider.ModelRef
	chain       []provider.ModelInfo // the models that can answer ref, in the order they are tried
	pairingSeen pairingReported
	tools       toolSurface
	spent       []provider.Usage // the summary calls of the current round
}

// toolSurface is the tools a request offers for one loaded set: the native
// array and, for the rest, the catalog lines of the system text.
type toolSurface struct {
	built              bool
	loaded             map[string]bool
	callable, loadable []provider.ToolDefinition
	catalog, inline    string
}

func (o *Orchestrator) newRoundRequests(ctx context.Context, sessions SessionStoreInterface, sessionID string,
	sysPrompt provider.SystemSegments, ref provider.ModelRef) (*roundRequests, error) {
	r := &roundRequests{
		o:          o,
		sessions:   sessions,
		sessionID:  sessionID,
		sysPrompt:  sysPrompt,
		nativeMode: o.supportsNativeTools(),
		ref:        ref,
		chain:      o.modelChain(ref),
	}
	if r.nativeMode {
		sess, err := r.stored()
		if err != nil {
			return nil, err
		}
		r.tools.update(ctx, o, requestConversation(sess.Messages))
	}
	return r, nil
}

// update rebuilds the surface when conv has loaded another set of tools than
// the one it was built for, and marks on conv's tool results what they loaded.
func (t *toolSurface) update(ctx context.Context, o *Orchestrator, conv []provider.Message) {
	loaded := o.loadedTools(ctx, conv)
	if t.built && maps.Equal(loaded, t.loaded) {
		return
	}
	t.built, t.loaded = true, loaded
	t.callable, t.loadable = o.buildToolDefinitions(ctx, loaded)
	t.catalog = o.renderToolCatalog(toolArraySet(t.callable), o.resolveAllowedPlugins(ctx))
	t.inline = renderInlineToolCatalog(t.callable, t.loadable)
}

// turnStartPrompt is the system text the turn starts with, for turn_start.
func (r *roundRequests) turnStartPrompt() string {
	return withToolCatalog(r.sysPrompt, r.tools.catalog, r.tools.inline).Join()
}

// stored reads the session as stored.
func (r *roundRequests) stored() (*state.Session, error) {
	sess, err := r.sessions.Get(r.sessionID)
	if err != nil {
		return nil, fmt.Errorf("session %q: %w", r.sessionID, err)
	}
	return sess, nil
}

// build assembles the round's request: the conversation as stored, transient
// appended (this round's nudges, never stored), the guard plugins' verdict on
// its newest message, the tool pairing repaired, and the tools and system text
// that conversation calls for. A guard that blocks returns its result.
func (r *roundRequests) build(ctx context.Context, transient []provider.Message) (*provider.CompletionRequest, *RunResult, error) {
	sess, err := r.stored()
	if err != nil {
		return nil, nil, err
	}
	conv := requestConversation(sess.Messages)
	conv = append(conv, transient...)
	conv, blocked, err := r.o.runGuardPlugins(ctx, conv)
	if err != nil || blocked != nil {
		return nil, blocked, err
	}
	conv, pairing := pairToolMessages(conv)
	if fresh := r.pairingSeen.fresh(pairing); !fresh.empty() {
		r.o.reportToolPairing(ctx, r.sessionID, fresh)
	}
	if r.nativeMode {
		r.tools.update(ctx, r.o, conv)
	}
	system := roundSystem(ctx, sess.Messages, withToolCatalog(r.sysPrompt, r.tools.catalog, r.tools.inline))
	messages := make([]provider.Message, 0, len(conv)+1)
	messages = append(messages, provider.Message{Role: provider.RoleSystem, Content: system.Join()})
	messages = append(messages, conv...)

	req := &provider.CompletionRequest{Messages: messages, Segments: &system, ModelRef: r.ref, Model: r.ref.Model()}
	if r.nativeMode {
		// Built from the loaded set, which changes only with a load or a
		// compaction, so the prefix stays byte-identical between rounds and
		// the provider-side caches keep hitting.
		req.Tools = r.tools.callable
		req.LoadableTools = r.tools.loadable
	}
	// A per-turn override (channel metadata) wins over the model default,
	// which the provider serving the request would otherwise fill.
	if eff := actor.ReasoningEffort(ctx); eff != "" {
		req.ReasoningEffort = eff
	}
	// Reasoning on every round the model supports it, the tool-deciding ones
	// included: with it off gpt-oss-120b skipped and mis-routed tool calls.
	if r.o.supportsReasoning() {
		req.Reasoning = true
		if p := profile.FromContext(ctx); p != nil && p.BudgetTokens > 0 {
			req.BudgetTokens = p.BudgetTokens
		}
	}
	logger.FromContext(ctx).Debug("LLM request", "messages", len(req.Messages), "tools", len(req.Tools),
		"native", r.nativeMode, "reasoning", req.Reasoning, "reasoning_effort", req.ReasoningEffort)
	return req, nil, nil
}

// roundSystem completes the turn's system text with this round's parts. They
// follow the stored history, not the request's conversation, so a compaction
// leaves the system text as it would be without it.
func roundSystem(ctx context.Context, stored []provider.Message, system provider.SystemSegments) provider.SystemSegments {
	// The per-round segment as built is the output-format section. Hold it
	// back until a tool result exists: on the first round the HTML format hint
	// stops weak models from emitting [tool_call] blocks.
	toolResults := hasToolResults(stored)
	if !toolResults {
		system.Round = ""
	}
	// For weaker / OSS models, repeat the channel format hint — but only after a
	// tool result exists, so it doesn't compete with the tool-calling
	// instruction on the first round.
	if hint := channelFormatHint(ctx); hint != "" && toolResults {
		system.Round += "\n\n[IMPORTANT — output format reminder] " + hint
	}
	// Don't-repeat reminder, only with actual prior conversation.
	if len(stored) > 2 {
		system.Round += "\n\n[IMPORTANT] Answer ONLY the user's most recent message. Do NOT repeat or summarize any earlier answers from this conversation. Be concise."
	}
	// A spoken turn's instruction holds in every round, the first included
	// (spoken_reply.go); the per-round segment sits after the cached prefix.
	if actor.SpokenReply(ctx) {
		system.Round += "\n\n" + prompts.OrchestratorSpokenReply
	}
	return system
}

// requestConversation is the conversation a request carries: the stored
// messages from the session's last compaction on (compactedConversation),
// without resolved confirmation exchanges and poisoned assistant turns, and
// with the knowledge context of past user messages stripped.
func requestConversation(stored []provider.Message) []provider.Message {
	return appendStrippingHistoricalKC(nil, sanitizeHistory(stripApprovedConfirmationExchanges(compactedConversation(stored))))
}

// send sends req as one call, streamed when stream is set.
func (r *roundRequests) send(ctx context.Context, req *provider.CompletionRequest, stream bool) (*provider.CompletionResponse, error) {
	// Always logged: the figure to reconcile against the provider's own count
	// when a session nears a trigger.
	logger.FromContext(ctx).Info("llm request size",
		"messages_count", len(req.Messages),
		"tools_count", len(req.Tools),
		"estimated_tokens", estimateRequestTokens(req))
	if stream {
		return r.o.streamComplete(ctx, req)
	}
	return r.o.llm.Complete(ctx, req)
}
