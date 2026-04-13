# Cluster Mode (Multi-Pod Deduplication)

When running multiple OpenTalon pods (e.g. in Kubernetes with `replicas: 2+`), each pod connects independently to the channel plugin (e.g. Slack Socket Mode). Because Slack delivers every event to all open WebSocket connections from the same app, each pod receives every inbound message — which would result in duplicate responses.

Setting `cluster.enabled: true` activates Redis-backed message deduplication. When a message arrives, each pod races to acquire a Redis lock (`SET NX EX`) keyed to the message's channel, conversation, and timestamp. Only the pod that wins the lock processes the message; the others silently skip it. If Redis is unreachable the lock attempt is logged as a warning and the message is processed anyway (fail-open), so a Redis outage never silences the bot.

## Configuration

```yaml
cluster:
  enabled: true

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

  # How long to hold the dedup lock. Must be longer than the slowest expected
  # message round-trip. Default: 5m.
  dedup_ttl: "5m"
```

All string values support `${ENV_VAR}` substitution:

```yaml
cluster:
  enabled: true
  redis_url: "${REDIS_URL}"
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
cluster:
  enabled: true
  redis_url: "${REDIS_URL}"
  dedup_ttl: "5m"
```
