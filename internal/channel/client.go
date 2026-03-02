package channel

import (
	"context"
	"fmt"
	"sync"
	"time"

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
	conn   *grpc.ClientConn
	client channelpb.ChannelServiceClient
	caps   pkg.Capabilities

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

// ID returns the channel's unique identifier.
func (c *PluginClient) ID() string { return c.caps.ID }

// Capabilities returns the channel's declared capabilities.
func (c *PluginClient) Capabilities() pkg.Capabilities { return c.caps }

// Configure sends channel-specific config to the plugin before start.
func (c *PluginClient) Configure(config map[string]interface{}) error {
	s, err := structpb.NewStruct(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	_, err = c.client.Configure(context.Background(), &channelpb.ConfigureRequest{Config: s})
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
		select {
		case inbox <- inboundFromProto(msg):
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
		ID:               pb.Id,
		Name:             pb.Name,
		Threads:          pb.Threads,
		Files:            pb.Files,
		Reactions:        pb.Reactions,
		Edits:            pb.Edits,
		MaxMessageLength: pb.MaxMessageLength,
	}
}

func inboundFromProto(pb *channelpb.InboundMessage) pkg.InboundMessage {
	if pb == nil {
		return pkg.InboundMessage{}
	}
	m := pkg.InboundMessage{
		ChannelID:      pb.ChannelId,
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
		ConversationId: m.ConversationID,
		ThreadId:       m.ThreadID,
		Content:        m.Content,
		Metadata:       m.Metadata,
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
