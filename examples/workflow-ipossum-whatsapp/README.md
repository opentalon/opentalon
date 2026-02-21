# Workflow: Ipossum Content Protection via WhatsApp

This example demonstrates how OpenTalon orchestrates an end-to-end content protection workflow using [Ipossum](https://ipossum.com/) (AI-powered content detection and takedown) and WhatsApp as the communication channel — all driven by natural conversation.

## Scenario

A content creator sends photos via WhatsApp to OpenTalon. The system uploads them to Ipossum for monitoring, continuously scans the internet for unauthorized copies, notifies the user on WhatsApp when violations are found, and lets the user approve takedowns directly from the chat.

## Full flow

```
Creator (WhatsApp)                OpenTalon                         Ipossum (app.ipossum.com)
       │                              │                                    │
       │  "Monitor these 2 photos"    │                                    │
       │  📷 photo1.jpg               │                                    │
       │  📷 photo2.jpg               │                                    │
       │ ──────────────────────────▶  │                                    │
       │                              │  ipossum.upload_content            │
       │                              │  (photo1.jpg, photo2.jpg)          │
       │                              │ ──────────────────────────────▶   │
       │                              │  ← content_ids: [c_001, c_002]    │
       │  "Done! Monitoring 2 files.  │                                    │
       │   I'll notify you if         │                                    │
       │   anything appears online."  │                                    │
       │ ◀──────────────────────────  │                                    │
       │                              │                                    │
       │         ... time passes ...  │                                    │
       │                              │                                    │
       │                              │  ipossum.check_violations          │
       │                              │  (content_ids: [c_001, c_002])     │
       │                              │ ──────────────────────────────▶   │
       │                              │  ← 3 violations found             │
       │                              │                                    │
       │  "⚠️ 3 unauthorized copies   │                                    │
       │   of your content found:     │                                    │
       │   1. example-tube.com/x123   │                                    │
       │   2. pirate-host.net/abc     │                                    │
       │   3. shady-site.org/img/99   │                                    │
       │                              │                                    │
       │   Reply TAKEDOWN ALL or      │                                    │
       │   pick numbers to remove."   │                                    │
       │ ◀──────────────────────────  │                                    │
       │                              │                                    │
       │  "TAKEDOWN ALL"              │                                    │
       │ ──────────────────────────▶  │                                    │
       │                              │  ipossum.request_takedown          │
       │                              │  (violation_ids: [v1, v2, v3])     │
       │                              │ ──────────────────────────────▶   │
       │                              │  ← takedowns initiated            │
       │                              │                                    │
       │  "Takedown requests sent     │                                    │
       │   for all 3 violations.      │                                    │
       │   I'll update you when       │                                    │
       │   they're removed."          │                                    │
       │ ◀──────────────────────────  │                                    │
       │                              │                                    │
       │         ... time passes ...  │                                    │
       │                              │                                    │
       │                              │  ipossum.check_takedown_status     │
       │                              │ ──────────────────────────────▶   │
       │                              │  ← 2 removed, 1 pending           │
       │                              │                                    │
       │  "Update: 2 of 3 violations  │                                    │
       │   successfully removed.      │                                    │
       │   1 still pending (pirate-   │                                    │
       │   host.net). I'll keep       │                                    │
       │   checking."                 │                                    │
       │ ◀──────────────────────────  │                                    │
```

## Components

### 1. WhatsApp channel plugin

A [channel plugin](../../docs/design/channels.md) that connects WhatsApp to OpenTalon. Handles:

- Receiving text messages and file attachments (photos, videos)
- Sending notifications and responses back to the user
- File transfer (photos from WhatsApp -> OpenTalon -> Ipossum)

```yaml
channels:
  whatsapp:
    enabled: true
    plugin: "./plugins/opentalon-whatsapp"
    config:
      phone_number_id: "${WA_PHONE_NUMBER_ID}"
      access_token: "${WA_ACCESS_TOKEN}"
      verify_token: "${WA_VERIFY_TOKEN}"
```

### 2. Ipossum tool plugin

A gRPC tool plugin (any language) that wraps the Ipossum API at `app.ipossum.com`.

**Capabilities:**

```yaml
name: ipossum
description: "AI-powered content protection — detect and remove unauthorized content from the web"
actions:
  - name: upload_content
    description: "Upload content (photos/videos) for monitoring"
    parameters:
      - name: files
        description: "List of file paths or binary data to monitor"
        required: true
      - name: content_type
        description: "Type of content: photo, video (default: auto-detect)"
        required: false
      - name: label
        description: "Human-readable label for the content group"
        required: false

  - name: check_violations
    description: "Check for unauthorized copies of monitored content"
    parameters:
      - name: content_ids
        description: "List of content IDs to check (or 'all' for everything)"
        required: false

  - name: get_violation_details
    description: "Get detailed information about a specific violation"
    parameters:
      - name: violation_id
        description: "Violation ID"
        required: true

  - name: request_takedown
    description: "Initiate takedown requests for specific violations"
    parameters:
      - name: violation_ids
        description: "List of violation IDs to take down"
        required: true

  - name: check_takedown_status
    description: "Check the status of pending takedown requests"
    parameters:
      - name: takedown_ids
        description: "List of takedown IDs to check (or 'all')"
        required: false

  - name: list_content
    description: "List all monitored content"
    parameters: []

  - name: get_stats
    description: "Get protection statistics — total monitored, violations found, takedowns completed"
    parameters:
      - name: period
        description: "Time period: week, month, all (default: month)"
        required: false
```

## Configuration

```yaml
# config.yaml
channels:
  whatsapp:
    enabled: true
    plugin: "./plugins/opentalon-whatsapp"
    config:
      phone_number_id: "${WA_PHONE_NUMBER_ID}"
      access_token: "${WA_ACCESS_TOKEN}"
      verify_token: "${WA_VERIFY_TOKEN}"

plugins:
  tools:
    plugin_dir: "./plugins"
    overrides:
      ipossum:
        timeout: "120s"   # scanning can take time

# Scheduled check — poll Ipossum for new violations periodically
scheduler:
  jobs:
    - name: "violation-check"
      interval: "1h"
      action: "ipossum.check_violations"
      notify_channel: "whatsapp"

# Environment variables (never in config):
#   WA_PHONE_NUMBER_ID=...
#   WA_ACCESS_TOKEN=...
#   WA_VERIFY_TOKEN=...
#   IPOSSUM_API_KEY=...
#   IPOSSUM_API_URL=https://app.ipossum.com/api/v1
```

## Workflow memory

After the first successful flow, the orchestrator remembers the pattern:

### Upload and monitor

```yaml
trigger: "monitor photos for unauthorized use"
steps:
  - plugin: ipossum
    action: upload_content
    order: 1
outcome: success
```

### Violation found -> notify -> takedown

```yaml
trigger: "violations found, notify user and handle takedown"
steps:
  - plugin: ipossum
    action: check_violations
    order: 1
  - plugin: ipossum
    action: request_takedown
    order: 2
  - plugin: ipossum
    action: check_takedown_status
    order: 3
outcome: success
```

## Conversation examples

The user interacts entirely via WhatsApp — no dashboard, no browser needed:

| User says | What happens |
|---|---|
| *sends 5 photos* "Protect these" | Upload to Ipossum, start monitoring, confirm |
| "Any violations?" | Check Ipossum, report findings or "all clear" |
| "TAKEDOWN ALL" | Initiate takedowns for all current violations |
| "Only remove #1 and #3" | Selective takedown for specific violations |
| "Status update?" | Check pending takedowns, report progress |
| "How many violations this month?" | Call `get_stats`, summarize |
| "Add this video too" *sends video* | Upload new content, add to monitoring |
| "Stop monitoring photo1.jpg" | Remove content from Ipossum watch list |

## Why this works

- **WhatsApp as the interface** — the user never needs to open a browser or learn a dashboard. Everything happens in a familiar chat.
- **LLM as the brain** — understands natural language ("protect these", "take them all down"), maps it to structured API calls.
- **Ipossum as the engine** — AI-powered detection and automated takedowns across the web.
- **Proactive notifications** — scheduled checks push alerts to WhatsApp when new violations appear. The user doesn't have to ask.
- **Approval in chat** — takedowns require explicit user approval via WhatsApp message. No accidental removals.

## Plugin internals

The Ipossum plugin is a black box to the core. Internally it can use:

- **Ipossum REST API** — direct HTTP calls to `app.ipossum.com/api/v1`
- **Ipossum webhooks** — register a webhook for real-time violation alerts instead of polling
- **File storage** — temporarily store uploaded files before forwarding to Ipossum
