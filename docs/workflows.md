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

With a workflow plugin installed, users can create and manage multi-step automated workflows — for example, fetching Jira issues every morning and posting a summary to Slack — directly through chat or a REST API.

## Plugin REST API (reverse proxy)

Plugins can optionally expose their own HTTP API through OpenTalon's existing webhook server. Set `OPENTALON_HTTP_PORT` in the plugin's environment and OpenTalon will automatically reverse-proxy `/{plugin-name}/*` to the plugin's HTTP server — no extra port or load-balancer config needed.

For example, with `opentalon-workflows` running on port 9091:

```yaml
plugins:
  opentalon-workflows:
    plugin: ./opentalon-workflows
    enabled: true
    env:
      - OPENTALON_HTTP_PORT=9091
    config:
      ...
```

The workflow REST API will then be available at `http://<your-host>:3978/opentalon-workflows/v1/workflows`.
