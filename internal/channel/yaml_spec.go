package channel

import (
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"gopkg.in/yaml.v3"
)

// YAMLChannelSpec is the top-level structure of a channel.yaml file.
type YAMLChannelSpec struct {
	Kind    string `yaml:"kind"`
	Version int    `yaml:"version"`
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`

	Capabilities CapabilitiesSpec `yaml:"capabilities"`
	RequiredEnv  []string         `yaml:"required_env"`

	Init       []InitStep     `yaml:"init"`
	Connection ConnectionSpec `yaml:"connection"`
	Inbound    InboundSpec    `yaml:"inbound"`
	Outbound   OutboundSpec   `yaml:"outbound"`
	Hooks      HooksSpec      `yaml:"hooks"`
	ToolsFile  string         `yaml:"tools_file"`
}

// CapabilitiesSpec declares what the channel supports.
type CapabilitiesSpec struct {
	Threads              bool               `yaml:"threads"`
	Files                bool               `yaml:"files"`
	Reactions            bool               `yaml:"reactions"`
	Edits                bool               `yaml:"edits"`
	MaxMessageLength     int                `yaml:"max_message_length"`
	ResponseFormat       pkg.ResponseFormat `yaml:"response_format"`
	ResponseFormatPrompt string             `yaml:"response_format_prompt"`
}

// InitStep is an HTTP call to run at startup. Results are stored in selfVars.
type InitStep struct {
	Name    string            `yaml:"name"`
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Store   map[string]string `yaml:"store"` // key = self var name, value = response JSON field
}

// ConnectionSpec describes the WebSocket connection.
type ConnectionSpec struct {
	URL       string        `yaml:"url"`
	Reconnect ReconnectSpec `yaml:"reconnect"`
}

// ReconnectSpec controls automatic reconnection.
type ReconnectSpec struct {
	Enabled        bool          `yaml:"enabled"`
	BackoffInitial time.Duration `yaml:"backoff_initial"`
	BackoffMax     time.Duration `yaml:"backoff_max"`
	ReInit         []string      `yaml:"re_init"` // init step names to re-run before reconnect
}

// InboundSpec describes how to process incoming messages from any source.
type InboundSpec struct {
	HTTPWebhook       *WebhookInboundSpec `yaml:"http_webhook"`
	Polling           *PollingInboundSpec `yaml:"polling"`
	Ack               AckSpec             `yaml:"ack"`
	EventPath         string              `yaml:"event_path"`
	EventTypes        []string            `yaml:"event_types"`
	AlwaysProcessWhen *FieldMatch         `yaml:"always_process_when"`
	ProcessWhen       []ProcessRule       `yaml:"process_when"`
	Skip              []SkipRule          `yaml:"skip"`
	Mapping           MappingSpec         `yaml:"mapping"`
	Media             []MediaRule         `yaml:"media"` // detect non-text messages, resolve files or inject descriptions
	Transforms        []Transform         `yaml:"transforms"`
	Dedup             DedupSpec           `yaml:"dedup"`
}

// MediaRule describes how to handle a non-text message type (e.g. photo, voice, sticker).
// Rules are evaluated in order; the first matching rule wins.
type MediaRule struct {
	// When is the event field whose presence triggers this rule (e.g. "photo", "voice", "sticker").
	When string `yaml:"when"`
	// Description is a template injected as message content when the original text is empty.
	// The LLM sees this and responds naturally. Supports {{event.*}} templates.
	Description string `yaml:"description"`
	// Resolve optionally downloads binary data (images, documents) to attach as a file.
	// When absent, only the description text is injected (for unsupported types like voice/video).
	Resolve *MediaResolveSpec `yaml:"resolve"`
}

// MediaResolveSpec describes how to download a media file via HTTP.
type MediaResolveSpec struct {
	// MimeType of the resolved file. Static (e.g. "image/jpeg") or template (e.g. "{{event.document.mime_type}}").
	MimeType string `yaml:"mime_type"`
	// Name of the resolved file. Optional template (e.g. "{{event.document.file_name}}").
	Name string `yaml:"name"`
	// Steps are sequential HTTP calls to resolve a file reference to binary data.
	// Intermediate steps (with Store) parse JSON and store fields in a "resolve" template context.
	// The final step (without Store) captures the raw response body as binary.
	Steps []MediaResolveStep `yaml:"steps"`
}

// MediaResolveStep is one HTTP call in a media resolution chain.
type MediaResolveStep struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	// Store maps self-var names to JSON response fields (like init step Store).
	// When present, the response is parsed as JSON. When absent, the response
	// body is treated as raw binary (this should be the final step).
	Store map[string]string `yaml:"store"`
}

// ProcessRule defines when to process an event (allowlist). If any
// process_when rules are configured, at least one must match for the
// event to be processed. Rules are OR'd together.
type ProcessRule struct {
	Field    string `yaml:"field"`
	Equals   string `yaml:"equals"`    // match exact value (supports templates)
	Contains string `yaml:"contains"`  // match substring (supports templates)
	NotEmpty *bool  `yaml:"not_empty"` // match if field is non-empty
}

// PollingInboundSpec configures an HTTP polling loop for inbound messages.
type PollingInboundSpec struct {
	Method   string            `yaml:"method"` // HTTP method (default GET)
	URL      string            `yaml:"url"`    // endpoint to poll (supports templates)
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`     // for POST requests
	Interval time.Duration     `yaml:"interval"` // poll frequency (default 1s)
	// ResultPath navigates into the response JSON to find the array of events.
	// e.g. "result" for Telegram's {"ok":true,"result":[...]}
	ResultPath string `yaml:"result_path"`
	// CursorField is the event field used for offset/cursor tracking.
	// After each poll, the max value of this field + 1 is stored in
	// self.poll_offset, which the URL template can reference.
	// e.g. "update_id" for Telegram's getUpdates API.
	CursorField string `yaml:"cursor_field"`
}

