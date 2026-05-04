package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// HostClient connects to the host's HostService gRPC server, giving plugins
// access to the shared memory store and LLM provider.
type HostClient struct {
	conn   *grpc.ClientConn
	client pluginpb.HostServiceClient
}

// DialHost connects to the host service at the given address (from __host_addr config).
func DialHost(addr string) (*HostClient, error) {
	cc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial host service at %s: %w", addr, err)
	}
	return &HostClient{conn: cc, client: pluginpb.NewHostServiceClient(cc)}, nil
}

// WriteMemory stores a memory in the host's memory store.
func (h *HostClient) WriteMemory(ctx context.Context, actorID, content string, tags ...string) (string, error) {
	resp, err := h.client.WriteMemory(ctx, &pluginpb.WriteMemoryRequest{
		ActorId: actorID,
		Content: content,
		Tags:    tags,
	})
	if err != nil {
		return "", fmt.Errorf("write memory: %w", err)
	}
	return resp.Id, nil
}

// MemoryEntry is a memory returned by ReadMemories.
type MemoryEntry struct {
	ID        string
	Content   string
	Tags      []string
	CreatedAt string
}

// ReadMemories retrieves memories from the host's memory store.
func (h *HostClient) ReadMemories(ctx context.Context, actorID, tag string) ([]MemoryEntry, error) {
	resp, err := h.client.ReadMemories(ctx, &pluginpb.ReadMemoriesRequest{
		ActorId: actorID,
		Tag:     tag,
	})
	if err != nil {
		return nil, fmt.Errorf("read memories: %w", err)
	}
	out := make([]MemoryEntry, len(resp.Memories))
	for i, m := range resp.Memories {
		out[i] = MemoryEntry{
			ID:        m.Id,
			Content:   m.Content,
			Tags:      m.Tags,
			CreatedAt: m.CreatedAt,
		}
	}
	return out, nil
}

// DeleteMemory removes a memory by ID.
func (h *HostClient) DeleteMemory(ctx context.Context, id string) error {
	_, err := h.client.DeleteMemory(ctx, &pluginpb.DeleteMemoryRequest{Id: id})
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	return nil
}

// LLMCompleteResult holds the response from an LLM completion.
type LLMCompleteResult struct {
	Content      string
	InputTokens  int
	OutputTokens int
}

// LLMComplete sends a completion request to the host's LLM provider.
func (h *HostClient) LLMComplete(ctx context.Context, systemPrompt, userMessage string, maxTokens int) (*LLMCompleteResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := h.client.LLMComplete(ctx, &pluginpb.LLMCompleteRequest{
		SystemPrompt: systemPrompt,
		UserMessage:  userMessage,
		MaxTokens:    int32(maxTokens),
	})
	if err != nil {
		return nil, fmt.Errorf("llm complete: %w", err)
	}
	return &LLMCompleteResult{
		Content:      resp.Content,
		InputTokens:  int(resp.InputTokens),
		OutputTokens: int(resp.OutputTokens),
	}, nil
}

// Close terminates the connection.
func (h *HostClient) Close() error {
	return h.conn.Close()
}
