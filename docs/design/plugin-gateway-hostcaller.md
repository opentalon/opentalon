# Plugin gateway: bridge external callers to a live `HostCaller`

## Status

Implemented in this repo (`internal/plugin/gateway.go`, `client.go`,
`manager.go`, `cmd/opentalon/main.go`). Rollout steps 2–4 (Helm image bump,
`talooner` tenant `features` list, CLI verification against a live cluster)
are still outstanding — they need an infra change and a real deployment,
not just code in this repo.

One deviation from the design below worth flagging for the next reader:
rather than converting the inbound `*pluginpb.ToolCallRequest` to an
`orchestrator.ToolCall` and calling `client.ExecuteBidi` (which rebuilds
`CredentialHeaders` from the request context's `profile.Profile`, not from
the request itself — there is no profile on this external path), the
gateway calls a new `Client.ExecuteBidiRaw(ctx, req, cb)` that forwards the
raw request byte-for-byte over the bidi stream, same as `ExecuteRaw` does
for unary. Keeps the "byte-for-byte forward" gateway invariant intact
instead of silently dropping any `CredentialHeaders` an external caller set
directly on the request.

## Problem

Any plugin action that needs to call back into the host mid-request (run a
model completion, dispatch another action) needs a live `HostCaller`
(`pkg/plugin/streaming.go`). That only exists on the `ExecuteBidi` gRPC
stream. Plain unary `Execute` has no callback channel — a handler invoked
that way always gets `host == nil`.

`internal/plugin/gateway.go` is core's **only** externally-reachable
`PluginService` surface (opt-in via a plugin's `grpc_port` config,
`internal/config/config.go:293`). Its `Execute` method:

```go
// internal/plugin/gateway.go:30
func (g *gateway) Execute(ctx context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	return g.client.ExecuteRaw(ctx, req)
}
```

`ExecuteRaw` (`internal/plugin/client.go:126`) is a byte-for-byte unary
forward — `c.client.Execute(ctx, req)`, never `ExecuteBidi`. So **every**
external caller going through the gateway gets `host == nil` in the plugin,
no matter how the plugin itself is deployed or configured. This is not a
plugin-side bug and not fixable from the plugin's side: `talooner-plugin`
already implements `StreamingHandler`/`ExecuteWithCallbacks` correctly
(`talooner-plugin/internal/service/service.go:177-195`) and already declares
`SupportsCallbacks` — the gateway just never dials the RPC that would use it.

Confirmed this is the live topology, not a hypothetical: the deployed Helm
config sets `plugins.talooner.grpc_port: 50100` specifically so `talooner`'s
GitHub Actions workflow can call the plugin directly through this gateway —
its own comment in that config already names this file. `talooner
generate_ruleset` and `llm_review` are therefore structurally incapable of
reaching a model today, in this or any deployment that uses the external
gateway, until this is fixed.

### Why this wasn't caught earlier

