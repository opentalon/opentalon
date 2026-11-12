package channel

import (
	"context"
	"fmt"
	"time"

	"github.com/opentalon/opentalon/internal/state"
)

// InboundMessage is a message from a channel (user → core).
//
// ChannelID and Kind are deliberately separate so a single opentalon process
// can host multiple instances of the same channel type (e.g. an admin Slack
// bot and a customer Slack bot). ChannelID is the per-instance identifier
// assigned by the host (config-map key under `channels:`); two bots of the
// same kind have distinct ChannelIDs and therefore distinct session keys,
// dedup keys, and actor scopes. Kind is the channel TYPE (`"slack"`,
// `"telegram"`, `"console"`) and drives type-based routing such as content
// preparer registration and the WhoAmI channel_type header.
type InboundMessage struct {
	ChannelID      string            `yaml:"channel_id" json:"channel_id"`
	Kind           string            `yaml:"kind,omitempty" json:"kind,omitempty"`
	ConversationID string            `yaml:"conversation_id" json:"conversation_id"`
	ThreadID       string            `yaml:"thread_id" json:"thread_id"`
	SenderID       string            `yaml:"sender_id" json:"sender_id"`
	SenderName     string            `yaml:"sender_name" json:"sender_name"`
	Content        string            `yaml:"content" json:"content"`
	Files          []FileAttachment  `yaml:"files,omitempty" json:"files,omitempty"`
	Metadata       map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
	Timestamp      time.Time         `yaml:"timestamp" json:"timestamp"`
}

// OutboundMessage is a message from core to a channel.
type OutboundMessage struct {
	ConversationID string            `yaml:"conversation_id" json:"conversation_id"`
	ThreadID       string            `yaml:"thread_id" json:"thread_id"`
	Content        string            `yaml:"content" json:"content"`
	Files          []FileAttachment  `yaml:"files,omitempty" json:"files,omitempty"`
	Metadata       map[string]string `yaml:"metadata,omitempty" json:"metadata,omitempty"`
}

// FileAttachment describes a file sent with a message.
type FileAttachment struct {
	Name     string `yaml:"name" json:"name"`
	MimeType string `yaml:"mime_type" json:"mime_type"`
	Data     []byte `yaml:"data,omitempty" json:"data,omitempty"`
	Size     int64  `yaml:"size" json:"size"`
}

// ResponseFormat identifies the output format a channel expects.
// The LLM uses this to format its replies appropriately.
type ResponseFormat string

const (
	FormatText     ResponseFormat = "text"     // plain text, no markup
	FormatMarkdown ResponseFormat = "markdown" // standard CommonMark markdown
	FormatSlack    ResponseFormat = "slack"    // Slack mrkdwn (*bold*, _italic_, `code`, >quote)
	FormatHTML     ResponseFormat = "html"     // HTML tags
	FormatTelegram ResponseFormat = "telegram" // Telegram HTML subset
	FormatTeams    ResponseFormat = "teams"    // Microsoft Teams markdown (*bold*, _italic_, `code`, >quote)
	FormatWhatsApp ResponseFormat = "whatsapp" // WhatsApp markup (*bold*, _italic_, ~strikethrough~, ```code```)
	FormatDiscord  ResponseFormat = "discord"  // Discord markdown (**bold**, *italic*, `code`, ```code blocks```)
)

// Capabilities declares what a channel supports.
type Capabilities struct {
	ID                   string         `yaml:"id" json:"id"`
	Name                 string         `yaml:"name" json:"name"`
	Threads              bool           `yaml:"threads" json:"threads"`
	Files                bool           `yaml:"files" json:"files"`
	Reactions            bool           `yaml:"reactions" json:"reactions"`
	Edits                bool           `yaml:"edits" json:"edits"`
	MaxMessageLength     int64          `yaml:"max_message_length" json:"max_message_length"`
	ResponseFormat       ResponseFormat `yaml:"response_format" json:"response_format"`
	ResponseFormatPrompt string         `yaml:"response_format_prompt" json:"response_format_prompt"`
}

