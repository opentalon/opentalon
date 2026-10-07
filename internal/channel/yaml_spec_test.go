package channel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"gopkg.in/yaml.v3"
)

func TestYAMLChannelSpecParse(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: test-channel
name: Test Channel

capabilities:
  threads: true
  files: false
  reactions: true
  edits: false
  max_message_length: 2000

required_env: [TEST_TOKEN]

init:
  - name: auth
    method: POST
    url: "https://example.com/auth"
    headers:
      Authorization: "Bearer {{env.TEST_TOKEN}}"
    store:
      user_id: "id"

connection:
  url: "{{self.ws_url}}"
  reconnect:
    enabled: true
    backoff_initial: 2s
    backoff_max: 60s
    re_init: [auth]

inbound:
  ack:
    when: "req_id"
    send: '{"req_id":"{{frame.req_id}}"}'
  event_path: "data.event"
  event_types: [message]
  skip:
    - field: "user"
      equals: "{{self.user_id}}"
    - field: "bot"
      not_empty: true
      except: [broadcast]
  mapping:
    conversation_id: "channel"
    sender_id: "user"
    content: "text"
    thread_id:
      field: "thread_ts"
      fallback: "ts"
    metadata:
      ts: "ts"
  transforms:
    - type: replace
      pattern: "<@{{self.user_id}}>"
      replacement: ""
    - type: trim
  dedup:
    key: "{{event.channel}}:{{event.ts}}"
    ttl: 5m

outbound:
  chunking:
    max_length: 2000
  send:
    method: POST
    url: "https://example.com/send"
    headers:
      Authorization: "Bearer {{env.TEST_TOKEN}}"
    body: '{"channel":"{{msg.conversation_id}}","text":"{{msg.content}}"}'

hooks:
  on_receive:
    - method: POST
      url: "https://example.com/react"
      body: '{"emoji":"{{config.ack_reaction}}"}'
      when: "{{config.ack_reaction}}"

tools_file: tools.yaml
`

	var spec YAMLChannelSpec
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if spec.ID != "test-channel" {
		t.Errorf("ID = %q, want %q", spec.ID, "test-channel")
	}
	if spec.Name != "Test Channel" {
		t.Errorf("Name = %q, want %q", spec.Name, "Test Channel")
	}
	if !spec.Capabilities.Threads {
		t.Error("Capabilities.Threads should be true")
	}
	if spec.Capabilities.MaxMessageLength != 2000 {
		t.Errorf("MaxMessageLength = %d, want 2000", spec.Capabilities.MaxMessageLength)
	}
	if len(spec.RequiredEnv) != 1 || spec.RequiredEnv[0] != "TEST_TOKEN" {
		t.Errorf("RequiredEnv = %v, want [TEST_TOKEN]", spec.RequiredEnv)
	}
	if len(spec.Init) != 1 {
		t.Fatalf("Init steps = %d, want 1", len(spec.Init))
	}
	if spec.Init[0].Name != "auth" {
		t.Errorf("Init[0].Name = %q, want %q", spec.Init[0].Name, "auth")
	}
	if spec.Init[0].Store["user_id"] != "id" {
		t.Errorf("Init[0].Store[user_id] = %q, want %q", spec.Init[0].Store["user_id"], "id")
	}
	if !spec.Connection.Reconnect.Enabled {
		t.Error("Reconnect.Enabled should be true")
	}
	if spec.Connection.Reconnect.BackoffInitial != 2*time.Second {
		t.Errorf("BackoffInitial = %v, want 2s", spec.Connection.Reconnect.BackoffInitial)
	}
	if spec.Inbound.EventPath != "data.event" {
		t.Errorf("EventPath = %q, want %q", spec.Inbound.EventPath, "data.event")
	}
	if len(spec.Inbound.Skip) != 2 {
		t.Fatalf("Skip rules = %d, want 2", len(spec.Inbound.Skip))
	}
	if spec.Inbound.Skip[1].NotEmpty == nil || !*spec.Inbound.Skip[1].NotEmpty {
		t.Error("Skip[1].NotEmpty should be true")
	}
	if len(spec.Inbound.Skip[1].Except) != 1 || spec.Inbound.Skip[1].Except[0] != "broadcast" {
		t.Errorf("Skip[1].Except = %v, want [broadcast]", spec.Inbound.Skip[1].Except)
	}
	if spec.Inbound.Mapping.ThreadID.Field != "thread_ts" {
		t.Errorf("Mapping.ThreadID.Field = %q, want %q", spec.Inbound.Mapping.ThreadID.Field, "thread_ts")
	}
	if spec.Inbound.Mapping.ThreadID.Fallback != "ts" {
		t.Errorf("Mapping.ThreadID.Fallback = %q, want %q", spec.Inbound.Mapping.ThreadID.Fallback, "ts")
	}
	if spec.Inbound.Dedup.TTL != 5*time.Minute {
		t.Errorf("Dedup.TTL = %v, want 5m", spec.Inbound.Dedup.TTL)
	}
	if spec.Outbound.Chunking.MaxLength != 2000 {
		t.Errorf("Chunking.MaxLength = %d, want 2000", spec.Outbound.Chunking.MaxLength)
	}
	if len(spec.Hooks.OnReceive) != 1 {
		t.Fatalf("OnReceive hooks = %d, want 1", len(spec.Hooks.OnReceive))
	}
	if spec.ToolsFile != "tools.yaml" {
		t.Errorf("ToolsFile = %q, want %q", spec.ToolsFile, "tools.yaml")
	}
}

func TestLoadYAMLChannelSpec(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: file-test
name: File Test
capabilities:
  threads: false
connection:
  url: "wss://example.com"
inbound:
  event_path: "event"
  mapping:
    conversation_id: "channel"
    content: "text"
outbound:
  send:
    method: POST
    url: "https://example.com/send"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	spec, err := LoadYAMLChannelSpec(path)
	if err != nil {
		t.Fatalf("LoadYAMLChannelSpec: %v", err)
	}
	if spec.ID != "file-test" {
		t.Errorf("ID = %q, want %q", spec.ID, "file-test")
	}
}

func TestLoadYAMLChannelSpecMissingID(t *testing.T) {
	specYAML := `
kind: channel
version: 1
name: No ID
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error for missing ID")
	}
}

