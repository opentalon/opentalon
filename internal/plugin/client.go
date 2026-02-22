package plugin

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

// Client connects to a running plugin over a Unix socket or TCP
// and implements orchestrator.PluginExecutor.
type Client struct {
	mu   sync.Mutex
	conn net.Conn
	name string
	caps orchestrator.PluginCapability
}

// Dial connects to a plugin at the given network/address and fetches
// its capabilities.
func Dial(network, address string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial plugin at %s://%s: %w", network, address, err)
	}

	c := &Client{conn: conn}
	if err := c.fetchCapabilities(); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// DialFromHandshake connects using information from a handshake.
func DialFromHandshake(hs Handshake, timeout time.Duration) (*Client, error) {
	return Dial(hs.Network, hs.Address, timeout)
}

func (c *Client) fetchCapabilities() error {
	req := Request{Method: "capabilities"}
	if err := WriteMessage(c.conn, &req); err != nil {
		return fmt.Errorf("request capabilities: %w", err)
	}

	var resp Response
	if err := ReadMessage(c.conn, &resp); err != nil {
		return fmt.Errorf("read capabilities: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("capabilities error: %s", resp.Error)
	}
	if resp.Caps == nil {
		return fmt.Errorf("plugin returned empty capabilities")
	}

	c.name = resp.Caps.Name
	c.caps = toPluginCapability(resp.Caps)
	return nil
}

// Name returns the plugin's registered name.
func (c *Client) Name() string { return c.name }

// Capability returns the plugin's declared capabilities.
func (c *Client) Capability() orchestrator.PluginCapability { return c.caps }

// Execute sends a tool call to the plugin and returns the result.
// It implements orchestrator.PluginExecutor.
func (c *Client) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := Request{
		Method: "execute",
		ID:     call.ID,
		Plugin: call.Plugin,
		Action: call.Action,
		Args:   call.Args,
	}

	if err := WriteMessage(c.conn, &req); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("write: %v", err)}
	}

	var resp Response
	if err := ReadMessage(c.conn, &resp); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("read: %v", err)}
	}

	return orchestrator.ToolResult{
		CallID:  resp.CallID,
		Content: resp.Content,
		Error:   resp.Error,
	}
}

// ExecuteContext is like Execute but respects context cancellation.
func (c *Client) ExecuteContext(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	done := make(chan orchestrator.ToolResult, 1)
	go func() { done <- c.Execute(call) }()

	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		return orchestrator.ToolResult{CallID: call.ID, Error: ctx.Err().Error()}
	}
}

// Close terminates the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

func toPluginCapability(msg *CapabilitiesMsg) orchestrator.PluginCapability {
	actions := make([]orchestrator.Action, len(msg.Actions))
	for i, a := range msg.Actions {
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
		}
	}
	return orchestrator.PluginCapability{
		Name:        msg.Name,
		Description: msg.Description,
		Actions:     actions,
	}
}