type capabilitiesKey struct{}

// WithCapabilities stores channel capabilities in the context.
func WithCapabilities(ctx context.Context, caps Capabilities) context.Context {
	return context.WithValue(ctx, capabilitiesKey{}, caps)
}

// CapabilitiesFromContext returns the channel capabilities stored in ctx,
// or zero-value Capabilities if none were set.
func CapabilitiesFromContext(ctx context.Context) Capabilities {
	caps, _ := ctx.Value(capabilitiesKey{}).(Capabilities)
	return caps
}

// Channel is the interface that external channel plugins implement.
// The core uses this interface regardless of the underlying transport
// (binary subprocess, remote gRPC, Docker, webhook, or WebSocket).
//
// Concurrency contract: the registry dispatches responses in separate goroutines,
// so Send may be called concurrently from multiple goroutines. Implementations
// must make Send safe for concurrent use (e.g. a gRPC stub or HTTP client that
// serialises internally). ID, Capabilities, and Stop are called from a single
// goroutine and have no concurrency requirement.
type Channel interface {
	// ID returns the per-instance identifier (the config-map key under
	// `channels:`). Two instances of the same channel kind must return
	// different IDs so session keys, dedup keys, and actor scopes do not
	// collide.
	ID() string
	// Kind returns the channel TYPE (e.g. "slack", "telegram", "console")
	// used for type-based routing. Multiple instances share a Kind.
	// Implementations that predate this method can fall back to ID() at
	// the call site; opentalon does this via KindOf below.
	Kind() string
	Capabilities() Capabilities
	Start(ctx context.Context, inbox chan<- InboundMessage) error
	Send(ctx context.Context, msg OutboundMessage) error
	Stop() error
}

// KindOf returns ch.Kind() if non-empty, otherwise ch.ID(). The fallback
// exists for older channel implementations that have not yet been updated to
// distinguish kind from instance — for them, ID() still carries the kind
// because there is only one instance.
func KindOf(ch Channel) string {
	if k := ch.Kind(); k != "" {
		return k
	}
	return ch.ID()
}

// ConfigurableChannel is an optional interface that channels can implement
// to receive config from the host before Start is called.
type ConfigurableChannel interface {
	Configure(config map[string]interface{}) error
}

// ToolProvider is an optional interface that channels can implement
// to advertise tool definitions (e.g. channel-specific actions).
type ToolProvider interface {
	Tools() []ToolDefinition
}

// ToolDefinition describes one tool action a channel provides.
type ToolDefinition struct {
	Plugin      string            `json:"plugin"`
	Description string            `json:"description"`
	Action      string            `json:"action"`
	ActionDesc  string            `json:"action_description"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"`
	RequiredEnv []string          `json:"required_env"`
	Parameters  []ToolParam       `json:"parameters"`
}

// ToolParam describes one parameter of a tool action.
type ToolParam struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// PluginMode identifies how the core connects to a channel plugin.
type PluginMode int

const (
	ModeBinary    PluginMode = iota // local subprocess
	ModeGRPC                        // remote gRPC address
	ModeDocker                      // Docker container
	ModeWebhook                     // HTTP webhook
	ModeWebSocket                   // WebSocket connection
	ModeYAML                        // YAML-driven channel (in-process)
)

func (m PluginMode) String() string {
	switch m {
	case ModeBinary:
		return "binary"
	case ModeGRPC:
		return "grpc"
	case ModeDocker:
		return "docker"
	case ModeWebhook:
		return "webhook"
	case ModeWebSocket:
		return "websocket"
	case ModeYAML:
		return "yaml"
	default:
		return "unknown"
	}
}

// Runner runs a user message through the orchestrator and returns the response.
// InputForDisplay is optional (e.g. what was sent to the LLM); channels may use it for display.
// Metadata is optional key-value pairs from the orchestrator (e.g. type=system for commands).
// Files are optional binary attachments (images, documents, etc.) to include with the message.
type Runner interface {
	Run(ctx context.Context, sessionKey, content string, files ...FileAttachment) (response string, inputForDisplay string, metadata map[string]string, err error)
}