// AckSpec describes how to acknowledge a frame.
type AckSpec struct {
	When string `yaml:"when"` // top-level field name that triggers ack
	Send string `yaml:"send"` // template for ack payload
}

// FieldMatch matches a single field value.
type FieldMatch struct {
	Field  string `yaml:"field"`
	Equals string `yaml:"equals"`
}

// SkipRule defines when to skip processing an event.
type SkipRule struct {
	Field    string   `yaml:"field"`
	Equals   string   `yaml:"equals"`
	NotEmpty *bool    `yaml:"not_empty"` // pointer to distinguish unset from false
	Except   []string `yaml:"except"`
}

// MappingField supports both simple string and object-with-fallback forms.
// Simple: "channel" (just a field name)
// Object: { field: "thread_ts", fallback: "ts" }
type MappingField struct {
	Field    string `yaml:"field"`
	Fallback string `yaml:"fallback"`
}

// UnmarshalYAML allows MappingField to be either a plain string or a map.
func (mf *MappingField) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		mf.Field = value.Value
		return nil
	}
	// Decode as struct (field + fallback)
	type raw MappingField
	return value.Decode((*raw)(mf))
}

// MappingSpec maps event fields to InboundMessage fields.
type MappingSpec struct {
	ConversationID MappingField      `yaml:"conversation_id"`
	SenderID       MappingField      `yaml:"sender_id"`
	Content        MappingField      `yaml:"content"`
	ThreadID       MappingField      `yaml:"thread_id"`
	Metadata       map[string]string `yaml:"metadata"` // key = metadata key, value = event field name
	// Files is the event field name whose value is an array of file objects.
	// Each object must have: name (string), mime_type (string), data (base64 string), size (number).
	Files string `yaml:"files"`
}

// Transform describes a text transformation step.
type Transform struct {
	Type        string `yaml:"type"`        // "replace" or "trim"
	Pattern     string `yaml:"pattern"`     // for replace: regex or literal (with templates)
	Replacement string `yaml:"replacement"` // for replace
	Regex       bool   `yaml:"regex"`       // if true, treat pattern as regexp
}

// WebhookInboundSpec configures an inbound HTTP webhook endpoint.
type WebhookInboundSpec struct {
	Path         string `yaml:"path"` // e.g. "/api/messages"
	Port         int    `yaml:"port"` // default 3978
	ValidateJWT  bool   `yaml:"validate_jwt"`
	OIDCEndpoint string `yaml:"oidc_endpoint"` // OpenID discovery URL for JWKS
	Audience     string `yaml:"audience"`      // expected JWT aud claim
	Issuer       string `yaml:"issuer"`        // expected JWT iss claim
	ResponseCode int    `yaml:"response_code"` // default 200
}

// DedupSpec configures event deduplication.
type DedupSpec struct {
	Key string        `yaml:"key"` // template for dedup key
	TTL time.Duration `yaml:"ttl"`
}

// OutboundSpec describes how to send messages.
type OutboundSpec struct {
	Chunking      ChunkingSpec      `yaml:"chunking"`
	Send          HTTPCallSpec      `yaml:"send"`
	Update        HTTPCallSpec      `yaml:"update"`         // optional: edit an existing message (for streaming); template has {{msg.message_id}}
	SendStoreID   string            `yaml:"send_store_id"`  // optional: JSON field in send response to capture as message ID (e.g. "ts" for Slack)
}

// ChunkingSpec configures message chunking.
type ChunkingSpec struct {
	MaxLength int `yaml:"max_length"`
}

// HTTPCallSpec is a templated HTTP call used for sends and hooks.
type HTTPCallSpec struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	When    string            `yaml:"when"` // optional: only run if template is non-empty
}

// HooksSpec groups lifecycle hooks.
type HooksSpec struct {
	OnReceive  []HTTPCallSpec `yaml:"on_receive"`
	OnResponse []HTTPCallSpec `yaml:"on_response"`
}
