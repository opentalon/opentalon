package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// Tool error tracking + sticky demotion.
//
// Two protections against runaway tool-failure loops. Both are opt-in:
// a zero/unset threshold turns that protection off.
//
//   - Loop-cap per tool per turn: at LoopCapPerTurn consecutive
//     identical-tool errors in the same turn, the orchestrator injects a
//     nudge — "Tool X failed N times in this turn — consider a different
//     approach." — into the next LLM call so the LLM stops slamming the
//     same broken tool. (Carried as a RoleUser "[system]" message, not a
//     RoleSystem one — see recordToolOutcome.)
//   - Session-level sticky demotion: at StickyDemotionThreshold
//     consecutive errors for a tool across the entire session, the
//     orchestrator flips Demoted=true on the tool's KnownToolEntry. There
//     are no tiers: promotedToolSet selects the sticky set on Demoted +
//     LRURank, so a Demoted tool is the preferred eviction target when
//     the sticky cap is exceeded. A later load_tools call re-promotes it
//     (and clears Demoted) — demotion is a soft penalty, not a block.
//
// Self-healing: any successful invocation clears the demoted flag. We
// reset BOTH the per-turn and per-session counters on success AND flip
// Demoted=false in KnownTools (best-effort: a transient store failure
// logs and continues, same robustness contract as the load_tools write
// path).
//
// Repair interaction (tool_call_repair.go): a repaired success resets the
// consecutive-error counters exactly like a normal success, but every
// corrector invocation bumps a SEPARATE per-tool repair counter
// (recordRepairAttempt) that feeds the same sticky-demotion threshold and
// only resets on a clean, unrepaired success — so a tool whose schema
// chronically misleads the planner (every call fails pre-dispatch, gets
// repaired, succeeds) still gets demoted instead of looking healthy while
// costing one corrector LLM call per invocation.
//
// State location: in-memory sync.Map keyed by sessionID. Counters
// are NOT persisted — a process restart resets them. The persisted
// artifact is only the Demoted flag (which lives in
// state.InjectionState alongside the rest of KnownTools).

// sessionErrorState holds the per-session error counters. Guarded
// by an internal mutex so concurrent tool-result handlers (rare —
// the agent loop is sequential — but possible across overlapping
// sessions sharing the same Orchestrator) don't race.
type sessionErrorState struct {
	mu             sync.Mutex
	currentTurn    int
	turnErrors     map[string]int // toolFQN → consecutive errors in currentTurn
	sessionErrors  map[string]int // toolFQN → consecutive errors across session
	sessionRepairs map[string]int // toolFQN → corrector invocations since the last clean (unrepaired) success
}

func newSessionErrorState() *sessionErrorState {
	return &sessionErrorState{
		turnErrors:     make(map[string]int),
		sessionErrors:  make(map[string]int),
		sessionRepairs: make(map[string]int),
	}
}

// record atomically applies one tool-outcome update: roll the
// per-turn counters when the turn number changed, then either bump
// both counters (error) or reset them (success). Returns the
// post-update (turnCount, sessionCount) on error and
// (0, 0, wasFailing) on success — the caller uses wasFailing to
// decide whether a self-heal write is warranted.
//
// One lock acquisition for the whole transition guarantees the
// turn-rollover and the increment/reset can't be split by a
// concurrent caller, even though the current agent loop is
// sequential per session: future architectures (parallel sub-agents,
// concurrent tool dispatch) shouldn't break the counter semantics.
// repaired qualifies a success: a repaired success resets the error
// counters like any success but does NOT reset the separate repair
// counter — only a clean, unrepaired success proves the tool's schema and
// the planner agree again.
func (s *sessionErrorState) record(turn int, fqn string, success, repaired bool) (turnCount, sessionCount int, wasFailing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn != s.currentTurn {
		s.currentTurn = turn
		s.turnErrors = make(map[string]int)
	}
	if success {
		wasFailing = s.sessionErrors[fqn] > 0
		delete(s.turnErrors, fqn)
		delete(s.sessionErrors, fqn)
		if !repaired {
			delete(s.sessionRepairs, fqn)
		}
		return 0, 0, wasFailing
	}
	s.turnErrors[fqn]++
	s.sessionErrors[fqn]++
	return s.turnErrors[fqn], s.sessionErrors[fqn], false
}