func TestCapabilitiesSpecResponseFormat(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: slack-test
name: Slack Test
capabilities:
  threads: true
  response_format: slack
  response_format_prompt: "Use Slack mrkdwn please."
`
	var spec YAMLChannelSpec
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Capabilities.ResponseFormat != pkg.FormatSlack {
		t.Errorf("ResponseFormat = %q, want %q", spec.Capabilities.ResponseFormat, pkg.FormatSlack)
	}
	if spec.Capabilities.ResponseFormatPrompt != "Use Slack mrkdwn please." {
		t.Errorf("ResponseFormatPrompt = %q, want %q", spec.Capabilities.ResponseFormatPrompt, "Use Slack mrkdwn please.")
	}
}

func TestCapabilitiesSpecResponseFormatAbsent(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: plain-test
name: Plain Test
capabilities:
  threads: false
`
	var spec YAMLChannelSpec
	if err := yaml.Unmarshal([]byte(specYAML), &spec); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Capabilities.ResponseFormat != "" {
		t.Errorf("ResponseFormat should be empty when not set, got %q", spec.Capabilities.ResponseFormat)
	}
	if spec.Capabilities.ResponseFormatPrompt != "" {
		t.Errorf("ResponseFormatPrompt should be empty when not set, got %q", spec.Capabilities.ResponseFormatPrompt)
	}
}

func TestLoadYAMLChannelSpecUnknownResponseFormat(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: bad-format
name: Bad Format
capabilities:
  response_format: slak
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error for unknown response_format")
	}
	if !strings.Contains(err.Error(), "slak") {
		t.Errorf("error should mention the bad value, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecKnownResponseFormats(t *testing.T) {
	formats := []string{"text", "markdown", "slack", "html", "telegram", "teams", "whatsapp", "discord"}
	for _, f := range formats {
		t.Run(f, func(t *testing.T) {
			specYAML := "kind: channel\nversion: 1\nid: test\nname: Test\ncapabilities:\n  response_format: " + f + "\n"
			dir := t.TempDir()
			path := filepath.Join(dir, "channel.yaml")
			if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
				t.Fatalf("write spec: %v", err)
			}
			if _, err := LoadYAMLChannelSpec(path); err != nil {
				t.Errorf("unexpected error for valid format %q: %v", f, err)
			}
		})
	}
}

