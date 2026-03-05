# Pipeline Execution Architecture

## Problem

Currently, OpenTalon's orchestrator handles multi-step tasks via an agentic loop — the LLM decides what to do at each step, calls tools, gets results, and repeats up to 20 iterations. The `invoke steps` feature provides a rigid sequential chain without LLM involvement.

Neither approach is fault-tolerant. When a tool call fails, the agent loop dumps the error back to the LLM and hopes for the best. There's no structured retry, no error recovery, no progress tracking, and no way to resume a failed multi-step task.

We need a middle ground: **structured pipeline execution with LLM-assisted error recovery**.

## Motivating Example

User prompt:
> Check bug in appsignal, checkout source code, create jira bug ticket, create MR in gitlab with fix

Expected behavior:

1. Planner decomposes this into 4 steps
2. **User reviews the plan** — confirms, edits, or rejects
3. **Check bug in appsignal** -> success -> proceed
4. **Checkout source code** -> success -> proceed
5. **Create jira bug ticket** -> fails: `jira: command not found`
   - Recovery advisor asks LLM: "why did this fail?"
   - LLM responds: "jira CLI is not installed, install it with `brew install jira-cli`"
   - System executes remediation -> retries step -> success -> proceed
6. **Create MR in gitlab with fix** -> success
7. Pipeline complete, return results to user

If retries exceed the configured limit (e.g., 5), the pipeline aborts and reports what succeeded, what failed, and why.

## Architecture Overview

```
+---------------------------------------------------------+
|                     User Prompt                         |
+----------------------------+----------------------------+
                             |
                             v
+---------------------------------------------------------+
|                       PLANNER                           |
|  LLM decomposes prompt into a Pipeline (DAG of Steps)  |
|  Each step: command, plugin/action, success criteria,   |
|  dependencies on prior steps                            |
+----------------------------+----------------------------+
                             |  Proposed Pipeline
                             v
+---------------------------------------------------------+
|                  CONFIRMATION GATE                      |
|  Present plan to user via channel                       |
|  User can: approve / edit steps / reject                |
|  Also triggered after every replan                      |
+----------------------------+----------------------------+
                             |  Approved Pipeline
                             v
+---------------------------------------------------------+
|                 PIPELINE EXECUTOR                       |
|  Walks DAG in topological order                         |
|  Manages state transitions per step                     |
|  Passes context (outputs) between steps                 |
|  Enforces global + per-step retry limits                |
+------------+----------------------------+---------------+
             | execute                    | on failure
             v                            v
+----------------+            +------------------------+
|  STEP RUNNER   |            |   RECOVERY ADVISOR     |
|  Plugin call   |            |   Sends error + ctx    |
|  Shell cmd     |            |   to LLM: "why did     |
|  HTTP request  |            |   this fail? how to    |
|  LLM query     |            |   fix it?"             |
+----------------+            +-----------+------------+
                                          |
                                          v
                              +------------------------+
                              |   REMEDIATOR           |
                              |   Executes the fix     |
                              |   suggested by LLM     |
                              |   Then retries step    |
                              +------------------------+
```

---

## Implementation Status

### Phase 1 — MVP: IMPLEMENTED

Phase 1 is fully implemented and tested. Below is a component-by-component breakdown.

#### Files Created

| File | Status | Description |
|------|--------|-------------|
| `internal/pipeline/pipeline.go` | Done | Pipeline, PipelineState, PipelineConfig, FormatForConfirmation |
| `internal/pipeline/step.go` | Done | Step, StepState, StepResult |
| `internal/pipeline/command.go` | Done | PluginCommand (only command type for Phase 1) |
| `internal/pipeline/context.go` | Done | PipelineContext with thread-safe Set/Get/Merge |
| `internal/pipeline/confirmation.go` | Done | ParseConfirmation — binary approve/reject |
| `internal/pipeline/planner.go` | Done | Planner with LLM decomposition, JSON parsing, markdown fence handling |
| `internal/pipeline/executor.go` | Done | Executor with retry, backoff, FailFast, dependency skipping |
| `internal/pipeline/context_test.go` | Done | 4 tests |
| `internal/pipeline/confirmation_test.go` | Done | 2 tests |
| `internal/pipeline/planner_test.go` | Done | 8 tests |
| `internal/pipeline/executor_test.go` | Done | 7 tests |
| `internal/pipeline/pipeline_test.go` | Done | 4 tests |