// recordRepairAttempt bumps the separate per-tool repair counter (one bump
// per corrector invocation) and returns the new count. Reset only by a
// clean, unrepaired success in record.
func (s *sessionErrorState) recordRepairAttempt(fqn string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionRepairs[fqn]++
	return s.sessionRepairs[fqn]
}

// toolErrorTracker holds per-session counter state in a sync.Map keyed
// by sessionID. Not persisted — a process restart resets all counters,
// accepted as a trade-off for not pinning observability state to the DB.
type toolErrorTracker struct {
	sessions sync.Map // sessionID → *sessionErrorState
}

func newToolErrorTracker() *toolErrorTracker {
	return &toolErrorTracker{}
}

// stateFor returns (and lazily creates) the per-session counter
// struct. Safe under concurrent access via LoadOrStore.
func (t *toolErrorTracker) stateFor(sessionID string) *sessionErrorState {
	if val, ok := t.sessions.Load(sessionID); ok {
		return val.(*sessionErrorState)
	}
	candidate := newSessionErrorState()
	actual, _ := t.sessions.LoadOrStore(sessionID, candidate)
	return actual.(*sessionErrorState)
}

// recordToolOutcome is called after every executeCall in the agent
// loop. repaired marks a result that came out of the repair phase
// (tool_call_repair.go) — a repaired success resets the error counters but
// keeps the separate repair counter accumulating. Updates the in-memory
// counters and returns:
//   - a system message to inject into the next LLM iteration when
//     the loop_cap_per_turn threshold trips, otherwise nil
//   - whether the caller should persist a Demoted=true flip
//     (sticky_demotion_threshold tripped this call) or
//     Demoted=false self-heal (success on a previously-failing tool)
//
// The Demoted flag lives in InjectionState, so the demotion side of the
// tracker only does durable work when a state store is wired; the loop-cap
// nudge works without one. The per-turn counter cost is trivial (two
// integer increments + a map lookup per tool call).
func (o *Orchestrator) recordToolOutcome(ctx context.Context, sessionID string, call ToolCall, result ToolResult, repaired bool) *provider.Message {
	if sessionID == "" {
		return nil
	}
	fqn := toolFQN(call.Plugin, call.Action)
	st := o.toolErrorTracker.stateFor(sessionID)
	turnCount, sessionCount, wasFailing := st.record(o.sessionTurnNumber(sessionID), fqn, result.Error == "", repaired)

	if result.Error == "" {
		if wasFailing && o.injectionStateStore != nil {
			o.clearDemotedFlag(ctx, sessionID, fqn)
		}
		return nil
	}

	if o.toolErrorHandling.StickyDemotionThreshold > 0 &&
		sessionCount >= o.toolErrorHandling.StickyDemotionThreshold && o.injectionStateStore != nil {
		o.markDemotedFlag(ctx, sessionID, fqn)
	}

	if o.toolErrorHandling.LoopCapPerTurn > 0 && turnCount >= o.toolErrorHandling.LoopCapPerTurn {
		// Carried back to the agent loop as a transient RoleUser message with
		// the "[system]" textual prefix the other retry nudges use. NOT a
		// RoleSystem message: the native Anthropic adapter folds every
		// RoleSystem message into the request's single `system` field, so a
		// nudge appended mid-array would contaminate the real system prompt.
		return &provider.Message{
			Role:    provider.RoleUser,
			Content: fmt.Sprintf("[system] Tool %s failed %d times in this turn — consider a different approach.", fqn, turnCount),
		}
	}
	return nil
}

