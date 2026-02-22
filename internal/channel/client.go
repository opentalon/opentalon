package channel

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// PluginClient connects to a channel plugin over a Unix socket or TCP
// and implements the Channel interface.
type PluginClient struct {
	mu   sync.Mutex
	conn net.Conn
	caps Capabilities

	inbox  chan<- InboundMessage
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Single-reader demux: only receiveLoop reads from conn. When Send() is
	// waiting for an ack, the next response is delivered here so the second
	// user message is not consumed as a fake ack.
	ackMu      sync.Mutex
	pendingAck chan ChannelResponse
}

// DialChannel connects to a channel plugin and fetches its capabilities.
func DialChannel(network, address string, timeout time.Duration) (*PluginClient, error) {
	conn, err := net.DialTimeout(network, address, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial channel at %s://%s: %w", network, address, err)
	}

	c := &PluginClient{conn: conn}
	if err := c.fetchCapabilities(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// NewClientWithConn is for testing: it creates a client that uses the given conn
// and caps without dialing. Caller must run the server side on the other end of conn.
func NewClientWithConn(conn net.Conn, caps Capabilities) *PluginClient {
	return &PluginClient{conn: conn, caps: caps}
}

func (c *PluginClient) fetchCapabilities() error {
	req := ChannelRequest{Method: "capabilities"}
	if err := writeMsg(c.conn, &req); err != nil {
		return fmt.Errorf("request capabilities: %w", err)
	}

	var resp ChannelResponse
	if err := readMsg(c.conn, &resp); err != nil {
		return fmt.Errorf("read capabilities: %w", err)
	}
	if resp.Error != "" {
		return fmt.Errorf("capabilities error: %s", resp.Error)
	}
	if resp.Caps == nil {
		return fmt.Errorf("channel returned empty capabilities")
	}

	c.caps = *resp.Caps
	return nil
}

// ID returns the channel's unique identifier.
func (c *PluginClient) ID() string { return c.caps.ID }

// Capabilities returns the channel's declared capabilities.
func (c *PluginClient) Capabilities() Capabilities { return c.caps }

// Start begins listening for inbound messages from the channel plugin.
// Messages are pushed into the provided inbox channel.
func (c *PluginClient) Start(ctx context.Context, inbox chan<- InboundMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inbox = inbox
	c.ctx, c.cancel = context.WithCancel(ctx)

	// Tell the plugin to start streaming messages.
	req := ChannelRequest{Method: "start"}
	if err := writeMsg(c.conn, &req); err != nil {
		return fmt.Errorf("send start: %w", err)
	}

	c.wg.Add(1)
	go c.receiveLoop()
	return nil
}

func (c *PluginClient) receiveLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		var resp ChannelResponse
		if err := readMsg(c.conn, &resp); err != nil {
			select {
			case <-c.ctx.Done():
				return
			default:
			}
			return
		}

		// If Send() is waiting for an ack, give it this response so the next
		// inbound user message is not consumed as the ack.
		c.ackMu.Lock()
		pending := c.pendingAck
		if pending != nil {
			c.pendingAck = nil
		}
		c.ackMu.Unlock()
		if pending != nil {
			select {
			case pending <- resp:
			case <-c.ctx.Done():
				return
			}
			continue
		}
		if resp.Msg != nil {
			select {
			case c.inbox <- *resp.Msg:
			case <-c.ctx.Done():
				return
			}
		}
	}
}

// Send dispatches an outbound message to the channel plugin.
// The ack is received via the single reader (receiveLoop) so the next inbound
// user message is not mistakenly consumed as the ack.
func (c *PluginClient) Send(ctx context.Context, msg OutboundMessage) error {
	ackCh := make(chan ChannelResponse, 1)
	c.ackMu.Lock()
	if c.pendingAck != nil {
		c.ackMu.Unlock()
		return fmt.Errorf("concurrent Send not allowed")
	}
	c.pendingAck = ackCh
	c.ackMu.Unlock()

	c.mu.Lock()
	req := ChannelRequest{Method: "send", Msg: &msg}
	err := writeMsg(c.conn, &req)
	c.mu.Unlock()
	if err != nil {
		c.ackMu.Lock()
		c.pendingAck = nil
		c.ackMu.Unlock()
		return fmt.Errorf("send message: %w", err)
	}

	var resp ChannelResponse
	var done <-chan struct{}
	if c.ctx != nil {
		done = c.ctx.Done()
	}
	select {
	case resp = <-ackCh:
	case <-done:
		c.ackMu.Lock()
		c.pendingAck = nil
		c.ackMu.Unlock()
		if c.ctx != nil {
			return c.ctx.Err()
		}
		return context.Canceled
	}
	c.ackMu.Lock()
	c.pendingAck = nil
	c.ackMu.Unlock()
	if resp.Error != "" {
		return fmt.Errorf("send error: %s", resp.Error)
	}
	return nil
}

// Stop shuts down the channel connection.
func (c *PluginClient) Stop() error {
	if c.cancel != nil {
		c.cancel()
	}
	// Close the connection to unblock any pending readMsg in receiveLoop.
	err := c.conn.Close()
	c.wg.Wait()
	return err
}