#### Files Modified

| File | Status | Description |
|------|--------|-------------|
| `internal/config/config.go` | Done | Added `PipelineOrchestratorConfig` to config struct |
| `internal/orchestrator/orchestrator.go` | Done | Pipeline integration: planner, pending pipelines, confirmation, execution |
| `cmd/opentalon/main.go` | Done | Wiring: config → PipelineConfig → OrchestratorOpts |
| `internal/orchestrator/orchestrator_test.go` | Done | 6 pipeline integration tests |

#### Component Details

**1. Planner** — Done
- Separate LLM call with system prompt listing available tools
- Returns `{"type": "direct"}` or `{"type": "pipeline", "steps": [...]}`
- Handles markdown code fences in LLM output (`extractJSON`)
- Falls back to "direct" on parse failure (safe degradation)
- Debug logging via `LOG_LEVEL=debug` env var

**2. Confirmation Gate** — Done (simplified from design)
- Uses **session-state pattern**: pipeline stored in `pendingPipelines[sessionID]`, next user message parses confirmation. This avoids blocking the channel dispatch goroutine.
- Binary decision only: `y`/`yes` (case-insensitive) → approve; everything else → reject with message "Pipeline cancelled (expected y/yes to confirm)."
- Unix-style prompt: `Proceed? (y)es / (n)o`

**Deviation from design:** The original design had three-way confirmation (approve/edit/reject) and a dedicated `ConfirmationGate` struct with channel sender and timeout. The implementation uses a simpler session-state approach with no edit capability and no timeout. The `Unknown` state (treating non-y/n input as a new request) was removed during development — strict binary is better UX.

**3. Pipeline & Step Types** — Done
- 6 pipeline states: `planned`, `awaiting_confirmation`, `running`, `completed`, `failed`, `rejected`
- 5 step states: `pending`, `running`, `succeeded`, `failed`, `skipped`
- `PipelineConfig`: MaxStepRetries (default 3), StepTimeout (default 60s), FailFast (default true)
- `FormatForConfirmation()` renders human-readable plan with step names, actions, args, dependencies

**Deviation from design:** No `MaxRemediations`, `FailStrategy` enum, `RequireConfirm`, or `ConfirmOnReplan` config fields — these belong to Phase 2/3. Config uses a simple `FailFast bool` instead of `FailStrategy` enum.

**4. Command Types** — Done (Phase 1 scope)
- Only `PluginCommand` implemented (struct with Plugin, Action, Args fields)
- No `Command` interface — unnecessary abstraction for Phase 1 with only one type

**Deviation from design:** The design proposed a `Command` interface with `Execute()`, `Describe()`, `Type()` methods and four implementations (Plugin, Shell, LLM, HTTP). Phase 1 uses a plain struct. The interface can be introduced in Phase 3 when additional command types are needed.

**5. Pipeline Context** — Done
- Thread-safe `sync.RWMutex` protected `map[string]any`
- Keys stored as `stepID+"."+key`
- Methods: `NewContext()`, `Set()`, `Get()`, `Merge()`
- Step outputs automatically merged into context on success

**6. Executor** — Done
- Sequential execution respecting `DependsOn` ordering
- Per-step retry with exponential backoff (500ms × attempt)
- Per-step timeout via `context.WithTimeout`
- FailFast: abort pipeline on first unrecoverable failure
- When FailFast=false: skip failed step's dependents, continue rest, report overall failure
- `StepRunnerFunc` adapter pattern: bridges pipeline package to orchestrator's `executeCall()` without import cycle
- Returns `ExecutionResult` with success flag, human-readable summary, and per-step records

**7. Orchestrator Integration** — Done
- **Block A** (top of `Run()`): checks `pendingPipelines[sessionID]` for pending confirmation
- **Block B** (after content preparers): calls planner, creates pipeline if >1 steps, stores in pendingPipelines
- `executePipeline()`: creates StepRunnerFunc adapter, runs executor, records all tool calls/results in session history
- `plannerLLMAdapter`: converts orchestrator LLMClient ↔ pipeline LLMClient (avoids import cycle)
- `capabilitiesToPlannerInfo()`: converts registry capabilities to planner-friendly format
- Debug logging throughout with `[pipeline]` prefix

