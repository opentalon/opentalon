# WebSocket Integration

The `websocket-channel` plugin lets any browser or HTTP client connect to OpenTalon over a persistent WebSocket connection. It is a standalone binary plugin — no changes to the core are required.

## How it works

1. Your website embeds a WebSocket client (or the provided demo UI via iframe).
2. After the user logs in, the client connects to the channel with a **profile token** in the URL.
3. The channel injects the token into every `InboundMessage` as `metadata["profile_token"]`.
4. OpenTalon verifies the token via the [Profiles & WhoAmI](profiles.md) system and scopes the session to that entity.
5. Responses are returned as HTML (`FormatHTML`) — ready to render in the browser.

## Installation

```bash
git clone https://github.com/opentalon/websocket-channel
cd websocket-channel
make build
# produces: ./websocket-channel
```

## Configuration

Add the channel to your OpenTalon config:

```yaml
channels:
  - name: websocket
    plugin: ./websocket-channel
    config:
      addr: "0.0.0.0:9000"   # host:port to listen on
      path: "/ws"             # WebSocket endpoint path
      cors_origins:           # allowed browser origins (omit to allow all — dev only)
        - "https://mysite.com"
```

OpenTalon launches the binary as a subprocess and connects to it over a Unix socket. The `config` block is passed to the channel via gRPC before it starts.

## Wire protocol

**Connect**

```
ws://host:9000/ws?token=<profile_token>
```

The token can also be passed as an HTTP header on the upgrade request:

```
Authorization: Bearer <profile_token>
```

If no token is present the connection is rejected with HTTP 401.

**Client → server** (JSON text frame)

```json
{
  "content": "What is the weather in London?",
  "files": [
    {
      "name": "report.pdf",
      "mime_type": "application/pdf",
      "data": "<base64-encoded bytes>"
    }
  ]
}
```

`files` is optional. Supported types: images, PDF, CSV. Maximum file size: 20 MB per attachment.

**Server → client** (JSON text frame)

```json
{
  "conversation_id": "3f2a1b...",
  "content": "<p>The weather in London is <strong>12°C</strong>...</p>"
}
```

`content` is HTML. Each WebSocket connection gets its own `conversation_id`; use it to correlate responses when handling multiple tabs or connections.

## Running the demo UI

The repository ships a zero-dependency HTML demo and a small Python server:

```bash
# Terminal 1 — start OpenTalon with the websocket channel
opentalon --config config.yaml

# Terminal 2 — serve the demo UI
python3 demo/serve.py
```

Open `http://localhost:8080`, enter your WebSocket URL and profile token, and start chatting. The demo supports text messages and file attachments (CSV, PDF, images).

## Website integration

After login, generate a short-lived profile token server-side and pass it to your frontend. Then connect:

```js
const ws = new WebSocket(`wss://chat.mysite.com/ws?token=${profileToken}`);

ws.onmessage = (e) => {
  const { content } = JSON.parse(e.data);
  chatBox.innerHTML += content; // content is HTML
};

ws.send(JSON.stringify({ content: userMessage }));
```

For file uploads, read the file as base64 and include it in the `files` array:

```js
const reader = new FileReader();
reader.onload = (e) => {
  const base64 = e.target.result.split(',')[1];
  ws.send(JSON.stringify({
    content: 'Summarise this document.',
    files: [{ name: file.name, mime_type: file.type, data: base64 }],
  }));
};
reader.readAsDataURL(file);
```

## Profile token lifecycle

Tokens are validated on every message via the WhoAmI server (with a configurable TTL cache). To end a user's session, invalidate the token on your auth server — OpenTalon will reject subsequent messages from that connection automatically.

See [Profiles & Multi-Tenancy](profiles.md) for full WhoAmI server documentation.

## Standalone usage (without OpenTalon managing the process)

You can also run the binary directly and point OpenTalon at it via gRPC:

```bash
./websocket-channel -addr 0.0.0.0:9000 -path /ws
```

```yaml
channels:
  - name: websocket
    plugin: grpc://localhost:9000
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `-addr` | `0.0.0.0:9000` | Listen address |
| `-path` | `/ws` | WebSocket path |
| `-origins` | *(empty — allow all)* | Comma-separated allowed CORS origins |