// recordRepairAttempt feeds one corrector invocation of the repair phase
// into the separate per-tool repair counter. When the count crosses
// StickyDemotionThreshold the tool is demoted exactly like consecutive
// hard errors would demote it — a tool that only ever works via repair is
// not healthy, it is expensive. Reset by a clean, unrepaired success
// (record); no loop-cap nudge, because a repaired call did succeed and
// telling the LLM it "failed" would be false.
func (o *Orchestrator) recordRepairAttempt(ctx context.Context, sessionID string, call ToolCall) {
	if sessionID == "" {
		return
	}
	fqn := toolFQN(call.Plugin, call.Action)
	n := o.toolErrorTracker.stateFor(sessionID).recordRepairAttempt(fqn)
	if o.toolErrorHandling.StickyDemotionThreshold > 0 &&
		n >= o.toolErrorHandling.StickyDemotionThreshold && o.injectionStateStore != nil {
		o.markDemotedFlag(ctx, sessionID, fqn)
	}
}

// markDemotedFlag flips KnownToolEntry.Demoted=true for fqn. If the
// entry doesn't exist yet (the tool was never loaded), the function still
// inserts a Demoted=true row so the next request's sticky set prefers it
// for eviction. Same defensive copy + warn-and-continue contract as
// persistToolPromotion.
func (o *Orchestrator) markDemotedFlag(ctx context.Context, sessionID, fqn string) {
	o.updateToolDemotion(ctx, sessionID, fqn, true)
}

// clearDemotedFlag flips KnownToolEntry.Demoted=false for fqn. No-op
// when the entry doesn't exist or the flag is already false (avoids
// a wasted write).
func (o *Orchestrator) clearDemotedFlag(ctx context.Context, sessionID, fqn string) {
	o.updateToolDemotion(ctx, sessionID, fqn, false)
}

// updateToolDemotion is the shared core for mark / clear: read state,
// upsert the entry's Demoted flag, write back. Skips the write when
// the requested flag value already matches.
func (o *Orchestrator) updateToolDemotion(ctx context.Context, sessionID, fqn string, demoted bool) {
	existing, err := o.injectionStateStore.GetInjectionState(ctx, sessionID)
	if err != nil {
		slog.WarnContext(ctx, "tool_error_tracking: read state failed, demotion update skipped",
			"component", "orchestrator", "session", sessionID, "tool", fqn, "demoted", demoted, "error", err)
		return
	}

	updated := state.InjectionState{
		KnownTools: append([]state.KnownToolEntry(nil), existing.KnownTools...),
	}
	changed := false
	found := false
	for i := range updated.KnownTools {
		if updated.KnownTools[i].ToolName != fqn {
			continue
		}
		found = true
		if updated.KnownTools[i].Demoted != demoted {
			updated.KnownTools[i].Demoted = demoted
			changed = true
		}
		break
	}
	if !found && demoted {
		// Only create a fresh entry when we're SETTING the flag —
		// clearing a non-existent flag is a no-op (no entry, no
		// demotion to clear).
		updated.KnownTools = append(updated.KnownTools, state.KnownToolEntry{
			ToolName: fqn,
			Demoted:  true,
		})
		changed = true
	}
	if !changed {
		return
	}
	if err := o.injectionStateStore.UpdateInjectionState(ctx, sessionID, updated); err != nil {
		slog.WarnContext(ctx, "tool_error_tracking: write state failed, demotion not persisted",
			"component", "orchestrator", "session", sessionID, "tool", fqn, "demoted", demoted, "error", err)
	}
}

// sessionTurnNumber returns a stable monotonically-increasing turn
// counter for the session. Used as KnownToolEntry.LRURank for sticky
// tool promotion and as the per-turn key for tool-error tracking.
//
// Implemented as the count of user-role messages in the session plus
// one, on the theory that the upcoming user message will become the
// next entry. Imperfect (assistant-led turns aren't counted, store
// errors silently fall back to turn=1) but sufficient for diagnostic
// value.
func (o *Orchestrator) sessionTurnNumber(sessionID string) int {
	sess, err := o.sessions.Get(sessionID)
	if err != nil || sess == nil {
		return 1
	}
	turn := 1
	for _, m := range sess.Messages {
		if m.Role == provider.RoleUser {
			turn++
		}
	}
	return turn
}
