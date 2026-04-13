# Workflows

OpenTalon supports external workflow plugins that can build automation on top of installed plugins, channels, and the scheduler.

## Enabling workflow plugin support

Add the following to your config:

```yaml
plugin_exec:
  enabled: true

cluster:
  redis_url: "redis://localhost:6379"   # also used for cluster deduplication
```

This lets trusted plugins execute actions through OpenTalon's ToolRegistry via an internal Redis stream. Redis is required.

> **Security:** the Redis instance used here is a trust boundary. Any process that can `XADD` to `opentalon:plugin-exec` can execute plugin actions impersonating any user or group. Restrict network access to Redis so that only OpenTalon and its plugins can reach it, and treat Redis credentials as high-privilege secrets.

With a workflow plugin installed, users can create and manage multi-step automated workflows — for example, fetching Jira issues every morning and posting a summary to Slack — directly through chat or a REST API.

## Plugin REST API (reverse proxy)

Plugins can optionally expose their own HTTP API through OpenTalon's existing webhook server. Set `OPENTALON_HTTP_PORT` in the plugin's environment **and** add `expose_http: true` to the plugin's config. OpenTalon will then reverse-proxy `/{plugin-name}/*` to the plugin's HTTP server — no extra port or load-balancer config needed.

The `expose_http: true` opt-in is required because the webhook server is typically internet-facing. Without explicit operator approval, a misconfigured plugin cannot accidentally publish its API.

For example, with `opentalon-workflows` running on port 9091:

```yaml
plugins:
  opentalon-workflows:
    plugin: ./opentalon-workflows
    enabled: true
    expose_http: true          # opt-in: expose REST API via reverse proxy
    env:
      - OPENTALON_HTTP_PORT=9091
    config:
      ...
```

The workflow REST API will then be available at `http://<your-host>:3978/opentalon-workflows/v1/workflows`.

Port `3978` is OpenTalon's default webhook port (configurable via `webhook.port` in your config).