`talooner-plugin`'s `host == nil` fallback is deliberate and well-documented
(`talooner-plugin/docs/llm-review.md:65`, "Standalone TCP mode has no
host") — but that line describes the plugin's own `TALOONER_GRPC_PORT`
standalone mode (`talooner-plugin/cmd/talooner-plugin/main.go:20-33`), a
different code path than the one actually in use here. The deployed plugin
runs core-managed (git-cloned by `internal/bundle/fetch.go`, spawned
normally), *not* in its own standalone TCP mode — `SetStandalone` is never
called on this path. The gap is entirely in the gateway's unary-only
forwarding, one level up from where the existing docs looked.

## Design

### 1. `internal/plugin/gateway.go` — dispatch via `ExecuteBidi` when the plugin supports it

The orchestrator already makes this same decision for its own internal tool
calls:

```go
// internal/orchestrator/orchestrator.go:4678
if cap, hasCap := o.registry.GetCapability(call.Plugin); hasCap && cap.SupportsCallbacks {
    // ... use ExecuteBidi
}
```

`gateway` needs the same capability lookup (the `toolRegistry` it can reach
through the manager already has this — `PluginCapability.SupportsCallbacks`,
`internal/orchestrator/types.go:68`) and, when true, call
`client.ExecuteBidi(ctx, call, cb)` (`internal/plugin/client.go:168`) instead
of `ExecuteRaw`. Translate `*pluginpb.ToolCallRequest` → `orchestrator.ToolCall`
and `orchestrator.ToolResult` → `*pluginpb.ToolResultResponse` at the
boundary — `internal/plugin/client.go`'s existing `Execute`/`ExecuteContext`
(around line 130-150) already does this conversion for the internal path and
is the reference implementation.

### 2. The missing piece: a `CallbackHandler` the gateway can hand to `ExecuteBidi`

`orchestrator.CallbackHandler` (`internal/orchestrator/registry.go:34-45`) is
"a thin wrapper over `RunAction`" that the orchestrator already implements
for its own internal use. The gateway needs a reference to that same
implementation so a callback answered through the external gateway goes
through the **identical** model-routing/credential/quota path as an internal
call — no new secret plumbing, no new LLM client, just reuse what already
exists.

### 3. Init-order problem in `cmd/opentalon/main.go`

```
pluginManager := plugin.NewManager(toolRegistry)   // line 613 — gateways start here as plugins load
...
orch := orchestrator.NewWithRules(...)              // line 822 — CallbackHandler doesn't exist yet
```

The manager (and any `grpc_port` gateways it starts) is built and plugins are
loaded *before* `orch` exists. Can't fix this with a plain constructor
argument. Needs a settable/lazy binding:

- Add a setter, e.g. `pluginManager.SetCallbackHandler(cb orchestrator.CallbackHandler)`,
  called immediately after `orch := orchestrator.NewWithRules(...)` at line
  822.
- Gateways don't actually serve traffic until `Serve()` is called later in
  `main`, so this ordering is safe — the setter just needs to run before the
  first request can arrive.
- Do **not** reorder construction to build `orch` first — `orch` itself
  depends on `toolRegistry`, which the plugin manager populates as it loads
  plugins. This is a genuine two-way dependency, resolved with a setter, not
  a reorder.

### 4. Security — review, not redesign

`gateway.go`'s existing doc comment: it forwards "byte-for-byte," doesn't
inspect/gate/rate-limit, auth is "entirely the plugin's own concern." That
doesn't change here. `talooner-plugin` already authenticates the tenant via
an API-key request arg and enforces its own per-tenant quota
(`talooner-plugin/internal/auth`, `internal/service/llm_review.go:38-56`)
before any callback would fire. Routing the callback leg through
`ExecuteBidi` doesn't introduce a new trust boundary — the callback only
gets a chance to run after the plugin's own per-request tenant auth already
gated the call, same as it does for every other action today. Worth one
sentence in the PR description, not a new auth layer.

### 5. Tests

- `internal/plugin`: a fake `StreamingHandler`-implementing plugin behind
  `gateway.go`, asserting the gateway picks `ExecuteBidi` when
  `SupportsCallbacks` is true and falls back to unary `Execute` otherwise;
  a fake `CallbackHandler` answers the callback and the result round-trips
  correctly to the external caller.
- `cmd/opentalon`: a smoke test (or manual check) that `SetCallbackHandler`
  is actually wired before `Serve()` starts accepting gateway traffic.
- No changes expected to `talooner-plugin`'s existing `host != nil` branch
  tests (`generate_ruleset.go`, `llm_review.go`) — this fix is what makes
  that branch reachable from outside the cluster for the first time.

## Rollout / deploy order

Only `opentalon` needs a code change. `talooner-plugin` and `talooner` (the
CLI) are already correctly built for this — verify, don't modify, unless
step 4 below surfaces something.

1. **`opentalon`** — implement gateway.go + manager.go + main.go wiring
   above, land the PR, tag a release.
2. **Infra (Helm chart / cluster config)** — bump the deployed `opentalon`
   core image to the new tag; redeploy. While in there, add
   `"generate_ruleset"` to the `talooner` tenant's `features` list (currently
   only `["llm_review"]`) so `whoami` reports it accurately — cosmetic, the
   action itself isn't gated on this list today, but fix it for consistency.
   No `talooner-plugin` version bump needed — its pinned commit ref is
   unchanged; it already implements `StreamingHandler` correctly and just
   starts actually receiving `ExecuteBidi` calls once core sends them.
3. **`talooner-plugin`** — no code change required for this fix. Only
   revisit if gateway-side integration testing surfaces something
   `ExecuteWithCallbacks` doesn't handle correctly under real callback
   traffic (it's only ever been exercised via the orchestrator's internal
   path before now).
4. **`talooner` (CLI)** — no code change, no redeploy. It only ever spoke
   plain unary `Execute` to `OPENTALON_HOST`
   (`talooner/internal/cluster/cluster.go:274-297`) and always will — the
   fix is entirely transparent from here. Verify with:
   ```
   talooner onboard --repo opentalon/opentalon
   ```
   and confirm the output says a ruleset was generated (`source: "llm"`),
   not `generate_ruleset fell back to the starter ruleset`.

## Open items for whoever implements this

- Confirm `internal/plugin/manager.go`'s `PluginCapability`/`toolRegistry`
  lookup is accessible from `gateway.go` without a new import cycle (it
  should be — `client.go` already imports `orchestrator` for
  `CallbackHandler`/`ToolCall`/`ToolResult`).
- Decide the exact `SetCallbackHandler` call site in `cmd/opentalon/main.go`
  — right after `orch := orchestrator.NewWithRules(...)` at line 822 looks
  correct, but confirm nothing between manager construction (line 613) and
  that point can already receive gateway traffic (it shouldn't — `Serve()`
  is later — but verify).
- No changes needed to `k8s-operator` — this is core-internal wiring, not a
  new CRD field or Helm value.
