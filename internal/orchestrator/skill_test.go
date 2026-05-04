package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// stubMemoryStore is a minimal in-memory store for testing.
type stubMemoryStore struct {
	mu        sync.Mutex
	memories  []*state.Memory
	nextID    int
	addErr    error
	deleteErr error
}

func (s *stubMemoryStore) AddScoped(_ context.Context, actorID string, content string, tags ...string) (*state.Memory, error) {
	if s.addErr != nil {
		return nil, s.addErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	m := &state.Memory{
		ID:      fmt.Sprintf("mem_%d", s.nextID),
		Content: content,
		Tags:    tags,
	}
	s.memories = append(s.memories, m)
	return m, nil
}

func (s *stubMemoryStore) MemoriesForContext(_ context.Context, tag string) ([]*state.Memory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*state.Memory
	for _, m := range s.memories {
		if tag == "" {
			out = append(out, m)
			continue
		}
		for _, t := range m.Tags {
			if t == tag {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

func (s *stubMemoryStore) Delete(id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, m := range s.memories {
		if m.ID == id {
			s.memories = append(s.memories[:i], s.memories[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("memory %q not found", id)
}

// stubLLM returns a fixed response.
type stubLLM struct {
	response string
	err      error
	calls    int
	mu       sync.Mutex
}

func (s *stubLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return &provider.CompletionResponse{Content: s.response}, nil
}

func TestIsSkillWorthy_MinToolCalls(t *testing.T) {
	se := &SkillExtractor{config: SkillExtractionConfig{Enabled: true, MinToolCalls: 3}}

	tests := []struct {
		name  string
		calls int
		want  bool
	}{
		{"below threshold", 2, false},
		{"at threshold", 3, true},
		{"above threshold", 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &RunResult{}
			for i := 0; i < tt.calls; i++ {
				result.ToolCalls = append(result.ToolCalls, ToolCall{Plugin: "p", Action: "a"})
				result.Results = append(result.Results, ToolResult{Content: "ok"})
			}
			if got := se.IsSkillWorthy(result); got != tt.want {
				t.Errorf("IsSkillWorthy(%d calls) = %v, want %v", tt.calls, got, tt.want)
			}
		})
	}
}

func TestIsSkillWorthy_DefaultMinToolCalls(t *testing.T) {
	se := &SkillExtractor{config: SkillExtractionConfig{Enabled: true}} // MinToolCalls = 0 → default 5

	result := &RunResult{}
	for i := 0; i < 4; i++ {
		result.ToolCalls = append(result.ToolCalls, ToolCall{Plugin: "p", Action: "a"})
		result.Results = append(result.Results, ToolResult{Content: "ok"})
	}
	if se.IsSkillWorthy(result) {
		t.Error("expected 4 calls to be below default threshold of 5")
	}

	result.ToolCalls = append(result.ToolCalls, ToolCall{Plugin: "p", Action: "a"})
	result.Results = append(result.Results, ToolResult{Content: "ok"})
	if !se.IsSkillWorthy(result) {
		t.Error("expected 5 calls to meet default threshold")
	}
}

func TestIsSkillWorthy_ErrorRecovery(t *testing.T) {
	se := &SkillExtractor{config: SkillExtractionConfig{Enabled: true, MinToolCalls: 100}} // high threshold

	result := &RunResult{
		ToolCalls: []ToolCall{
			{Plugin: "jira", Action: "create"},
			{Plugin: "jira", Action: "create"},
		},
		Results: []ToolResult{
			{Error: "permission denied"},
			{Content: "issue PROJ-123 created"},
		},
	}
	if !se.IsSkillWorthy(result) {
		t.Error("expected error recovery to trigger skill-worthy")
	}
}

func TestIsSkillWorthy_NoErrorRecovery(t *testing.T) {
	se := &SkillExtractor{config: SkillExtractionConfig{Enabled: true, MinToolCalls: 100}}

	result := &RunResult{
		ToolCalls: []ToolCall{
			{Plugin: "jira", Action: "create"},
			{Plugin: "slack", Action: "send"},
		},
		Results: []ToolResult{
			{Error: "permission denied"},
			{Content: "message sent"},
		},
	}
	if se.IsSkillWorthy(result) {
		t.Error("different plugins failing/succeeding should not be error recovery")
	}
}

func TestHasErrorRecovery(t *testing.T) {
	tests := []struct {
		name    string
		calls   []ToolCall
		results []ToolResult
		want    bool
	}{
		{
			name: "error then success same plugin",
			calls: []ToolCall{
				{Plugin: "jira", Action: "create"},
				{Plugin: "jira", Action: "create"},
			},
			results: []ToolResult{
				{Error: "timeout"},
				{Content: "done"},
			},
			want: true,
		},
		{
			name: "error then success different plugin",
			calls: []ToolCall{
				{Plugin: "jira", Action: "create"},
				{Plugin: "slack", Action: "send"},
			},
			results: []ToolResult{
				{Error: "timeout"},
				{Content: "done"},
			},
			want: false,
		},
		{
			name: "all success",
			calls: []ToolCall{
				{Plugin: "jira", Action: "create"},
				{Plugin: "jira", Action: "list"},
			},
			results: []ToolResult{
				{Content: "created"},
				{Content: "listed"},
			},
			want: false,
		},
		{
			name:    "empty",
			calls:   nil,
			results: nil,
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &RunResult{ToolCalls: tt.calls, Results: tt.results}
			if got := hasErrorRecovery(result); got != tt.want {
				t.Errorf("hasErrorRecovery() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildToolTrace(t *testing.T) {
	result := &RunResult{
		ToolCalls: []ToolCall{
			{Plugin: "slack", Action: "get_message", Args: map[string]string{"channel": "C123"}},
			{Plugin: "jira", Action: "create_issue"},
		},
		Results: []ToolResult{
			{Content: "Hello world message content here"},
			{Error: "project not found"},
		},
	}

	trace := buildToolTrace(result)
	if trace == "" {
		t.Fatal("expected non-empty trace")
	}
	// Check key elements are present.
	for _, want := range []string{"slack", "get_message", "jira", "create_issue", "project not found", "Hello world"} {
		if !contains(trace, want) {
			t.Errorf("trace missing %q", want)
		}
	}
}

func TestExtractAndStore(t *testing.T) {
	mem := &stubMemoryStore{}
	llm := &stubLLM{response: "name: test-skill\ntrigger: test\nsteps:\n  - plugin: p, action: a, order: 1\noutcome: success"}
	se := NewSkillExtractor(llm, mem, SkillExtractionConfig{Enabled: true, MinToolCalls: 2})

	ctx := actor.WithActor(context.Background(), "user1")
	result := &RunResult{
		ToolCalls: []ToolCall{{Plugin: "p", Action: "a"}, {Plugin: "p", Action: "b"}},
		Results:   []ToolResult{{Content: "ok"}, {Content: "ok"}},
	}

	se.ExtractAndStore(ctx, result, "do the thing")

	memories, _ := mem.MemoriesForContext(ctx, "skill")
	if len(memories) != 1 {
		t.Fatalf("expected 1 skill stored, got %d", len(memories))
	}
	if memories[0].Content == "" {
		t.Error("stored skill should not be empty")
	}
}

func TestExtractAndStore_NotWorthy(t *testing.T) {
	mem := &stubMemoryStore{}
	llm := &stubLLM{response: "should not be called"}
	se := NewSkillExtractor(llm, mem, SkillExtractionConfig{Enabled: true, MinToolCalls: 5})

	ctx := actor.WithActor(context.Background(), "user1")
	result := &RunResult{
		ToolCalls: []ToolCall{{Plugin: "p", Action: "a"}},
		Results:   []ToolResult{{Content: "ok"}},
	}

	se.ExtractAndStore(ctx, result, "simple thing")

	llm.mu.Lock()
	calls := llm.calls
	llm.mu.Unlock()
	if calls != 0 {
		t.Error("LLM should not have been called for a non-skill-worthy result")
	}
}

func TestImproveSkill(t *testing.T) {
	mem := &stubMemoryStore{}
	// Pre-seed a skill.
	ctx := actor.WithActor(context.Background(), "user1")
	existing, _ := mem.AddScoped(ctx, "user1", "name: old-skill\ntrigger: old\nsteps:\n  - p.a\noutcome: success", "skill")

	llm := &stubLLM{response: "name: improved-skill\ntrigger: better\nsteps:\n  - p.a\n  - p.b\noutcome: success"}
	se := NewSkillExtractor(llm, mem, SkillExtractionConfig{Enabled: true})

	result := &RunResult{
		ToolCalls: []ToolCall{{Plugin: "p", Action: "a"}, {Plugin: "p", Action: "b"}},
		Results:   []ToolResult{{Content: "ok"}, {Content: "ok"}},
	}

	se.ImproveSkill(ctx, existing.ID, existing.Content, result, "do better thing")

	memories, _ := mem.MemoriesForContext(ctx, "skill")
	if len(memories) != 1 {
		t.Fatalf("expected 1 skill (improved), got %d", len(memories))
	}
	if memories[0].Content != llm.response {
		t.Errorf("expected improved skill content, got %q", memories[0].Content)
	}
}

func TestFindMatchingSkills(t *testing.T) {
	mem := &stubMemoryStore{}
	ctx := actor.WithActor(context.Background(), "user1")
	_, _ = mem.AddScoped(ctx, "user1", "name: skill-1\ntrigger: test", "skill")
	_, _ = mem.AddScoped(ctx, "user1", "name: skill-2\ntrigger: other", "skill")
	_, _ = mem.AddScoped(ctx, "user1", "not a skill", "workflow") // different tag

	se := NewSkillExtractor(&stubLLM{}, mem, SkillExtractionConfig{Enabled: true})
	skills := se.FindMatchingSkills(ctx)
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}
}

func TestMaybePrune(t *testing.T) {
	mem := &stubMemoryStore{}
	ctx := actor.WithActor(context.Background(), "user1")

	se := NewSkillExtractor(&stubLLM{}, mem, SkillExtractionConfig{Enabled: true, MaxSkills: 2})

	// Add 2 skills (at capacity).
	_, _ = mem.AddScoped(ctx, "user1", "skill-oldest", "skill")
	_, _ = mem.AddScoped(ctx, "user1", "skill-newer", "skill")

	se.maybePrune(ctx)

	memories, _ := mem.MemoriesForContext(ctx, "skill")
	if len(memories) != 1 {
		t.Fatalf("expected 1 skill after prune, got %d", len(memories))
	}
	// maybePrune removes the last element (oldest in DESC-sorted DB results).
	// In the stub, memories are insertion-order, so the last is "skill-newer".
	// The real DB returns DESC by created_at, so the last is the oldest.
	// Either way, one skill is removed and one remains.
}

func TestNewSkillExtractor_Disabled(t *testing.T) {
	se := NewSkillExtractor(nil, nil, SkillExtractionConfig{Enabled: false})
	if se != nil {
		t.Error("expected nil when disabled")
	}
}

func TestMatchedSkillsContext(t *testing.T) {
	ctx := context.Background()

	// No skills in context.
	if skills := matchedSkillsFromContext(ctx); skills != nil {
		t.Error("expected nil from empty context")
	}

	// With skills.
	s := []*matchedSkill{{memoryID: "m1", content: "skill1"}}
	ctx = withMatchedSkills(ctx, s)
	got := matchedSkillsFromContext(ctx)
	if len(got) != 1 || got[0].memoryID != "m1" {
		t.Error("expected skill from context")
	}
}

func TestShouldNudgeSkillExtraction(t *testing.T) {
	if shouldNudgeSkillExtraction(0) {
		t.Error("round 0 should not nudge")
	}
	if !shouldNudgeSkillExtraction(10) {
		t.Error("round 10 should nudge")
	}
	if shouldNudgeSkillExtraction(7) {
		t.Error("round 7 should not nudge")
	}
	if !shouldNudgeSkillExtraction(20) {
		t.Error("round 20 should nudge")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstring(s, sub))
}

func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