// RunActionFunc runs a single plugin action. Used by channel-specific preparers.
type RunActionFunc func(ctx context.Context, plugin, action string, args map[string]string) (string, error)

// HasActionFunc reports whether a plugin action is available.
type HasActionFunc func(plugin, action string) bool

// ContentPreparer is channel-specific pre-processing: it can transform user content
// before it is sent to the Runner. Channels register their preparer via RegisterContentPreparer in init().
type ContentPreparer func(ctx context.Context, content string, runAction RunActionFunc, hasAction HasActionFunc) string

// ResumeSessionFunc validates that a session exists for sessionKey. It returns
// nil when the session is present in the store. The handler distinguishes two
// failure modes via errors.Is against state.ErrSessionNotFound: a wrapped
// ErrSessionNotFound means the row is genuinely absent (expired, pruned,
// never existed) and surfaces as error_code=session_expired so the UI drops
// its stale conversation_id and reconnects fresh; any other error is treated
// as an infrastructure failure (e.g. dropped DB connection) and surfaces as
// error_code=internal_error so a valid conversation_id is preserved across
// a retry. Implementations MUST preserve this distinction — collapsing both
// onto a generic error reintroduces the silent-drift bug this contract
// exists to prevent. Named Resume rather than Load to avoid colliding with
// SessionStore.Load (the yaml-from-disk method on the in-memory store).
type ResumeSessionFunc func(sessionKey string) error

// CreateSessionFunc registers (or returns) a session row for p: the packed
// session key, the actor scope and the two labels (state.SessionParams). Kind
// is the session's interaction_kind ("chat" | "system"; empty is stored as
// "chat"). SystemSource is the per-feature label of the backend feature that
// opened the session (empty for a human chat, stored as NULL); it says which
// feature OPENED the conversation, while the per-run label on the usage row
// says which feature drove each turn — the two differ for a system turn
// injected into an existing chat. Idempotent: an existing session is returned
// untouched, never wiped, so both labels are set once at creation and never
// updated. The handler calls Create on messages NOT flagged with resume_intent
// — i.e. the channel did not have a client-supplied id and just minted one for
// this connection. Splitting Resume/Create replaces the previous
// EnsureSessionFunc which conflated the two paths and silently auto-created on
// any cache miss.
type CreateSessionFunc func(p state.SessionParams)

// ResumeIntentMetadataKey is the InboundMessage metadata key channels set
// to "true" when the conversation_id on the message came from the client
// (resume) rather than being server-minted on this connection (fresh).
// Handlers route Resume vs Create based on this flag.
//
// Trust boundary: this key is client-trustable because session_key is
// always entity-scoped at handler.go (sessionKey = p.EntityID + ":" + key)
// before either Resume or Create is invoked. A malicious client cannot
// resume a session belonging to another entity by forging this metadata —
// any guessed conversation_id is prefixed with the verified profile's
// EntityID, so cross-tenant lookup is structurally unreachable. Removing
// or reshaping that prefix in handler.go would re-open this surface.
const ResumeIntentMetadataKey = "resume_intent"

// ControlMetadataKey marks an inbound message as an out-of-band control signal
// rather than user chat input. The value names the specific control (see
// ControlResumeHello). Control messages never run the LLM: the handler routes
// them to a dedicated path and they bypass the debouncer. Content is empty, so
// they must be injected into the core inbox directly (a channel's normal read
// loop, which drops empty frames, is not the path for them).
const ControlMetadataKey = "control"

// ControlResumeHello is the ControlMetadataKey value a channel sends once,
// immediately after a client reconnects to an existing conversation, so the
// core can re-emit any still-pending tool-confirmation prompt and the reloaded
// UI can redraw its Approve/Reject buttons without waiting for the user to type.
// It carries ResumeIntentMetadataKey="true" (it is a resume) and the profile
// token so the handler can scope the session and validate it exists.
const ControlResumeHello = "resume_hello"