**8. Config** — Done
```yaml
orchestrator:
  pipeline:
    enabled: true          # default false
    max_step_retries: 2    # default 3
    step_timeout: "30s"    # Go duration, default "60s"
```

#### Known Limitations (Phase 1)

1. **Console multi-line input**: The console channel sends each line as a separate message. Multi-line requests split across messages cause the planner to receive partial input. This is a console-specific limitation; Slack/Discord send complete messages. No clean fix without breaking the simple line-by-line console UX.

2. **Step output templating not resolved**: The planner sometimes puts template references in args (e.g., `{{steps.1.output.error_id}}`), but these are passed literally to plugins — no template engine resolves them. Planned for Phase 3.

3. **Planner quality depends on LLM**: Weaker models (e.g., DeepSeek) sometimes return 1 step for multi-step requests, causing fallback to the normal agent loop. This is expected behavior (safe degradation) but reduces pipeline usefulness.

4. **No parallel step execution**: Steps with independent dependencies run sequentially. Planned for Phase 3.

5. **No pipeline persistence**: Pipelines exist only in memory. Process restart loses pending/running pipelines. Planned for Phase 4.

---

## Remaining Phases

### Phase 2 — Smart Recovery (NOT STARTED)

| Component | Description |
|-----------|-------------|
| Recovery Advisor | LLM-assisted error diagnosis: sends error + context to LLM asking "why did this fail? how to fix it?" |
| Remediator | Executes the fix suggested by Recovery Advisor, then retries the step |
| Circuit Breaker | Stops retrying when same error occurs 3x or LLM keeps suggesting same failing fix |
| `RemediationRecord` | Tracks error, diagnosis, action, result, timestamp per recovery attempt |
| `Step.Remediations` field | History of recovery attempts per step |
| `PipelineConfig.MaxRemediations` | Max recovery advisor attempts per step |

### Phase 3 — Adaptive Execution (NOT STARTED)

| Component | Description |
|-----------|-------------|
| Replan strategy | `FailStrategy` enum: `fail_fast` / `continue_on_error` / `replan`. On failure, LLM replans remaining steps with user confirmation |
| Plan editing | User can modify steps before approval (add, remove, reorder) |
| Step output templating | Resolve `{{steps.X.output.Y}}` references in step args before execution |
| Parallel step execution | Run independent steps concurrently (no mutual dependencies) |
| `Command` interface | Abstract over PluginCommand, ShellCommand, LLMCommand, HTTPCommand |
| `ConfirmOnReplan` config | Require user confirmation after replan |

### Phase 4 — Production Hardening (NOT STARTED)

| Component | Description |
|-----------|-------------|
| Plugin permissions | Per-plugin config: allowed actions, shell/network access, host restrictions, path restrictions |
| Pipeline-level permission overrides | Per-channel/per-user scoping of what plugins are available |
| Startup validation | Validate `required_env` and `required_cli` at plugin load time |
| Pipeline persistence | Survive process restarts — store pipeline state in DB/file |
| Pipeline history & audit log | Record all pipeline executions for review |
| Cost tracking | LLM tokens spent per pipeline |
| Rate limiting | Per-plugin invocation limits |
| `require_approval` per plugin | Force user confirmation for specific plugin invocations |

---

## Core Components (Full Design Reference)

### 1. Planner

Takes a user prompt and asks the LLM to decompose it into a structured pipeline — a DAG (directed acyclic graph) of steps with dependencies. The output is a `Pipeline` struct.

The plan is a "best guess." The system must be ready for it to be wrong (see Replan strategy in Phase 3).

### 2. Confirmation Gate

**Every plan must be confirmed by the user before execution begins.** This includes initial plans and replans (Phase 3).

The Confirmation Gate sends the proposed plan back to the user via their channel (Slack, console, etc.) in a human-readable format and waits for a response.

```
I've created a plan with the following steps:

1. **Check bug in AppSignal**
   Action: `appsignal.get_error`
   Args: error_id=4521
2. **Checkout source code**
   Action: `git.clone`
   Depends on: 1
3. **Create Jira bug ticket**
   Action: `jira.create_issue`
   Depends on: 1
4. **Create MR with fix**
   Action: `gitlab.create_mr`
   Depends on: 2, 3

Proceed? (y)es / (n)o
```

