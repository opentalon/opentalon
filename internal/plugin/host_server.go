package plugin

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// HostServer implements pluginpb.HostServiceServer, giving plugins access to
// the host's memory store and LLM provider.
type HostServer struct {
	pluginpb.UnimplementedHostServiceServer
	memory orchestrator.MemoryStoreInterface
	llm    orchestrator.LLMClient
	addr   string // address the server is listening on
}

// NewHostServer creates a host service backed by the given memory store and LLM.
func NewHostServer(memory orchestrator.MemoryStoreInterface, llm orchestrator.LLMClient) *HostServer {
	return &HostServer{memory: memory, llm: llm}
}

// Start starts the host service on a random localhost TCP port. Returns the
// address (e.g. "127.0.0.1:54321") that should be passed to plugins as "__host_addr".
func (s *HostServer) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("host service: listen: %w", err)
	}
	s.addr = ln.Addr().String()
	srv := grpc.NewServer()
	pluginpb.RegisterHostServiceServer(srv, s)
	go func() { _ = srv.Serve(ln) }()
	return s.addr, nil
}

// Addr returns the address the server is listening on. Empty before Start.
func (s *HostServer) Addr() string { return s.addr }

func (s *HostServer) WriteMemory(_ context.Context, req *pluginpb.WriteMemoryRequest) (*pluginpb.WriteMemoryResponse, error) {
	mem, err := s.memory.AddScoped(context.Background(), req.ActorId, req.Content, req.Tags...)
	if err != nil {
		return nil, fmt.Errorf("write memory: %w", err)
	}
	return &pluginpb.WriteMemoryResponse{Id: mem.ID}, nil
}

func (s *HostServer) ReadMemories(ctx context.Context, req *pluginpb.ReadMemoriesRequest) (*pluginpb.ReadMemoriesResponse, error) {
	if req.ActorId != "" {
		ctx = actor.WithActor(ctx, req.ActorId)
	}
	memories, err := s.memory.MemoriesForContext(ctx, req.Tag)
	if err != nil {
		return nil, fmt.Errorf("read memories: %w", err)
	}
	entries := make([]*pluginpb.MemoryEntry, len(memories))
	for i, m := range memories {
		entries[i] = memoryToProto(m)
	}
	return &pluginpb.ReadMemoriesResponse{Memories: entries}, nil
}

func (s *HostServer) DeleteMemory(_ context.Context, req *pluginpb.DeleteMemoryRequest) (*emptypb.Empty, error) {
	if err := s.memory.Delete(req.Id); err != nil {
		return nil, fmt.Errorf("delete memory: %w", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *HostServer) LLMComplete(ctx context.Context, req *pluginpb.LLMCompleteRequest) (*pluginpb.LLMCompleteResponse, error) {
	maxTokens := int(req.MaxTokens)
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	resp, err := s.llm.Complete(ctx, &provider.CompletionRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: req.SystemPrompt},
			{Role: provider.RoleUser, Content: req.UserMessage},
		},
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("llm complete: %w", err)
	}
	return &pluginpb.LLMCompleteResponse{
		Content:      resp.Content,
		InputTokens:  int32(resp.Usage.InputTokens),
		OutputTokens: int32(resp.Usage.OutputTokens),
	}, nil
}

func memoryToProto(m *state.Memory) *pluginpb.MemoryEntry {
	return &pluginpb.MemoryEntry{
		Id:        m.ID,
		Content:   m.Content,
		Tags:      m.Tags,
		CreatedAt: m.CreatedAt.Format(time.RFC3339),
	}
}
