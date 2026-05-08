# WebSocket Message Types

Every outbound WebSocket message is a JSON object with these fields:

```json
{
  "conversation_id": "abc123",
  "thread_id": "",
  "content": "You have 55 items.",
  "metadata": {
    "type": "assistant",
    "error_code": ""
  }
}
```

The `metadata.type` field tells the frontend **what kind of message this is**, so it can render it appropriately and translate the content for i18n.

---

## Message Types

### `assistant` (default)

Normal LLM response. No special `type` metadata is set (or absent). The `content` is the LLM's natural language answer.

```json
{ "type": "assistant" }
```

Frontend: render as a chat bubble from the assistant.

---

### `system`

A server-side command or action was executed without going through the LLM. The response is a direct result from the system.

```json
{
  "type": "system",
  "action": "clear_session"
}
```

| `action` value | Description |
|---|---|
| `clear_session` | Session was cleared (`/clear` or `/new`) |
| `list_commands` | Available commands listed (`/commands`) |
| `show_config` | Config displayed (`/show config`) |
| `set_prompt` | Runtime prompt updated (`/set prompt`) |
| `install_skill` | Skill installed (`/install skill`) |
| `reload_mcp` | MCP plugin reloaded (`/reload mcp`) |
| `profile_assign` | Plugin assigned to group |
| `profile_revoke` | Plugin revoked from group |
| `profile_list_group` | Group plugins listed |
| `pipeline_cancelled` | User rejected a multi-step pipeline |

Frontend: render as a system notification (not a chat bubble). The `action` value is stable and can be used as an i18n key.

---

### `confirmation`

The system is asking the user to confirm a multi-step pipeline before execution. The `content` is a human-readable description of the planned steps.

```json
{
  "type": "confirmation",
  "prompt_type": "confirmation",
  "pipeline_id": "abc-123",
  "options": "approve,reject"
}
```

| Field | Description |
|---|---|
| `pipeline_id` | Unique ID of the pending pipeline |
| `options` | Comma-separated list of valid responses |

Frontend: render as a confirmation dialog with approve/reject buttons. The user's next message determines the pipeline's fate.

---

### `error`

An error occurred. The `content` is a human-readable fallback message (English). The `error_code` is a stable machine-readable code for i18n.

```json
{
  "type": "error",
  "error_code": "timeout"
}
```

| `error_code` | Default English message | When |
|---|---|---|
| `timeout` | The request timed out. Please try again. | LLM or API call exceeded deadline |
| `rate_limited` | I'm being rate-limited right now. Please try again in a moment. | Provider returned 429 |
| `context_length_exceeded` | Sorry, this conversation has grown too long for the model to process. Please start a new conversation or clear the session. | Input exceeded model's context window |
| `internal_error` | Something went wrong processing your message. Please try again or start a new conversation. | Any other unrecognized error |
| `token_required` | profile token required | WebSocket connection missing auth token |
| `auth_failed` | authentication failed | Token validation failed |
| `token_limit_exceeded` | token limit reached, please try again later | User's token spend limit exceeded |
| `empty_content` | I received your message but couldn't read its content. Could you try sending it as text? | Empty message with no file attachments |
| `guard_blocked` | Request blocked: guard {name} failed. | Content guard rejected the message |

Frontend: use `error_code` as an i18n translation key (e.g., `errors.timeout`, `errors.rate_limited`). Fall back to `content` if no translation is available.

---

### `_typing` (typing indicator)

Sent periodically (~25s) while the system is processing a request. Keeps the WebSocket alive and signals that work is in progress.

```json
{
  "content": "",
  "metadata": {
    "_typing": "true"
  }
}
```

No `type` field. Presence of `_typing` key is the signal.

Frontend: show a typing indicator. Stop when a real message arrives.

---

## i18n Integration

The recommended pattern for frontend i18n:

```typescript
function renderMessage(msg: OutboundMessage) {
  const type = msg.metadata?.type;
  const errorCode = msg.metadata?.error_code;
  const action = msg.metadata?.action;

  if (msg.metadata?._typing === "true") {
    return showTypingIndicator();
  }

  if (type === "error" && errorCode) {
    // Use error_code as translation key, fall back to content
    return showError(t(`errors.${errorCode}`, msg.content));
  }

  if (type === "system" && action) {
    // Use action as translation key for system notifications
    return showSystemNotification(t(`system.${action}`, msg.content));
  }

  if (type === "confirmation") {
    return showConfirmationDialog(msg.content, msg.metadata.options);
  }

  // Default: render as assistant message
  return showAssistantMessage(msg.content);
}
```

## Metadata Field Reference

| Key | Values | Present when |
|---|---|---|
| `type` | `system`, `error`, `confirmation`, `assistant` | Always (absent = `assistant`) |
| `error_code` | See error table above | `type=error` |
| `action` | See system action table above | `type=system` |
| `prompt_type` | `confirmation` | `type=confirmation` |
| `pipeline_id` | UUID string | `type=confirmation` |
| `options` | Comma-separated (e.g., `approve,reject`) | `type=confirmation` |
| `_typing` | `true` | Typing indicator only |