Phase 1: approve (y/yes) or reject (anything else).
Phase 3: adds edit capability.

### 3. Pipeline

```go
type Pipeline struct {
    ID        string
    Steps     []*Step
    State     PipelineState
    Config    PipelineConfig
    Context   *PipelineContext
    CreatedAt time.Time
}

type PipelineConfig struct {
    MaxStepRetries    int            // default 3
    StepTimeout       time.Duration  // default 60s
    FailFast          bool           // Phase 1: only strategy implemented
    // Phase 2+:
    // MaxRemediations   int
    // FailStrategy      FailStrategy  // fail_fast | continue_on_error | replan
    // RequireConfirm    bool
    // ConfirmOnReplan   bool
}
```

### 4. Step

```go
type Step struct {
    ID         string
    Name       string
    Command    *PluginCommand
    DependsOn  []string
    State      StepState
    Attempts   int
    MaxRetries int           // -1 = use pipeline default
    Result     *StepResult
    // Phase 2+:
    // Remediations []RemediationRecord
}
```

### 5. Command

Phase 1: `PluginCommand` struct only.
Phase 3: `Command` interface with PluginCommand, ShellCommand, LLMCommand, HTTPCommand implementations.

### 6. Pipeline Context

Shared state between steps. Each step's output is merged into the context, making it available to subsequent steps.

Phase 3 adds template resolution for `{{steps.X.output.Y}}` references in step args.

### 7. Recovery Advisor (Phase 2)

When a step fails, sends error context to the LLM: "This step failed with this error. Given the pipeline context, why did it fail and what should be done to fix it?"

### 8. Circuit Breaker (Phase 2)

Prevents burning retries on the same broken thing. If the same error occurs 3 times in a row, or the LLM keeps suggesting the same failing remediation, the circuit breaker trips.

---

## Failure Strategies

| Strategy | Status | Behavior | Use Case |
|---|---|---|---|
| **FailFast** | Phase 1 ✅ | Abort pipeline on first unrecoverable failure | Critical workflows where partial completion is dangerous |
| **ContinueOnError** | Phase 1 ✅ (partial) | Skip failed step and its dependents, continue rest | Best-effort workflows where some steps are optional |
| **Replan** | Phase 3 | Ask LLM to replan remaining steps, user confirms new plan | Most flexible — handles unexpected situations gracefully |

Note: ContinueOnError works in the executor (FailFast=false) but is not exposed as a named strategy in config yet. Config only has `FailFast bool`.

---

## Integration with Existing OpenTalon

- **Orchestrator** gets a new code path: if the Planner decomposes a prompt into multiple steps, hand off to the Pipeline Executor. Single-step requests still use the existing agent loop.
- **Plugin system** stays unchanged — `PluginCommand` calls `plugin.Client.Execute()` via gRPC as it does today.
- **Sessions** store pipeline state for inspection and resume (Phase 4: persistence).
- **Recovery Advisor** wraps the existing LLM provider with a specific prompt template (Phase 2).
- **Channels** don't need changes — confirmation works via session state, not channel protocol.

---

## Key Design Principles

1. **Confirm before executing.** Every plan goes through user confirmation. The cost of one prompt is trivial compared to undoing a pipeline that created wrong Jira tickets and merged bad MRs.

2. **Replan > Retry.** Blindly retrying the same command is usually useless. Diagnose first, remediate, then retry. If remediation fails too, replan the whole approach. (Phase 1 has retry only; Phase 2 adds diagnosis; Phase 3 adds replan.)

3. **DAG, not a linked list.** Real-world tasks have branching and conditional dependencies. A flat command chain breaks down quickly.

4. **Steps compose via context, not coupling.** Steps don't know about each other — they read from and write to a shared context. This makes them reorderable and replaceable.

5. **Safe degradation.** If the planner fails to parse, returns "direct", or produces only 1 step — the system falls back to the normal agent loop. Pipeline is additive, never breaks existing functionality.

6. **Fail loudly with history.** When a pipeline fails, the user sees: what succeeded, what failed, how many retries were attempted. No silent failures.

7. **Don't over-plan.** The initial plan is a best guess. The system must handle the plan being wrong. Over-investing in perfect upfront planning is a trap — invest in good recovery instead.
