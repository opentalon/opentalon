package plugin

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/proto/pluginpb"
)

type concurrentCallbackStream struct {
	grpc.ClientStream
	callbacks int
	recvd     int
	sent      chan *pluginpb.HostMessage
	inSend    atomic.Int32
	overlaps  atomic.Int32
	lastSent  *pluginpb.HostMessage
}

func (s *concurrentCallbackStream) Send(m *pluginpb.HostMessage) error {
	if s.inSend.Add(1) > 1 {
		s.overlaps.Add(1)
	}
	s.lastSent = m
	time.Sleep(time.Millisecond)
	s.inSend.Add(-1)
	s.sent <- m
	return nil
}

func (s *concurrentCallbackStream) Recv() (*pluginpb.PluginMessage, error) {
	if s.recvd < s.callbacks {
		s.recvd++
		return &pluginpb.PluginMessage{Payload: &pluginpb.PluginMessage_CallbackRequest{
			CallbackRequest: &pluginpb.CallbackRequest{Id: fmt.Sprintf("cb-%d", s.recvd), Plugin: "_subprocess", Action: "run"},
		}}, nil
	}
	got := 0
	for m := range s.sent {
		if m.GetCallbackResponse() != nil {
			got++
		}
		if got == s.callbacks {
			break
		}
	}
	return &pluginpb.PluginMessage{Payload: &pluginpb.PluginMessage_Result{Result: &pluginpb.ToolResultResponse{CallId: "c1", Content: "done"}}}, nil
}

type concurrentCallbackService struct {
	pluginpb.PluginServiceClient
	stream *concurrentCallbackStream
}

func (c *concurrentCallbackService) ExecuteBidi(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[pluginpb.HostMessage, pluginpb.PluginMessage], error) {
	return c.stream, nil
}

type barrierCallbackHandler struct {
	wg sync.WaitGroup
}

func (b *barrierCallbackHandler) RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error) {
	content, _, err := b.RunActionResult(ctx, plugin, action, args)
	return content, err
}

func (b *barrierCallbackHandler) RunActionResult(context.Context, string, string, map[string]string) (string, string, error) {
	b.wg.Done()
	b.wg.Wait()
	return "reply", "", nil
}

func TestClient_ExecuteBidi_ConcurrentCallbackRepliesDoNotOverlapOnStream(t *testing.T) {
	const callbacks = 16
	stream := &concurrentCallbackStream{callbacks: callbacks, sent: make(chan *pluginpb.HostMessage, callbacks+1)}
	client := &Client{client: &concurrentCallbackService{stream: stream}}
	cb := &barrierCallbackHandler{}
	cb.wg.Add(callbacks)

	result := client.ExecuteBidi(context.Background(), orchestrator.ToolCall{ID: "c1", Plugin: "test", Action: "go"}, cb)

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.Content != "done" {
		t.Errorf("content: %q", result.Content)
	}
	if n := stream.overlaps.Load(); n != 0 {
		t.Errorf("stream.Send was entered concurrently %d times", n)
	}
}
