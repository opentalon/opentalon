# Cluster Mode (Multi-Pod Deduplication)

When running multiple OpenTalon pods (e.g. in Kubernetes with `replicas: 2+`), each pod connects independently to the channel plugin (e.g. Slack Socket Mode). Because Slack delivers every event to all open WebSocket connections from the same app, each pod receives every inbound message — which would result in duplicate responses.

Setting `cluster.enabled: true` activates Redis-backed message deduplication. When a message arrives, each pod races to acquire a Redis lock (`SET NX EX`) keyed to the message's channel, conversation, and timestamp. Only the pod that wins the lock processes the message; the others silently skip it. If Redis is unreachable the lock attempt is logged as a warning and the message is processed anyway (fail-open), so a Redis outage never silences the bot.

## Deployment scenarios

Pick the scenario that matches your scale and availability needs. All four are first-class — OpenTalon ships with sensible defaults for the single-pod case, and the multi-pod modes are config-only changes.

| Scenario | Pods | State store | Redis | When to choose |
|---|---|---|---|---|
| **A. Single instance** | 1 | SQLite (file) | none | Dev, single-team deploys, single-region low-traffic |
| **B. Multi-pod, shared state** | 2+ | Postgres | Standalone (dedup only) | Production HA, rolling upgrades, basic horizontal scaling |
| **C. Multi-pod, HA Redis** | 2+ | Postgres | Sentinel | Regulated / SLA-bound environments where dedup must survive a Redis node loss |
| **D. Autoscaled** | 2+ (HPA) | Postgres | Standalone or Sentinel | Bursty workloads (campaign-driven spikes, batch ingestion); pods are stateless, all state in shared stores |

### A. Single instance (default)

No Redis, no Postgres — SQLite under `state.data_dir`. Fastest to stand up; not safe to run more than one pod against the same data directory.

```yaml
cluster:
  enabled: false   # default
state:
  data_dir: ~/.opentalon
```

### B. Multi-pod, shared state

Run 2+ pods behind the same channel registrations. Postgres provides shared sessions/memory; Redis provides per-message dedup so only one pod responds.

```yaml
redis:
  redis_url: "${REDIS_URL}"
cluster:
  enabled: true
  dedup_ttl: "5m"
state:
  db:
    driver: postgres
    dsn: "${DATABASE_URL}"
```

This is the recommended baseline for production. Rolling upgrades work cleanly: a new pod can take over conversations a draining pod started, because the conversation lives in Postgres, not on disk.

### C. Multi-pod, HA Redis (Sentinel)

For environments where a single Redis instance is unacceptable risk, point cluster mode at a Sentinel cluster. The dedup lock remains correct across Redis failovers.

```yaml
redis:
  master_name: "mymaster"
  sentinels:
    - "sentinel1:26379"
    - "sentinel2:26379"
    - "sentinel3:26379"
  password: "${REDIS_PASSWORD}"
cluster:
  enabled: true
  dedup_ttl: "5m"
state:
  db:
    driver: postgres
    dsn: "${DATABASE_URL}"
```

Combine with Postgres read replicas / managed HA Postgres for end-to-end HA.

### D. Autoscaled (HPA)

Same config as scenario B or C; the difference is at the Kubernetes layer. Because all session/memory state lives in Postgres and dedup is centralized in Redis, pods are effectively stateless and can be added or removed freely under load.

```yaml
# k8s HPA snippet
spec:
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
```

Tune `dedup_ttl` to be comfortably longer than your slowest expected message round-trip — if a pod is killed mid-processing, the lock must outlast its replacement picking up the work, or the message would be re-processed.

## Configuration

Redis connection details live in the top-level `redis:` block (shared with other Redis-backed subsystems such as `plugin_exec`):

```yaml
redis:
  # --- Standalone Redis ---
  redis_url: "redis://:yourpassword@redis-host:6379/0"

  # --- Redis Sentinel (comment out redis_url if using this) ---
  # master_name: "mymaster"
  # sentinels:
  #   - "sentinel1:26379"
  #   - "sentinel2:26379"
  #   - "sentinel3:26379"
  # password: "redis-master-password"       # optional
  # sentinel_password: "sentinel-password"  # optional Sentinel ACL password

cluster:
  enabled: true

  # How long to hold the dedup lock. Must be longer than the slowest expected
  # message round-trip. Default: 5m.
  dedup_ttl: "5m"
```

All string values support `${ENV_VAR}` substitution:

```yaml
redis:
  redis_url: "${REDIS_URL}"
cluster:
  enabled: true
```

## Modes

| Mode | When to use | Required fields |
|---|---|---|
| Standalone | Single Redis instance | `redis_url` |
| Sentinel | High-availability setup with Sentinel failover | `master_name` + `sentinels` |

## How the dedup key is built

```
dedup:{channelID}:{conversationID}:{message_timestamp_nanoseconds}
```

For Slack, `conversationID` is the Slack channel ID (e.g. `C0ABC1234`) and `message_timestamp` is the Slack event `ts`, which is unique per message per channel. The lock expires after `dedup_ttl` so Redis memory stays bounded.

## Edge cases

| Situation | Behaviour |
|---|---|
| Message has no timestamp (zero value) | Dedup is skipped for that message; a warning is logged. The message is always processed. |
| Redis unreachable | Lock attempt fails; a warning is logged. The message is processed anyway (fail-open). |
| `cluster.enabled: false` (default) | No Redis connection is made; all messages are processed normally. |

## PostgreSQL state store

By default OpenTalon stores sessions and memory in SQLite (one file per pod). In a multi-pod deployment all pods must share the same state so that any pod can continue a conversation started by another. Switch to PostgreSQL by setting `state.db`:

```yaml
state:
  data_dir: ""          # unused when driver is postgres
  db:
    driver: postgres
    dsn: "${DATABASE_URL}"   # e.g. postgres://user:pass@pg-host:5432/opentalon?sslmode=require
```

The `dsn` field supports `${ENV_VAR}` substitution. A typical DSN looks like:

```
postgres://opentalon:secret@postgres:5432/opentalon?sslmode=require
```

| DSN option | Meaning |
|---|---|
| `sslmode=require` | Enforce TLS (recommended in production) |
| `sslmode=disable` | No TLS (local development only) |
| `connect_timeout=5` | Seconds before connection attempt times out |

> **Note:** SQLite is the default (`driver: sqlite`) and writes to `state.data_dir`. Set `driver: postgres` only when you need multi-pod shared state.

## Example: Kubernetes deployment

```yaml
# deployment.yaml
spec:
  replicas: 3
  containers:
    - name: opentalon
      env:
        - name: REDIS_URL
          valueFrom:
            secretKeyRef:
              name: opentalon-secrets
              key: redis-url
```

```yaml
# config.yaml
redis:
  redis_url: "${REDIS_URL}"
cluster:
  enabled: true
  dedup_ttl: "5m"
```