// OpenSessionMetadataKey="true" marks an inbound message as a server's
// request to open a chat session for the verified user and run its first
// turn, the message content, hidden from the transcript. Only a verified
// system invocation may send it. Keys of the request:
//
//   - OpenedByMetadataKey, OpenKeyMetadataKey: the origin label and the
//     idempotency key; the same user, origin and key always answer the same
//     session, and its first turn runs once.
//   - OpenSessionIDMetadataKey and NoteKeyMetadataKey instead: continue that
//     session of the user with the content as a hidden note, stored and
//     answered once per note key.
//   - OpenRequestIDMetadataKey: the attempt's correlation id.
//
// The core answers every attempt with one frame carrying that correlation id,
// sent before any model run: FrameSessionOpened, with session_id (the session
// key) and created ("true" exactly when this attempt took the turn's claim and
// so runs the first turn or the note), or FrameOpenRejected, with error_code
// forbidden, bad_request, not_found or internal_error, or the code any message
// gets for the same refusal (token_required, auth_failed, ...). No other frame
// carries the correlation id: the turn's reply and its error frame reach the
// session's conversation as every reply does.
//
// A request reaches only the sessions the store records for the user and the
// channel it arrives on, which only sessions opened this way record: any other
// note target, or the user's session for the origin and key on another
// channel, answers not_found like a missing one. A channel with threads, a
// request in a thread, or one whose channel id is not its channel's, is
// refused with bad_request.
const (
	OpenSessionMetadataKey   = "open_session"
	OpenedByMetadataKey      = "opened_by"
	OpenKeyMetadataKey       = "open_key"
	OpenSessionIDMetadataKey = "session_id"
	NoteKeyMetadataKey       = "note_key"
	OpenRequestIDMetadataKey = "open_request_id"

	FrameSessionOpened = "session_opened"
	FrameOpenRejected  = "open_rejected"
)

// OwnerEntityMetadataKey carries the conversation-owning entity on outbound
// metadata. This is the authoritative statement of the contract:
//
//   - The core stamps it after identity resolution (see StampOwnerEntity)
//     onto outbound metadata, so every frame derived from a verified inbound
//     message names the resolved owner entity.
//   - Multi-pod channels read it to gate cross-pod delivery: a frame fanned
//     out to other pods is delivered only to connections belonging to the
//     conversation owner.
//   - An EMPTY or absent owner means deliver anyway. This fail-open is
//     deliberate: under version skew (a core that does not stamp yet) frames
//     degrade to the pre-gate behavior instead of being dropped.
//   - Underscore-prefixed metadata keys are INTERNAL core-to-channel
//     plumbing. Channels must strip them from client-facing frames — a
//     client must never see them.
const OwnerEntityMetadataKey = "_owner_entity"

// StampOwnerEntity sets OwnerEntityMetadataKey=entity on meta (allocating the
// map when nil) and returns it; a no-op returning meta unchanged when entity
// is empty.
func StampOwnerEntity(meta map[string]string, entity string) map[string]string {
	if entity == "" {
		return meta
	}
	if meta == nil {
		meta = map[string]string{}
	}
	meta[OwnerEntityMetadataKey] = entity
	return meta
}

// MessageHandler is called when an inbound message arrives. The implementation
// feeds the message to the orchestrator and returns the response.
type MessageHandler func(ctx context.Context, sessionKey string, msg InboundMessage) (OutboundMessage, error)

// SessionKey builds a deterministic session identifier from the
// channel, conversation, and thread triple.
func SessionKey(channelID, conversationID, threadID string) string {
	if threadID == "" {
		return fmt.Sprintf("%s:%s", channelID, conversationID)
	}
	return fmt.Sprintf("%s:%s:%s", channelID, conversationID, threadID)
}
