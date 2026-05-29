package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"github.com/opentalon/opentalon/pkg/channel/channelpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

// PluginClient connects to a channel plugin over gRPC
// and implements the pkg.Channel interface.
type PluginClient struct {
	conn       *grpc.ClientConn
	client     channelpb.ChannelServiceClient
	caps       pkg.Capabilities
	instanceID string // per-instance identifier assigned by opentalon (config-map key)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// DialChannel connects to a channel plugin via gRPC and fetches its capabilities.
// For Unix sockets, use network="unix" and address="/path/to/socket".
// For TCP, use network="tcp" and address="host:port".
func DialChannel(network, address string, timeout time.Duration) (*PluginClient, error) {
	var target string
	switch network {
	case "unix":
		target = "unix:" + address
	case "tcp":
		target = address
	default:
		return nil, fmt.Errorf("unsupported network %q", network)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cc, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial channel at %s://%s: %w", network, address, err)
	}

	c := &PluginClient{
		conn:   cc,
		client: channelpb.NewChannelServiceClient(cc),
	}
	if err := c.fetchCapabilities(ctx); err != nil {
		_ = cc.Close()
		return nil, err
	}
	return c, nil
}

func (c *PluginClient) fetchCapabilities(ctx context.Context) error {
	resp, err := c.client.Capabilities(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("fetch capabilities: %w", err)
	}
	c.caps = capabilitiesFromProto(resp)
	return nil
}

// ID returns the per-instance identifier. Defaults to the plugin's
// capability ID (its kind) for single-instance setups where opentalon never
// called SetInstanceID; multi-instance configurations override this via
// SetInstanceID before Start.
func (c *PluginClient) ID() string {
	if c.instanceID != "" {
		return c.instanceID
	}
	return c.caps.ID
}

// Kind returns the channel TYPE as reported by the plugin in its
// Capabilities response (e.g. "slack"). Shared by all instances of the
// same plugin.
func (c *PluginClient) Kind() string { return c.caps.ID }

// SetInstanceID assigns the per-instance identifier opentalon uses for
// session/dedup/actor scoping. Manager calls this once before Start when
// the channel entry's config-map key differs from the plugin's spec id.
func (c *PluginClient) SetInstanceID(id string) { c.instanceID = id }

// Capabilities returns the channel's declared capabilities.
func (c *PluginClient) Capabilities() pkg.Capabilities { return c.caps }

// Configure sends channel-specific config to the plugin before start. The
// instance ID is forwarded too — plugins that emit InboundMessage.channel_id
// should use this value rather than their static spec id so two installations
// of the same plugin keep distinct identities across the wire.
func (c *PluginClient) Configure(config map[string]interface{}) error {
	s, err := structpb.NewStruct(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	_, err = c.client.Configure(context.Background(), &channelpb.ConfigureRequest{
		Config:     s,
		InstanceId: c.instanceID,
	})
	if err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	return nil
}

// Tools requests the channel's tool definitions.
func (c *PluginClient) Tools() ([]pkg.ToolDefinition, error) {
	resp, err := c.client.Tools(context.Background(), &emptypb.Empty{})
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	return toolsFromProto(resp.Tools), nil
}

// Start begins listening for inbound messages from the channel plugin.
// Messages are pushed into the provided inbox channel.
func (c *PluginClient) Start(ctx context.Context, inbox chan<- pkg.InboundMessage) error {
	c.ctx, c.cancel = context.WithCancel(ctx)

	stream, err := c.client.Start(c.ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("start stream: %w", err)
	}

	c.wg.Add(1)
	go c.receiveLoop(stream, inbox)
	return nil
}

func (c *PluginClient) receiveLoop(stream channelpb.ChannelService_StartClient, inbox chan<- pkg.InboundMessage) {
	defer c.wg.Done()
	for {
		msg, err := stream.Recv()
		if err != nil {
			return
		}
		converted := inboundFromProto(msg)
		// Host-side instance stamp: opentalon assigned this client's
		// instance id at construction; trust it over whatever the
		// plugin chose to put in channel_id. A misconfigured or older
		// plugin can therefore never make two instances of itself
		// collide on session/dedup/actor keys.
		if c.instanceID != "" {
			converted.ChannelID = c.instanceID
		}
		select {
		case inbox <- converted:
		case <-c.ctx.Done():
			return
		}
	}
}

// Send dispatches an outbound message to the channel plugin.
func (c *PluginClient) Send(ctx context.Context, msg pkg.OutboundMessage) error {
	_, err := c.client.Send(ctx, outboundToProto(msg))
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}

// Stop shuts down the channel connection.
func (c *PluginClient) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	c.wg.Wait()
	return c.conn.Close()
}

// --- Proto conversion helpers (delegate to pkg/channel) ---

func capabilitiesFromProto(pb *channelpb.ChannelCapabilities) pkg.Capabilities {
	if pb == nil {
		return pkg.Capabilities{}
	}
	return pkg.Capabilities{
		ID:                   pb.Id,
		Name:                 pb.Name,
		Threads:              pb.Threads,
		Files:                pb.Files,
		Reactions:            pb.Reactions,
		Edits:                pb.Edits,
		MaxMessageLength:     pb.MaxMessageLength,
		ResponseFormat:       pkg.ResponseFormat(pb.ResponseFormat),
		ResponseFormatPrompt: pb.ResponseFormatPrompt,
	}
}

func inboundFromProto(pb *channelpb.InboundMessage) pkg.InboundMessage {
	if pb == nil {
		return pkg.InboundMessage{}
	}
	// Kind: prefer the explicit field; older plugins set channel_id with
	// their spec id (which was the kind in the single-instance world), so
	// fall back to it. ChannelID stamping happens in receiveLoop after
	// this call so the host-known instance id always wins; we keep the
	// pb.ChannelId fallback here for stand-alone inboundFromProto callers
	// (tests, replay).
	kind := pb.Kind
	if kind == "" {
		kind = pb.ChannelId
	}
	m := pkg.InboundMessage{
		ChannelID:      pb.ChannelId,
		Kind:           kind,
		ConversationID: pb.ConversationId,
		ThreadID:       pb.ThreadId,
		SenderID:       pb.SenderId,
		SenderName:     pb.SenderName,
		Content:        pb.Content,
		Metadata:       pb.Metadata,
	}
	if pb.Timestamp != nil {
		m.Timestamp = pb.Timestamp.AsTime()
	}
	for _, f := range pb.Files {
		if f != nil {
			m.Files = append(m.Files, pkg.FileAttachment{
				Name:     f.Name,
				MimeType: f.MimeType,
				Data:     f.Data,
				Size:     f.Size,
			})
		}
	}
	return m
}

func outboundToProto(m pkg.OutboundMessage) *channelpb.OutboundMessage {
	pb := &channelpb.OutboundMessage{
		ConversationId: ensureValidUTF8(m.ConversationID),
		ThreadId:       ensureValidUTF8(m.ThreadID),
		Content:        ensureValidUTF8(m.Content),
		Metadata:       ensureValidUTF8Map(m.Metadata),
	}
	for _, f := range m.Files {
		pb.Files = append(pb.Files, &channelpb.FileAttachment{
			Name:     f.Name,
			MimeType: f.MimeType,
			Data:     f.Data,
			Size:     f.Size,
		})
	}
	return pb
}

// ensureValidUTF8 replaces invalid UTF-8 sequences with U+FFFD.
// gRPC proto marshaling rejects string fields with invalid UTF-8.
func ensureValidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\ufffd")
}

func ensureValidUTF8Map(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	for k, v := range m {
		if !utf8.ValidString(v) {
			m[k] = strings.ToValidUTF8(v, "\ufffd")
		}
	}
	return m
}

func toolsFromProto(pbs []*channelpb.ToolDefinition) []pkg.ToolDefinition {
	out := make([]pkg.ToolDefinition, len(pbs))
	for i, pb := range pbs {
		params := make([]pkg.ToolParam, len(pb.Parameters))
		for j, p := range pb.Parameters {
			params[j] = pkg.ToolParam{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		out[i] = pkg.ToolDefinition{
			Plugin:      pb.Plugin,
			Description: pb.Description,
			Action:      pb.Action,
			ActionDesc:  pb.ActionDescription,
			Method:      pb.Method,
			URL:         pb.Url,
			Body:        pb.Body,
			Headers:     pb.Headers,
			RequiredEnv: pb.RequiredEnv,
			Parameters:  params,
		}
	}
	return out
}