func TestLoadYAMLChannelSpecLongResponseFormatPrompt(t *testing.T) {
	// A prompt over 500 chars should load successfully (warning only, not an error).
	longPrompt := strings.Repeat("x", 501)
	specYAML := "kind: channel\nversion: 1\nid: long-prompt\nname: Long\ncapabilities:\n  response_format_prompt: " + longPrompt + "\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	spec, err := LoadYAMLChannelSpec(path)
	if err != nil {
		t.Fatalf("expected no error for long prompt, got: %v", err)
	}
	if len(spec.Capabilities.ResponseFormatPrompt) != 501 {
		t.Errorf("prompt length = %d, want 501", len(spec.Capabilities.ResponseFormatPrompt))
	}
}

func TestLoadYAMLChannelSpecDispatchRequiresAuth(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: gitlab-test
name: GitLab Test
inbound:
  http_webhook:
    path: /webhook
  event_path: "event"
  dispatch:
    when:
      - field: object_kind
        equals: note
    call:
      method: POST
      url: "https://gitlab.example.com/trigger"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error: dispatch on an unauthenticated public webhook must be rejected")
	}
	if !strings.Contains(err.Error(), "dispatch") {
		t.Errorf("error should mention dispatch, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecDispatchAllowedWithJWT(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: gitlab-test
name: GitLab Test
inbound:
  http_webhook:
    path: /webhook
    validate_jwt: true
    oidc_endpoint: "https://example.com/.well-known/openid-configuration"
    audience: "aud"
    issuer: "iss"
  event_path: "event"
  dispatch:
    when:
      - field: object_kind
        equals: note
    call:
      method: POST
      url: "https://gitlab.example.com/trigger"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if _, err := LoadYAMLChannelSpec(path); err != nil {
		t.Fatalf("unexpected error with validate_jwt set: %v", err)
	}
}

func TestLoadYAMLChannelSpecDispatchAllowedWithSecretHeader(t *testing.T) {
	specYAML := `
kind: channel
version: 1
id: gitlab-test
name: GitLab Test
inbound:
  http_webhook:
    path: /webhook
    secret_header: "X-Gitlab-Token"
    secret_value: "{{env.GITLAB_WEBHOOK_SECRET}}"
  event_path: "event"
  dispatch:
    when:
      - field: object_kind
        equals: note
    call:
      method: POST
      url: "https://gitlab.example.com/trigger"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if _, err := LoadYAMLChannelSpec(path); err != nil {
		t.Fatalf("unexpected error with secret_header set: %v", err)
	}
}

func TestLoadYAMLChannelSpecDispatchWithoutWebhookAllowed(t *testing.T) {
	// dispatch is only attacker-reachable via an inbound.http_webhook; a
	// websocket/polling channel with dispatch configured is not a public
	// endpoint, so no auth requirement applies.
	specYAML := `
kind: channel
version: 1
id: ws-test
name: WS Test
connection:
  url: "wss://example.com"
inbound:
  event_path: "event"
  dispatch:
    when:
      - field: object_kind
        equals: note
    call:
      method: POST
      url: "https://example.com/trigger"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	if _, err := LoadYAMLChannelSpec(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeSpecFile(t *testing.T, specYAML string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "channel.yaml")
	if err := os.WriteFile(path, []byte(specYAML), 0644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

func signatureSpecYAML(authLines string) string {
	return `
kind: channel
version: 1
id: gitlab-test
name: GitLab Test
inbound:
  http_webhook:
    path: /webhook
` + authLines + `
  event_path: "event"
  dispatch:
    when:
      - field: object_kind
        equals: note
    call:
      method: POST
      url: "https://gitlab.example.com/trigger"
`
}

func TestLoadYAMLChannelSpecDispatchAllowedWithSignature(t *testing.T) {
	for _, scheme := range []string{"github", "standard_webhooks"} {
		path := writeSpecFile(t, signatureSpecYAML(`    signature_scheme: "`+scheme+`"
    signature_secret: "{{env.SIGNING_SECRET}}"`))
		spec, err := LoadYAMLChannelSpec(path)
		if err != nil {
			t.Fatalf("scheme %s: unexpected error: %v", scheme, err)
		}
		if spec.Inbound.HTTPWebhook.SignatureScheme != scheme {
			t.Errorf("scheme %s: parsed signature_scheme %q", scheme, spec.Inbound.HTTPWebhook.SignatureScheme)
		}
		if spec.Inbound.HTTPWebhook.SignatureSecret != "{{env.SIGNING_SECRET}}" {
			t.Errorf("scheme %s: parsed signature_secret %q", scheme, spec.Inbound.HTTPWebhook.SignatureSecret)
		}
	}
}

func TestLoadYAMLChannelSpecUnknownSignatureScheme(t *testing.T) {
	for _, scheme := range []string{"gitlab", "sha1", "GITHUB", " github"} {
		path := writeSpecFile(t, signatureSpecYAML(`    signature_scheme: "`+scheme+`"
    signature_secret: "x"`))
		_, err := LoadYAMLChannelSpec(path)
		if err == nil {
			t.Fatalf("scheme %q: expected error for unknown signature_scheme", scheme)
		}
		if !strings.Contains(err.Error(), "signature_scheme") {
			t.Errorf("scheme %q: error should mention signature_scheme, got: %v", scheme, err)
		}
	}
}

func TestLoadYAMLChannelSpecSignatureSchemeWithoutSecret(t *testing.T) {
	path := writeSpecFile(t, signatureSpecYAML(`    signature_scheme: "github"`))
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error: signature_scheme without signature_secret must be rejected")
	}
	if !strings.Contains(err.Error(), "signature_secret") {
		t.Errorf("error should mention signature_secret, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecSignatureSecretWithoutScheme(t *testing.T) {
	path := writeSpecFile(t, signatureSpecYAML(`    signature_secret: "{{env.SIGNING_SECRET}}"`))
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error: signature_secret without signature_scheme would silently leave the webhook unauthenticated")
	}
	if !strings.Contains(err.Error(), "signature_scheme") {
		t.Errorf("error should mention signature_scheme, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecSignatureWithoutDispatchStillValidated(t *testing.T) {
	path := writeSpecFile(t, `
kind: channel
version: 1
id: hook-test
name: Hook Test
inbound:
  http_webhook:
    path: /webhook
    signature_scheme: "md5"
    signature_secret: "x"
`)
	if _, err := LoadYAMLChannelSpec(path); err == nil {
		t.Fatal("expected error for unknown signature_scheme even without dispatch")
	}
}

func TestLoadYAMLChannelSpecSecretHeaderWithoutValue(t *testing.T) {
	path := writeSpecFile(t, signatureSpecYAML(`    secret_header: "X-Gitlab-Token"`))
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error: secret_header without secret_value rejects every request")
	}
	if !strings.Contains(err.Error(), "secret_value") {
		t.Errorf("error should mention secret_value, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecSecretValueWithoutHeader(t *testing.T) {
	path := writeSpecFile(t, `
kind: channel
version: 1
id: hook-test
name: Hook Test
inbound:
  http_webhook:
    path: /webhook
    secret_value: "{{env.GITLAB_WEBHOOK_SECRET}}"
`)
	_, err := LoadYAMLChannelSpec(path)
	if err == nil {
		t.Fatal("expected error: secret_value without secret_header would silently leave the webhook unauthenticated")
	}
	if !strings.Contains(err.Error(), "secret_header") {
		t.Errorf("error should mention secret_header, got: %v", err)
	}
}

func TestLoadYAMLChannelSpecSecretHeaderWithoutValueRejectedWithoutDispatch(t *testing.T) {
	path := writeSpecFile(t, `
kind: channel
version: 1
id: hook-test
name: Hook Test
inbound:
  http_webhook:
    path: /webhook
    secret_header: "X-Gitlab-Token"
`)
	if _, err := LoadYAMLChannelSpec(path); err == nil {
		t.Fatal("expected error for secret_header without secret_value even without dispatch")
	}
}

func TestLoadYAMLChannelSpecDispatchOnPollingNeedsNoWebhookAuth(t *testing.T) {
	path := writeSpecFile(t, `
kind: channel
version: 1
id: poll-test
name: Poll Test
inbound:
  polling:
    url: "https://api.example.com/updates"
  event_path: "event"
  dispatch:
    when:
      - field: kind
        equals: note
    call:
      method: POST
      url: "https://api.example.com/trigger"
`)
	if _, err := LoadYAMLChannelSpec(path); err != nil {
		t.Fatalf("dispatch on a polling transport exposes no public endpoint and must load without webhook auth: %v", err)
	}
}
