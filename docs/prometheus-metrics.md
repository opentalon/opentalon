# Prometheus Metrics

OpenTalon can expose a Prometheus-compatible `/metrics` endpoint, which lets you track token spend, LLM usage per model, and plugin call activity.

## Configuration

Add a `metrics` section to your `config.yaml`:

```yaml
metrics:
  enabled: true
  addr: ":2112"   # optional; defaults to :2112
```

When enabled, OpenTalon starts an HTTP server on the configured address. Prometheus can then scrape it at `http://<host>:2112/metrics`.

> **Security:** `/metrics` is unauthenticated. Bind to a loopback address (`127.0.0.1:2112`) or a private interface if the network is not trusted.

## Available metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `opentalon_llm_input_tokens_total` | Counter | `model`, `channel`, `group`, `entity_id` | Total input (prompt) tokens sent to the LLM |
| `opentalon_llm_output_tokens_total` | Counter | `model`, `channel`, `group`, `entity_id` | Total output (completion) tokens received from the LLM |
| `opentalon_llm_input_cost_usd_total` | Counter | `model`, `channel`, `group`, `entity_id` | Total input spend in USD |
| `opentalon_llm_output_cost_usd_total` | Counter | `model`, `channel`, `group`, `entity_id` | Total output spend in USD |
| `opentalon_orchestrator_runs_total` | Counter | `model`, `channel`, `group`, `entity_id` | Total completed orchestrator runs |
| `opentalon_plugin_calls_total` | Counter | `plugin`, `action`, `status` | Total plugin/tool calls; `status` is `success` or `error` |
| `opentalon_plugin_input_tokens_total` | Counter | `plugin`, `action` | LLM input tokens attributed to each plugin/tool call |
| `opentalon_plugin_output_tokens_total` | Counter | `plugin`, `action` | LLM output tokens attributed to each plugin/tool call |

Standard Go runtime and process metrics (`go_*`, `process_*`) are also exposed.

> **Note:** Cost metrics are only non-zero when model `cost.input` / `cost.output` pricing is configured in `models.providers.<id>.models[*].cost`. Zero-cost (free-tier) models still emit the series with a value of `0`.

### Label semantics

- `model` — the LLM model that served the run (e.g. `gpt-oss-120b`).
- `channel` — the channel plugin that initiated the run (e.g. `slack`, `msteams`, `console`, `websocket`).
- `group` — the channel-scoped group identifier (e.g. Slack team/workspace ID). Stable per tenant.
- `entity_id` — the channel-scoped actor identifier (e.g. the Slack user ID that sent the message). Use this to attribute spend to individual users. Empty for runs without a resolved actor.

> **Cardinality:** `entity_id` adds one series per unique user. For deployments with a bounded user base this is fine; for public-facing deployments with unbounded users, consider dropping the label via `metric_relabel_configs` in your Prometheus scrape config.

## Prometheus sidecar / Docker Compose example

The simplest deployment is a Prometheus sidecar that scrapes the OpenTalon metrics port.

**`docker-compose.yml`**

```yaml
version: "3.9"

services:
  opentalon:
    image: opentalon/opentalon:latest
    volumes:
      - ./config.yaml:/config/config.yaml:ro
      - opentalon-data:/home/opentalon/.opentalon
    command: ["-config", "/config/config.yaml"]
    ports:
      - "2112:2112"   # metrics port (only needed if you scrape from outside the compose network)

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "9090:9090"
    depends_on:
      - opentalon

volumes:
  opentalon-data:
```

**`prometheus.yml`**

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: opentalon
    static_configs:
      - targets: ["opentalon:2112"]
```

## Kubernetes sidecar example

When running in Kubernetes you can add Prometheus as a sidecar container or rely on a cluster-wide Prometheus Operator with a `ServiceMonitor`.

### Sidecar approach

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: opentalon
spec:
  template:
    spec:
      containers:
        - name: opentalon
          image: opentalon/opentalon:latest
          args: ["-config", "/config/config.yaml"]
          ports:
            - name: metrics
              containerPort: 2112

        - name: prometheus
          image: prom/prometheus:latest
          args:
            - "--config.file=/etc/prometheus/prometheus.yml"
          volumeMounts:
            - name: prom-config
              mountPath: /etc/prometheus
          ports:
            - name: prom-ui
              containerPort: 9090

      volumes:
        - name: prom-config
          configMap:
            name: opentalon-prom-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: opentalon-prom-config
data:
  prometheus.yml: |
    global:
      scrape_interval: 15s
    scrape_configs:
      - job_name: opentalon
        static_configs:
          - targets: ["localhost:2112"]
```

### Prometheus Operator (`ServiceMonitor`)

If you use the [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator), expose the metrics port via a `Service` and create a `ServiceMonitor`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: opentalon-metrics
  labels:
    app: opentalon
spec:
  selector:
    app: opentalon
  ports:
    - name: metrics
      port: 2112
      targetPort: metrics
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: opentalon
spec:
  selector:
    matchLabels:
      app: opentalon
  endpoints:
    - port: metrics
      interval: 30s
      path: /metrics
```

## Example PromQL queries

```promql
# Input token rate per model (summed across channels and groups)
sum by (model) (rate(opentalon_llm_input_tokens_total[5m]))

# Total USD spend per model
sum by (model) (
  opentalon_llm_input_cost_usd_total + opentalon_llm_output_cost_usd_total
)

# Plugin call error rate
rate(opentalon_plugin_calls_total{status="error"}[5m])
  / rate(opentalon_plugin_calls_total[5m])

# Most used plugins
topk(10, sum by (plugin) (opentalon_plugin_calls_total))

# Token usage per MCP server / plugin
sum by (plugin) (opentalon_plugin_input_tokens_total + opentalon_plugin_output_tokens_total)

# Top 10 plugins by token consumption
topk(10, sum by (plugin) (opentalon_plugin_input_tokens_total + opentalon_plugin_output_tokens_total))

# Orchestrator runs per channel
sum by (channel) (opentalon_orchestrator_runs_total)
```
