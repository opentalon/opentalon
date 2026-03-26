package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	pkg "github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Client connects to a running plugin over gRPC
// and implements orchestrator.PluginExecutor.
type Client struct {
	conn   *grpc.ClientConn
	client pluginpb.PluginServiceClient
	name   string
	caps   orchestrator.PluginCapability
}

// Dial connects to a plugin at the given network/address via gRPC and fetches
// its capabilities, passing configJSON to the plugin during the handshake.
func Dial(network, address string, timeout time.Duration, configJSON string) (*Client, error) {
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
		return nil, fmt.Errorf("dial plugin at %s://%s: %w", network, address, err)
	}

	c := &Client{
		conn:   cc,
		client: pluginpb.NewPluginServiceClient(cc),
	}
	if err := c.fetchCapabilities(ctx, configJSON); err != nil {
		_ = cc.Close()
		return nil, err
	}
	return c, nil
}

// DialFromHandshake connects using information from a handshake.
func DialFromHandshake(hs pkg.Handshake, timeout time.Duration, configJSON string) (*Client, error) {
	return Dial(hs.Network, hs.Address, timeout, configJSON)
}

func (c *Client) fetchCapabilities(ctx context.Context, configJSON string) error {
	if _, err := c.client.Init(ctx, &pluginpb.PluginInitRequest{ConfigJson: configJSON}); err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unimplemented {
			return fmt.Errorf("init plugin: plugin does not implement Init — update the plugin to the latest SDK (go get github.com/opentalon/opentalon@latest)")
		}
		return fmt.Errorf("init plugin: %w", err)
	}
	resp, err := c.client.Capabilities(ctx, &emptypb.Empty{})
	if err != nil {
		return fmt.Errorf("fetch capabilities: %w", err)
	}

	c.name = resp.Name
	c.caps = toPluginCapability(resp)
	return nil
}

// Name returns the plugin's registered name.
func (c *Client) Name() string { return c.name }

// Capability returns the plugin's declared capabilities.
func (c *Client) Capability() orchestrator.PluginCapability { return c.caps }

// Execute sends a tool call to the plugin and returns the result.
// It implements orchestrator.PluginExecutor.
func (c *Client) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	return c.ExecuteContext(context.Background(), call)
}

// ExecuteContext is like Execute but respects context cancellation.
func (c *Client) ExecuteContext(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	resp, err := c.client.Execute(ctx, &pluginpb.ToolCallRequest{
		Id:     call.ID,
		Plugin: call.Plugin,
		Action: call.Action,
		Args:   call.Args,
	})
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("grpc: %v", err)}
	}
	return orchestrator.ToolResult{
		CallID:  resp.CallId,
		Content: resp.Content,
		Error:   resp.Error,
	}
}

// Close terminates the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

func toPluginCapability(pb *pluginpb.PluginCapabilities) orchestrator.PluginCapability {
	actions := make([]orchestrator.Action, len(pb.Actions))
	for i, a := range pb.Actions {
		params := make([]orchestrator.Parameter, len(a.Parameters))
		for j, p := range a.Parameters {
			params[j] = orchestrator.Parameter{
				Name:        p.Name,
				Description: p.Description,
				Required:    p.Required,
			}
		}
		actions[i] = orchestrator.Action{
			Name:        a.Name,
			Description: a.Description,
			Parameters:  params,
			UserOnly:    a.UserOnly,
		}
	}
	return orchestrator.PluginCapability{
		Name:        pb.Name,
		Description: pb.Description,
		Actions:     actions,
	}
}
