# Deploying OpenTalon to Kubernetes

This guide covers deploying OpenTalon to any Kubernetes cluster (k3s, EKS, GKE, AKS, OVH, etc.) using either the Kubernetes Operator (recommended) or raw manifests.

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Method 1: Operator — Recommended](#method-1-operator--recommended)
  - [Step 1: Install the Operator](#step-1-install-the-operator)
  - [Step 2: Create Secrets](#step-2-create-secrets)
  - [Step 3: Create the ConfigMap](#step-3-create-the-configmap)
  - [Step 4: Deploy the OpenTalonInstance](#step-4-deploy-the-opentaloninstance)
  - [Step 5: Verify](#step-5-verify)
- [Method 2: Raw Manifests](#method-2-raw-manifests)
- [Configuration](#configuration)
  - [Adding LLM Providers](#adding-llm-providers)
  - [Channels](#channels)
  - [Plugins and Request Packages](#plugins-and-request-packages)
  - [Orchestrator Rules](#orchestrator-rules)
- [Updating Configuration](#updating-configuration)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Uninstall](#uninstall)

---

## Prerequisites

- **Kubernetes 1.28+** cluster (any distribution: k3s, EKS, GKE, AKS, etc.)
- **kubectl** configured and pointing at your cluster
- **Helm 3** (for deploying the instance chart; operator itself uses kubectl)
- **At least one LLM API key**:

| Provider | Env var | Sign up |
|----------|---------|---------|
| Anthropic | `ANTHROPIC_API_KEY` | console.anthropic.com |
| OpenAI | `OPENAI_API_KEY` | platform.openai.com |
| DeepSeek | `DEEPSEEK_API_KEY` | platform.deepseek.com |
| OpenRouter | `OPENROUTER_API_KEY` (uses `OPENAI_API_KEY` env var) | openrouter.ai |
| Ollama (self-hosted) | none | ollama.com |

Optional channel tokens:

| Channel | Env vars needed |
|---------|----------------|
| Telegram | `TELEGRAM_BOT_TOKEN` |
| Slack | `SLACK_BOT_TOKEN` (xoxb-...), `SLACK_APP_TOKEN` (xapp-...) |

---

## Method 1: Operator — Recommended

The OpenTalon Kubernetes Operator manages the full lifecycle: StatefulSet, Service, ConfigMap mount, PVC, RBAC, and auto-rollout on config changes.

> **Important:** The operator install creates the `opentalon-operator-system` namespace. All resources (secrets, ConfigMap, the OpenTalonInstance CRD) go into this same namespace. Do not create a separate namespace — it adds complexity with no benefit.

### Step 1: Install the Operator

The operator is installed via `kubectl apply` from the release manifests (not Helm):

```bash
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/latest/download/opentalon-operator.crds.yaml
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/latest/download/opentalon-operator.install.yaml
```

This creates the `opentalon-operator-system` namespace and deploys the operator controller.

Verify:

```bash
kubectl get pods -n opentalon-operator-system
```

You should see the controller-manager pod running (`1/1 Running`).

To install a specific version instead of `latest`:

```bash
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/download/v0.2.5/opentalon-operator.crds.yaml
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/download/v0.2.5/opentalon-operator.install.yaml
```

### Step 2: Create Secrets

Store your API keys as a Kubernetes Secret in the operator namespace:

```bash
kubectl create secret generic opentalon-secrets \
  --namespace opentalon-operator-system \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=OPENAI_API_KEY=sk-or-... \
  --from-literal=TELEGRAM_BOT_TOKEN=123456:ABC-DEF... \
  --from-literal=BRAVE_API_KEY=BSA...
```

Only include the keys you actually need. At minimum you need one LLM provider key and one channel token.

### Step 3: Create the ConfigMap

The ConfigMap contains two files:
1. `config.yaml` — the full OpenTalon configuration
2. `channel.yaml` — the channel spec (e.g., Telegram), because the Docker image only ships the compiled binary, not the source channel definitions

> **Key detail:** The operator mounts the ConfigMap at `/etc/opentalon/` (not `/config/`). The channel.yaml path in your config must reference `/etc/opentalon/channel.yaml`. Data is mounted at `/data/` (not `/home/opentalon/.opentalon`).

First, create your `config.k8s.yaml` locally:

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      models:
        - id: claude-haiku-4-5-20251001
          name: Claude Haiku 4.5
          input: [text]
          context_window: 200000
          max_tokens: 8192
          cost:
            input: 0.80
            output: 4.00

  catalog:
    anthropic/claude-haiku-4-5-20251001:
      alias: haiku
      weight: 100

routing:
  primary: anthropic/claude-haiku-4-5-20251001

orchestrator:
  rules: []
  content_preparers: []
  pipeline:
    enabled: true
    max_step_retries: 2
    step_timeout: "30s"

channels:
  telegram:
    enabled: true
    plugin: "/etc/opentalon/channel.yaml"
    config: {}

state:
  data_dir: /data/opentalon

log:
  file: /data/opentalon/opentalon.log
```

Then create the ConfigMap from your config + the channel spec:

```bash
kubectl create configmap opentalon-config \
  --from-file=config.yaml=config.k8s.yaml \
  --from-file=channel.yaml=channels/telegram-channel/channel.yaml \
  -n opentalon-operator-system \
  --dry-run=client -o yaml | kubectl apply -f -
```

> **Why `--dry-run=client -o yaml | kubectl apply`?** This pattern lets you re-run the same command to update the ConfigMap without needing to delete it first.

### Step 4: Deploy the OpenTalonInstance

You can deploy the CRD directly with `kubectl apply` or wrap it in a Helm chart for CI/CD integration.

#### Option A: Direct kubectl apply

Create `opentalon-instance.yaml`:

```yaml
apiVersion: opentalon.io/v1alpha1
kind: OpenTalonInstance
metadata:
  name: opentalon
  namespace: opentalon-operator-system
spec:
  image:
    repository: ghcr.io/opentalon/opentalon
    tag: "latest"
    pullPolicy: Always
  configFrom:
    name: opentalon-config
  envFrom:
    - secretRef:
        name: opentalon-secrets
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 256Mi
  storage:
    persistence:
      enabled: true
      size: 1Gi
  observability:
    metrics:
      enabled: false
  networking:
    networkPolicy:
      enabled: false
```

Apply:

```bash
kubectl apply -f opentalon-instance.yaml
```

#### Option B: Helm chart (for CI/CD)

If you manage deployments via Helm and want change detection in CI/CD, create a Helm chart that templates the OpenTalonInstance CRD. See the [example chart structure](#helm-chart-for-cicd) below.

```bash
helm upgrade --install opentalon ./helm/charts/opentalon/ \
  --namespace opentalon-operator-system \
  --wait --timeout 5m
```

> **Do not include a Namespace or ConfigMap template in the Helm chart.** The namespace is created by the operator install. The ConfigMap is created manually because it includes binary channel files (`channel.yaml`) that come from local source, not values.yaml.

### Step 5: Verify

```bash
kubectl get opentaloninstances -n opentalon-operator-system
kubectl get pods -n opentalon-operator-system
kubectl logs -l app.kubernetes.io/name=opentalon -n opentalon-operator-system
```

You should see:

```
OpenTalon starting...
yaml-channel: init delete_webhook done
yaml-channel: init get_me done
yaml-channel: telegram started
channel-manager: loaded telegram via yaml
Channels loaded.
```

The operator creates a **StatefulSet**, so the pod is named `opentalon-0` (stable name, PVC stays bound across restarts).

### Helm Chart for CI/CD

Example chart structure for automated deployments:

```
helm/charts/opentalon/
├── Chart.yaml
├── values.yaml
├── templates/
│   └── opentaloninstance.yaml
└── README.md
```

**Chart.yaml:**
```yaml
apiVersion: v2
name: opentalon
description: OpenTalon LLM orchestration platform instance
type: application
version: 0.1.0
appVersion: "latest"
```

**values.yaml:**
```yaml
namespace: opentalon-operator-system
image:
  repository: ghcr.io/opentalon/opentalon
  tag: latest
  pullPolicy: Always
resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 256Mi
storage:
  size: 1Gi
secrets:
  name: opentalon-secrets
```

**templates/opentaloninstance.yaml:**
```yaml
apiVersion: opentalon.io/v1alpha1
kind: OpenTalonInstance
metadata:
  name: opentalon
  namespace: {{ .Values.namespace }}
spec:
  image:
    repository: {{ .Values.image.repository }}
    tag: {{ .Values.image.tag | quote }}
    pullPolicy: {{ .Values.image.pullPolicy }}
  configFrom:
    name: opentalon-config
  env:
    - name: LOG_LEVEL
      value: debug
  envFrom:
    - secretRef:
        name: {{ .Values.secrets.name }}
  resources:
    requests:
      cpu: {{ .Values.resources.requests.cpu }}
      memory: {{ .Values.resources.requests.memory }}
    limits:
      cpu: {{ .Values.resources.limits.cpu }}
      memory: {{ .Values.resources.limits.memory }}
  storage:
    persistence:
      enabled: true
      size: {{ .Values.storage.size }}
  observability:
    metrics:
      enabled: false
  networking:
    networkPolicy:
      enabled: false
```

---

## Method 2: Raw Manifests

For users who cannot use the operator or prefer full control over each resource. This creates a plain Deployment (not a StatefulSet).

### Step 1: Create Namespace and Secret

```bash
kubectl create namespace opentalon

kubectl create secret generic opentalon-secrets \
  --namespace opentalon \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-... \
  --from-literal=TELEGRAM_BOT_TOKEN=123456:ABC-DEF...
```

### Step 2: Create the ConfigMap

With raw manifests, you control mount paths. Config is mounted at `/config/`, data goes to `/home/opentalon/.opentalon`:

```bash
kubectl create configmap opentalon-config \
  --from-file=config.yaml=config.yaml \
  --from-file=channel.yaml=channels/telegram-channel/channel.yaml \
  -n opentalon \
  --dry-run=client -o yaml | kubectl apply -f -
```

In your `config.yaml`, set paths for the raw manifest layout:

```yaml
channels:
  telegram:
    enabled: true
    plugin: "/config/channel.yaml"    # ConfigMap mounted at /config/
    config: {}

state:
  data_dir: /home/opentalon/.opentalon  # PVC mount path

log:
  file: /home/opentalon/.opentalon/opentalon.log
```

### Step 3: Apply the Deployment Manifests

Create `opentalon.yaml`:

```yaml
---
# ServiceAccount — no token automount, zero RBAC bindings = no API access
apiVersion: v1
kind: ServiceAccount
metadata:
  name: opentalon
  namespace: opentalon
automountServiceAccountToken: false

---
# PersistentVolumeClaim
# Omit storageClassName to use the cluster default (local-path on k3s).
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: opentalon-data
  namespace: opentalon
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi

---
# Deployment
apiVersion: apps/v1
kind: Deployment
metadata:
  name: opentalon
  namespace: opentalon
  labels:
    app: opentalon
spec:
  replicas: 1
  selector:
    matchLabels:
      app: opentalon
  template:
    metadata:
      labels:
        app: opentalon
    spec:
      serviceAccountName: opentalon
      automountServiceAccountToken: false

      # The Dockerfile creates Alpine system user "opentalon" via adduser -S.
      # Alpine system users start at UID 100, GID 101.
      securityContext:
        runAsNonRoot: true
        runAsUser: 100
        runAsGroup: 101
        fsGroup: 101
        seccompProfile:
          type: RuntimeDefault

      containers:
        - name: opentalon
          image: ghcr.io/opentalon/opentalon:latest
          imagePullPolicy: Always
          args:
            - "-config"
            - "/config/config.yaml"
          ports:
            - name: http
              containerPort: 3978
              protocol: TCP
          envFrom:
            - secretRef:
                name: opentalon-secrets
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
          volumeMounts:
            # ConfigMap -> /config/ (read-only, contains config.yaml + channel.yaml)
            - name: config
              mountPath: /config
              readOnly: true
            # PVC -> persistent state, session history, plugins, logs
            - name: data
              mountPath: /home/opentalon/.opentalon
            # Writable /tmp — required for Go build cache (plugin compilation)
            - name: tmp
              mountPath: /tmp
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi

      volumes:
        - name: config
          configMap:
            name: opentalon-config
        - name: data
          persistentVolumeClaim:
            claimName: opentalon-data
        - name: tmp
          emptyDir: {}

---
# Service — ClusterIP only
apiVersion: v1
kind: Service
metadata:
  name: opentalon
  namespace: opentalon
  labels:
    app: opentalon
spec:
  type: ClusterIP
  selector:
    app: opentalon
  ports:
    - name: http
      port: 3978
      targetPort: http
      protocol: TCP
```

Apply:

```bash
kubectl apply -f opentalon.yaml
```

Verify:

```bash
kubectl get pods -n opentalon
kubectl logs -n opentalon -l app=opentalon -f
```

**Notes:**

- **Image:** `ghcr.io/opentalon/opentalon:latest` is public. No `imagePullSecrets` required.
- **No NetworkPolicy included.** On k3s with Flannel, NetworkPolicy resources are accepted but not enforced.
- **The `/tmp` emptyDir mount is required.** The runtime image includes the Go toolchain for compiling plugins. Go needs writable `GOCACHE=/tmp/go-build` and `GOPATH=/tmp/go`.
- **No readiness probe.** The pod shows `1/1` immediately. Add a probe if you need one.

### Operator vs Raw Manifests

| | Operator | Raw Manifests |
|---|---|---|
| **Install method** | `kubectl apply` (CRDs + install manifest) | `kubectl apply -f opentalon.yaml` |
| **Creates** | StatefulSet (pod: `opentalon-0`) | Deployment (pod: `opentalon-<hash>`) |
| **Config mount** | `/etc/opentalon/` | `/config/` (you control it) |
| **Data mount** | `/data/` | `/home/opentalon/.opentalon` (you control it) |
| **Config reload** | Auto-rollout on ConfigMap change | Manual: `kubectl rollout restart` |
| **Manages** | PVC, Service, RBAC, NetworkPolicy, etc. | You manage everything |
| **Namespace** | `opentalon-operator-system` (shared with operator) | Any namespace you choose |

---

## Configuration

### Adding LLM Providers

Each provider is configured under `models.providers` in `config.yaml`. API keys use `${ENV_VAR}` syntax, resolved from environment variables injected via Kubernetes Secrets.

#### Anthropic

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      models:
        - id: claude-haiku-4-5-20251001
          name: Claude Haiku 4.5
          input: [text]
          context_window: 200000
          max_tokens: 8192
          cost:
            input: 0.80
            output: 4.00
```

#### OpenAI

```yaml
    openai:
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions
      models:
        - id: gpt-4o
          name: GPT-4o
          input: [text]
          context_window: 128000
          max_tokens: 4096
          cost:
            input: 2.50
            output: 10.00
```

#### DeepSeek (OpenAI-compatible)

```yaml
    deepseek:
      base_url: "https://api.deepseek.com/v1"
      api_key: "${DEEPSEEK_API_KEY}"
      api: openai-completions
      models:
        - id: deepseek-chat
          name: DeepSeek Chat
          input: [text]
          context_window: 128000
          cost:
            input: 0.14
            output: 0.28
```

#### OpenRouter (OpenAI-compatible)

```yaml
    openrouter:
      base_url: "https://openrouter.ai/api/v1"
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions
      models:
        - id: mistralai/ministral-8b-2512
          name: Ministral 8B
          input: [text]
          context_window: 262144
          max_tokens: 4096
          cost:
            input: 0.15
            output: 0.15
```

#### Ollama (Self-Hosted)

Point `base_url` at the Ollama service in your cluster. No API key needed.

```yaml
    ollama:
      base_url: "http://ollama.default.svc:11434/v1"
      api_key: ""
      api: openai-completions
      models:
        - id: llama3.1
          name: Llama 3.1 8B
          input: [text]
          context_window: 128000
```

#### Model Catalog and Routing

After defining providers, set up the catalog (aliases, weights) and routing (primary, fallback chain):

```yaml
  catalog:
    anthropic/claude-haiku-4-5-20251001:
      alias: haiku
      weight: 100
    deepseek/deepseek-chat:
      alias: deepseek
      weight: 80

routing:
  primary: anthropic/claude-haiku-4-5-20251001
  fallbacks:
    - deepseek/deepseek-chat
```

### Channels

Channels define how users interact with OpenTalon. They can be compiled Go plugins or **YAML-driven** (no binary needed — the spec runs in-process).

#### Telegram (YAML-Driven)

The Telegram channel uses a `channel.yaml` spec file. The Docker image only ships the compiled OpenTalon binary — it does not include channel spec files. You must provide `channel.yaml` via the ConfigMap.

**Operator method** — channel.yaml is included in the ConfigMap and mounted at `/etc/opentalon/`:

```yaml
channels:
  telegram:
    enabled: true
    plugin: "/etc/opentalon/channel.yaml"
    config: {}
```

**Raw manifest method** — ConfigMap mounted at `/config/`:

```yaml
channels:
  telegram:
    enabled: true
    plugin: "/config/channel.yaml"
    config: {}
```

In both cases, create the ConfigMap with both files:

```bash
kubectl create configmap opentalon-config \
  --from-file=config.yaml=config.k8s.yaml \
  --from-file=channel.yaml=channels/telegram-channel/channel.yaml \
  -n <namespace> \
  --dry-run=client -o yaml | kubectl apply -f -
```

Requires `TELEGRAM_BOT_TOKEN` in your Secret.

<details>
<summary>Full Telegram channel.yaml reference</summary>

```yaml
kind: channel
version: 1
id: telegram
name: Telegram

capabilities:
  threads: false
  files: false
  reactions: false
  edits: false
  max_message_length: 4096

required_env: [TELEGRAM_BOT_TOKEN]

init:
  - name: delete_webhook
    method: POST
    url: "https://api.telegram.org/bot{{env.TELEGRAM_BOT_TOKEN}}/deleteWebhook"
    headers:
      Content-Type: application/json
  - name: get_me
    method: GET
    url: "https://api.telegram.org/bot{{env.TELEGRAM_BOT_TOKEN}}/getMe"
    store:
      bot_id: "result.id"
      bot_username: "result.username"

inbound:
  polling:
    method: GET
    url: "https://api.telegram.org/bot{{env.TELEGRAM_BOT_TOKEN}}/getUpdates?timeout=30&allowed_updates=[\"message\"]&offset={{self.poll_offset}}"
    interval: 1s
    result_path: result
    cursor_field: update_id
  event_path: "message"
  process_when:
    - field: "chat.type"
      equals: "private"
    - field: "text"
      contains: "@{{self.bot_username}}"
    - field: "reply_to_message.from.id"
      equals: "{{self.bot_id}}"
  skip:
    - field: "from.id"
      equals: "{{self.bot_id}}"
  mapping:
    conversation_id: "chat.id"
    sender_id: "from.id"
    content: "text"
    thread_id: ""
    metadata:
      message_id: "message_id"
  transforms:
    - type: replace
      pattern: "@{{self.bot_username}}"
      replacement: ""
    - type: trim
  dedup:
    key: "{{event.chat.id}}:{{event.message_id}}"
    ttl: 10m

hooks:
  on_receive:
    - method: POST
      url: "https://api.telegram.org/bot{{env.TELEGRAM_BOT_TOKEN}}/sendChatAction"
      headers:
        Content-Type: application/json
      body: '{"chat_id":"{{event.conversation_id}}","action":"typing"}'

outbound:
  chunking:
    max_length: 4096
  send:
    method: POST
    url: "https://api.telegram.org/bot{{env.TELEGRAM_BOT_TOKEN}}/sendMessage"
    headers:
      Content-Type: application/json
    body: '{"chat_id":"{{msg.conversation_id}}","text":"{{msg.content}}"}'
```

</details>

#### Slack (YAML-Driven)

Slack follows the same YAML-driven pattern. OpenTalon auto-fetches the channel definition from GitHub when the `github` field is set:

```yaml
channels:
  slack:
    enabled: true
    plugin: "./channels/slack/channel.yaml"
    github: "opentalon/slack-channel"
    ref: "master"
    config:
      ack_reaction: eyes
      done_reaction: white_check_mark
```

Requires `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` in your Secret. Unlike Telegram, the Slack channel has a GitHub repo (`opentalon/slack-channel`) so it can be auto-fetched — no need to include it in the ConfigMap.

### Plugins and Request Packages

#### Compiled Plugins (Go)

Plugins are Go binaries auto-fetched from GitHub and compiled at runtime inside the container (this is why the image ships with the Go toolchain):

```yaml
plugins:
  opentalon-commands:
    enabled: true
    insecure: false
    github: "opentalon/opentalon-commands"
    ref: "master"
    config: {}
```

#### Request Packages (No-Code HTTP Skills)

Request packages are HTTP-based tool definitions — no compiled plugin needed:

From GitHub:

```yaml
request_packages:
  skills:
    - name: brave-search
      github: opentalon/brave-plugin
      ref: master
```

Inline:

```yaml
request_packages:
  inline:
    - plugin: jira
      description: Create and manage Jira issues
      packages:
        - action: create_issue
          description: Create a Jira issue
          method: POST
          url: "{{env.JIRA_URL}}/rest/api/3/issue"
          body: '{"fields":{"project":{"key":"{{args.project}}"},"summary":"{{args.summary}}","issuetype":{"name":"Task"}}}'
          headers:
            Authorization: "Bearer {{env.JIRA_API_TOKEN}}"
          required_env: [JIRA_URL, JIRA_API_TOKEN]
          parameters:
            - name: project
              description: Project key
              required: true
            - name: summary
              description: Issue summary
              required: true
```

### Orchestrator Rules

Rules define the system prompt and persona:

```yaml
orchestrator:
  rules:
    - "You are a helpful technical assistant."
    - "Always respond in the same language the user writes in."
    - "Keep responses concise."
  content_preparers: []
  pipeline:
    enabled: true
    max_step_retries: 2
    step_timeout: "30s"
```

---

## Updating Configuration

### Operator Method

Re-run the ConfigMap creation command (the `--dry-run=client | kubectl apply` pattern handles updates):

```bash
kubectl create configmap opentalon-config \
  --from-file=config.yaml=config.k8s.yaml \
  --from-file=channel.yaml=channels/telegram-channel/channel.yaml \
  -n opentalon-operator-system \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl rollout restart statefulset/opentalon -n opentalon-operator-system
```

For CRD changes (image, resources, storage), edit the OpenTalonInstance:

```bash
kubectl edit opentaloninstance opentalon -n opentalon-operator-system
# or
kubectl apply -f opentalon-instance.yaml
# or
helm upgrade opentalon ./helm/charts/opentalon/ -n opentalon-operator-system
```

### Raw Manifests Method

```bash
kubectl create configmap opentalon-config \
  --from-file=config.yaml=config.yaml \
  --from-file=channel.yaml=channels/telegram-channel/channel.yaml \
  -n opentalon \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl rollout restart deployment/opentalon -n opentalon
```

### Updating the Operator

To upgrade the operator to a new version:

```bash
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/download/vX.Y.Z/opentalon-operator.crds.yaml
kubectl apply -f https://github.com/opentalon/k8s-operator/releases/download/vX.Y.Z/opentalon-operator.install.yaml
```

Then redeploy the instance to pick up any behavior changes:

```bash
helm uninstall opentalon -n opentalon-operator-system
helm install opentalon ./helm/charts/opentalon/ -n opentalon-operator-system --wait --timeout 5m
```

---

## Troubleshooting

### CrashLoopBackOff

```bash
kubectl logs <pod-name> -n <namespace> --previous
```

Common causes:

- **Missing API keys:** Secret not created or not referenced in `envFrom`. Check: `kubectl get secret opentalon-secrets -n <namespace> -o yaml`
- **Invalid config.yaml:** Syntax errors. Logs show the exact parse error.
- **Permission errors (UID mismatch):** PVC was created with a different `fsGroup`. The Dockerfile creates Alpine user at UID 100 / GID 101. Ensure: `runAsUser: 100`, `runAsGroup: 101`, `fsGroup: 101`.

### Channel "read channel spec ... no such file or directory"

```
failed to load channels: telegram: channel "telegram": read channel spec /config/channel.yaml: open /config/channel.yaml: no such file or directory
```

The Docker image only ships the compiled binary, not source files like `channel.yaml`. You must include the channel spec in your ConfigMap.

**Common mistakes:**

1. **Forgot to include `channel.yaml` in the ConfigMap.** The `--from-file` command must include both config.yaml and channel.yaml.
2. **Wrong path in config.yaml.** The operator mounts at `/etc/opentalon/`, raw manifests at `/config/`. Match the `plugin:` path to your mount point.
3. **ConfigMap key mismatch.** If you used `--from-file=channel.yaml=...`, the file is available as `channel.yaml` in the mount directory.

### "state store: mkdir /.opentalon: read-only file system"

The `data_dir` in config.yaml resolves to a read-only path. This happens when `data_dir: ~/.opentalon` resolves to `/.opentalon` (the container user's home may not be `/home/opentalon` in all contexts).

**Fix:** Use absolute paths:
- Operator: `data_dir: /data/opentalon` (PVC is at `/data/`)
- Raw manifests: `data_dir: /home/opentalon/.opentalon` (PVC is at `/home/opentalon/.opentalon`)

### Pending Pods

```bash
kubectl describe pod <pod-name> -n <namespace>
```

- **PVC not bound:** Check StorageClass exists: `kubectl get storageclass`. On k3s, `local-path` should be available by default.
- **Insufficient resources:** Node lacks CPU/memory. Check: `kubectl describe node`.

### Plugin Download / Build Failures

Plugins are fetched from GitHub and compiled at runtime.

- **Egress blocked:** Pod needs outbound HTTPS to `github.com:443`.
- **Go build cache errors:** The image sets `GOCACHE=/tmp/go-build` and `GOPATH=/tmp/go`. If `/tmp` is not writable, compilation fails.
- **/tmp not writable:** You **must** mount `/tmp` as an emptyDir. With `readOnlyRootFilesystem: true`, the container filesystem is read-only — `/tmp` needs an explicit mount.

### Helm "cannot be imported into the current release"

```
ConfigMap "opentalon-config" exists and cannot be imported into the current release: invalid ownership metadata
```

This happens when a resource was created manually (via `kubectl`) and then Helm tries to manage it. Helm requires ownership annotations.

**Fix:** Don't include manually-managed resources (ConfigMap, Namespace) in the Helm chart. The chart should only contain the OpenTalonInstance CRD template. Create ConfigMap and Namespace outside of Helm.

### Helm "Apply failed with 1 conflict"

```
conflict with "manager" using opentalon.io/v1alpha1: .spec.observability.metrics.enabled
```

The operator's controller-manager owns the field and Helm can't override it.

**Fix:** `helm uninstall` then `helm install` (fresh install instead of upgrade).

### Logs Location

Logs are written to the data directory, not stdout:

```bash
# Operator (data at /data/)
kubectl exec opentalon-0 -n opentalon-operator-system -- ls /data/opentalon/logs/
kubectl exec opentalon-0 -n opentalon-operator-system -- cat /data/opentalon/opentalon.log

# Raw manifests (data at /home/opentalon/.opentalon)
kubectl exec <pod> -n opentalon -- ls /home/opentalon/.opentalon/logs/
```

Enable debug logging by adding `LOG_LEVEL=debug` as an env var:

```yaml
# In CRD spec
spec:
  env:
    - name: LOG_LEVEL
      value: debug
```

### Entering the Container

```bash
kubectl exec -it opentalon-0 -n opentalon-operator-system -- /bin/sh
```

The `-it` flags are required for an interactive shell.

---

## FAQ

**Q: Should I create a separate namespace for the OpenTalon instance?**
A: No. Deploy everything into `opentalon-operator-system` — the operator and instance in the same namespace. A separate namespace adds complexity with no benefit for a single-instance deployment.

**Q: Why is the pod named `opentalon-0` instead of `opentalon-abc123`?**
A: The operator creates a StatefulSet (stable pod names, stable PVC bindings). Raw manifests create a Deployment (random suffix). `opentalon-0` is correct for operator deployments.

**Q: Why does the image include the Go toolchain?**
A: OpenTalon compiles plugins from source at runtime. The runtime image is `golang:1.24-alpine`, not `scratch`. This is why `/tmp` must be writable (`GOCACHE=/tmp/go-build`, `GOPATH=/tmp/go`).

**Q: Can I use `configFrom` with the inline `config` field?**
A: No. When `configFrom` is set, the inline `config` field is ignored entirely. Use one or the other.

**Q: How do I add a new channel without redeploying?**
A: Update your `config.yaml`, re-run the ConfigMap creation command, then restart the StatefulSet (`kubectl rollout restart statefulset/opentalon`).

**Q: What UID/GID does the container run as?**
A: The Dockerfile creates an Alpine system user `opentalon` at UID 100 / GID 101. The operator's default security context uses UID 1000 / GID 1000. With raw manifests, set `runAsUser: 100`, `runAsGroup: 101`, `fsGroup: 101`.

---

## Uninstall

### Operator Method

```bash
# Delete the instance (PVC is retained)
kubectl delete opentaloninstance opentalon -n opentalon-operator-system

# Delete secrets and configmap
kubectl delete secret opentalon-secrets -n opentalon-operator-system
kubectl delete configmap opentalon-config -n opentalon-operator-system

# Uninstall the operator
kubectl delete -f https://github.com/opentalon/k8s-operator/releases/latest/download/opentalon-operator.install.yaml
kubectl delete -f https://github.com/opentalon/k8s-operator/releases/latest/download/opentalon-operator.crds.yaml

# Remove persistent data (optional)
kubectl delete pvc -n opentalon-operator-system -l app.kubernetes.io/name=opentalon
kubectl delete namespace opentalon-operator-system
```

If the instance was deployed via Helm:

```bash
helm uninstall opentalon -n opentalon-operator-system
```

### Raw Manifests Method

```bash
kubectl delete -f opentalon.yaml
kubectl delete configmap opentalon-config -n opentalon
kubectl delete secret opentalon-secrets -n opentalon
kubectl delete pvc opentalon-data -n opentalon
kubectl delete namespace opentalon
```
