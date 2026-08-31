# Plugin Gateway: External Callers and the Live HostCaller

## Overview

Core exposes exactly one externally-reachable `PluginService` surface: a
per-plugin gRPC gateway, opt-in via `grpc_port` in that plugin's config
(`internal/config/config.go`). It lets something outside the orchestrator's
LLM tool-call loop — a CI workflow, an external service — call a loaded
plugin's action directly, over the network, without going through a chat
session.

A plugin action that needs to call back into the host mid-request (run a
model completion, dispatch another action) needs a live `HostCaller`
(`pkg/plugin/streaming.go`), which only exists on the bidirectional
`ExecuteBidi` gRPC stream — plain unary `Execute` has no callback channel.
The gateway picks between the two per plugin, the same way the orchestrator
picks for its own internal tool calls: `PluginCapability.SupportsCallbacks`.

```
External caller (e.g. talooner's GitHub Action)
        │  gRPC PluginService.Execute
        ▼
┌────────────────────────────────────────────┐
│  gateway (internal/plugin/gateway.go)       │
│  one TCP listener per grpc_port plugin      │
└───────────────────┬──────────────────────────┘
                     │
     SupportsCallbacks? ──No──▶ Client.ExecuteRaw (unary, byte-for-byte)
                     │
                    Yes
                     │
                     ▼
          Client.ExecuteBidiRaw (bidirectional stream)
                     │
     CallbackRequest frames ──▶ Manager.callbackHandler() ──▶ orchestrator.RunActionResult
                     │
                     ▼
            ToolResultResponse
```

## Dispatch

`gateway.Execute` (`internal/plugin/gateway.go`):

```go
func (g *gateway) Execute(ctx context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	if g.client.Capability().SupportsCallbacks {
		if cb := g.mgr.callbackHandler(); cb != nil {
			return g.client.ExecuteBidiRaw(ctx, req, cb)
		}
		slog.Warn("plugin gateway: callback handler not wired yet, falling back to unary Execute", ...)
	}
	return g.client.ExecuteRaw(ctx, req)
}
```

`client.Capability()` is the same `PluginCapability` the plugin declared at
`Init`/`Capabilities` time and that the `ToolRegistry` holds — the gateway
and the orchestrator's internal dispatch (`internal/orchestrator/orchestrator.go`,
`o.registry.GetCapability(call.Plugin).SupportsCallbacks`) read the identical
signal.

Both RPCs the gateway can take forward the caller's request byte-for-byte:
neither injects credential headers from `ctx` (`profile.FromContext`), the
way the orchestrator's internal `Execute`/`ExecuteBidi` do — there is no
`profile.Profile` on an inbound external gRPC call, so any
`CredentialHeaders` the caller wants applied must already be on the request
it sent.

- `Client.ExecuteRaw` (`internal/plugin/client.go`) — `c.client.Execute(ctx, req)`, unchanged from the request.
- `Client.ExecuteBidiRaw` (`internal/plugin/client.go`) — opens `ExecuteBidi`, sends `req` as the initial `HostMessage`, and returns the plugin's final `ToolResultResponse`. Shares its stream send/recv/callback-dispatch loop with the orchestrator's own `Client.ExecuteBidi` via the private `executeBidiStream` helper — the two differ only in how the outbound request is built (from a caller-supplied `*pluginpb.ToolCallRequest` vs. from an `orchestrator.ToolCall` plus ctx-derived credential headers).

## Callback dispatch and identity

Every `CallbackRequest` frame the plugin sends back — on either the internal
or external path — is handled by `Client.handleCallback`, which calls
`cb.RunActionResult(ctx, plugin, action, args)`. `cb` is an
`orchestrator.CallbackHandler`; in production it is the `*Orchestrator`
itself (`RunAction`/`RunActionResult` in `internal/orchestrator/orchestrator.go`
dispatch through `executeCall`, the same path a real tool call takes — model
routing, credential injection, and quota accounting for that action are
identical to an internal call).

`applyCallbackIdentity` (`internal/plugin/client.go`) lets a plugin carry
actor/group/session identity into a callback via reserved arg keys
(`contextargs.Callback*`), stripping them from the args the target action
actually sees. This is the only identity path a callback has — `ctx` itself
carries a `profile.Profile` on the internal orchestrator path, but never on
the external gateway path, since the inbound gRPC call has no profile to
begin with. A callback whose downstream action needs `profile.Credentials`
must have that identity threaded through by the plugin via
`contextargs.Callback*`; one that doesn't will run with none. Today's only
gateway caller with `SupportsCallbacks` (`talooner-plugin`, via
`generate_ruleset`/`llm_review`) only calls the host's built-in
`_subprocess` action, which uses the host's own configured model client —
no per-tenant credentials involved.

## Gateway serving lifecycle

`Manager` builds and loads plugins (including any `grpc_port` gateways)
before the orchestrator exists — the orchestrator depends on the
`ToolRegistry` the manager populates while loading, so this ordering can't
be reversed. `Manager.SetCallbackHandler` wires the orchestrator in as
`CallbackHandler` once it's built, in `cmd/opentalon/main.go`, right after
`orch := orchestrator.NewWithRules(...)`.

A gateway must never accept a connection before a `CallbackHandler` exists
for it to route callbacks to — otherwise a `SupportsCallbacks` plugin
reached through it would silently fall back to unary `Execute` and run with
`host == nil`. `startGateway` (`internal/plugin/gateway.go`) only binds the
listener and registers the `PluginService` handler; it does not start
accepting connections. `gateway.startServing()` does that, and is invoked
exactly once per gateway:

- Immediately, in `Manager.loadLocked`, if `Manager.cbHandler` is already
  set (true for any plugin loaded after startup — the retry loop, `Reload`).
- Otherwise the gateway is appended to `Manager.pendingGateways`, and
  `Manager.SetCallbackHandler` calls `startServing` on every queued gateway
  once it sets the handler (true for every gateway started during the
  initial `LoadAll`, since that runs before the orchestrator exists).

`gateway.stop()` calls both `server.GracefulStop()` and closes the listener
directly — `GracefulStop` alone only closes listeners the server has
actually `Serve`'d, so a gateway stopped while still in
`pendingGateways` (never served) would otherwise leak its bound port.

## Security

`gateway.go`'s forwarding is byte-for-byte on both the unary and bidi paths:
it does not inspect, gate, or rate-limit the request, so auth is entirely
the plugin's own concern (e.g. an API key carried as a regular `Execute`
arg, as `talooner-plugin` does). Routing the callback leg through
`ExecuteBidi` does not introduce a new trust boundary — a callback only runs
after the plugin's own per-request tenant auth has already gated the call,
same as every other plugin action.

## Configuration

```yaml
plugins:
  talooner:
    enabled: true
    github: "opentalon/talooner-plugin"
    grpc_port: 50100   # opt-in: expose Execute over this port, forwarding to the plugin unchanged
```

`grpc_port: 0` (the default, omitted) disables the gateway for that plugin;
it remains reachable only through the orchestrator's normal LLM tool-call
loop.
