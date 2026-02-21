# Examples

Real-world workflow examples showing how OpenTalon plugins work together, orchestrated by the LLM.

## Workflows

| Example | Plugins + Channels | What it does |
|---|---|---|
| [Jira + GitLab](workflow-jira-gitlab/) | `jira`, `gitlab` | Turn a Jira ticket into a GitLab merge request — read ticket, create branch, commit, open MR, link back to Jira |
| [Jira + GitHub](workflow-jira-github/) | `jira`, `github` | Same flow but for GitHub — read ticket, create branch, commit, open PR, link back to Jira |
| [Ipossum + WhatsApp](workflow-ipossum-whatsapp/) | `ipossum` + WhatsApp channel | Content protection via chat — send photos on WhatsApp, monitor the web for unauthorized copies, get alerts, approve takedowns from the conversation |

## How workflows work

The LLM orchestrator chains plugin calls to accomplish complex tasks. Each plugin is a standalone binary (written in any language) that exposes actions via gRPC. The LLM decides which plugins to call, in what order, and how to pass data between them.

```
User request
    │
    ▼
LLM Orchestrator
    ├──▶ Plugin A: action 1 ──▶ result
    ├──▶ Plugin B: action 2 ──▶ result
    ├──▶ Plugin A: action 3 ──▶ result
    └──▶ Plugin B: action 4 ──▶ result
    │
    ▼
Response to user
```

After a successful multi-step workflow, the orchestrator **saves the pattern** to memory. Next time a similar request arrives, the LLM already knows the exact sequence and executes faster.

## Building your own

Each example includes:

- **Flow diagram** — step-by-step visualization of the workflow
- **Plugin capabilities** — YAML declarations of all actions and parameters
- **Configuration** — how to set up the plugins in `config.yaml`
- **Workflow memory** — the saved pattern the orchestrator learns
- **Variations** — other workflows the same plugins enable

To create a new workflow, you only need plugins that expose the right actions. The LLM figures out how to chain them — you don't write glue code.

## Related documentation

- [Plugin architecture](../docs/design/plugins.md) — how to build gRPC plugins and Lua hooks
- [Channel framework](../docs/design/channels.md) — how to build messaging platform adapters
- [State & orchestrator](../docs/design/state.md) — session management, workflow memory, plugin state
- [Provider routing](../docs/design/providers.md) — smart model selection and failover
