package main

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/provider"
)

// fakeProvider implements provider.Provider for testing.
type fakeProvider struct {
	models    []provider.ModelInfo
	lastReq   *provider.CompletionRequest
	streamReq *provider.CompletionRequest
}

func (f *fakeProvider) ID() string                   { return "fake" }
func (f *fakeProvider) Models() []provider.ModelInfo { return f.models }
func (f *fakeProvider) SupportsFeature(feat provider.Feature) bool {
	for _, m := range f.models {
		if m.SupportsFeature(feat) {
			return true
		}
	}
	return false
}
func (f *fakeProvider) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	f.lastReq = req
	return &provider.CompletionResponse{Content: "ok"}, nil
}
func (f *fakeProvider) Stream(_ context.Context, req *provider.CompletionRequest) (provider.ResponseStream, error) {
	f.streamReq = req
	return nil, nil
}

func modelMap(models []provider.ModelInfo) map[string]provider.ModelInfo {
	m := make(map[string]provider.ModelInfo, len(models))
	for _, mi := range models {
		m[mi.ID] = mi
	}
	return m
}

func TestDefaultModelClient_SupportsFeature_Reasoning(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{
			{ID: "gpt-oss-120b", Features: []provider.Feature{provider.FeatureStreaming, provider.FeatureReasoning}},
		},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	if !c.SupportsFeature(provider.FeatureReasoning) {
		t.Error("expected SupportsFeature(FeatureReasoning) = true")
	}
	if !c.SupportsFeature(provider.FeatureStreaming) {
		t.Error("expected SupportsFeature(FeatureStreaming) = true")
	}
	if c.SupportsFeature(provider.FeatureImages) {
		t.Error("expected SupportsFeature(FeatureImages) = false")
	}
}

func TestDefaultModelClient_SupportsFeature_NoReasoning(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{
			{ID: "some-model", Features: []provider.Feature{provider.FeatureStreaming}},
		},
	}
	c := &defaultModelClient{provider: fp, model: "some-model", models: modelMap(fp.models)}

	if c.SupportsFeature(provider.FeatureReasoning) {
		t.Error("expected SupportsFeature(FeatureReasoning) = false")
	}
}

func TestDefaultModelClient_Complete_PreservesReasoningFields(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Reasoning:       true,
		ReasoningEffort: "high",
		BudgetTokens:    5000,
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq == nil {
		t.Fatal("expected provider to receive request")
	}
	if fp.lastReq.Model != "gpt-oss-120b" {
		t.Errorf("expected model gpt-oss-120b, got %s", fp.lastReq.Model)
	}
	if !fp.lastReq.Reasoning {
		t.Error("expected Reasoning=true to be preserved")
	}
	if fp.lastReq.ReasoningEffort != "high" {
		t.Errorf("expected ReasoningEffort=high, got %s", fp.lastReq.ReasoningEffort)
	}
	if fp.lastReq.BudgetTokens != 5000 {
		t.Errorf("expected BudgetTokens=5000, got %d", fp.lastReq.BudgetTokens)
	}
}

func TestDefaultModelClient_Stream_PreservesReasoningFields(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Reasoning:       true,
		ReasoningEffort: "medium",
	}
	_, _ = c.Stream(context.Background(), req)

	if fp.streamReq == nil {
		t.Fatal("expected provider to receive stream request")
	}
	if !fp.streamReq.Reasoning {
		t.Error("expected Reasoning=true to be preserved in stream")
	}
	if fp.streamReq.ReasoningEffort != "medium" {
		t.Errorf("expected ReasoningEffort=medium, got %s", fp.streamReq.ReasoningEffort)
	}
}

func TestDefaultModelClient_ApplyModelDefaults_MaxTokens(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", MaxTokens: 32768}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq.MaxTokens != 32768 {
		t.Errorf("expected MaxTokens=32768 from model config, got %d", fp.lastReq.MaxTokens)
	}
}

func TestDefaultModelClient_ApplyModelDefaults_MaxTokensNotOverridden(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", MaxTokens: 32768}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		MaxTokens: 1024,
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq.MaxTokens != 1024 {
		t.Errorf("expected MaxTokens=1024 (explicit), got %d", fp.lastReq.MaxTokens)
	}
}

func TestDefaultModelClient_ApplyModelDefaults_ReasoningEffort(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", ReasoningEffort: "high"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq.ReasoningEffort != "high" {
		t.Errorf("expected ReasoningEffort=high from model config, got %s", fp.lastReq.ReasoningEffort)
	}
}

func TestDefaultModelClient_ApplyModelDefaults_ReasoningEffortNotOverridden(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", ReasoningEffort: "high"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages:        []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		ReasoningEffort: "low",
	}
	_, _ = c.Complete(context.Background(), req)

	if fp.lastReq.ReasoningEffort != "low" {
		t.Errorf("expected ReasoningEffort=low (explicit), got %s", fp.lastReq.ReasoningEffort)
	}
}

func TestDefaultModelClient_ApplyModelDefaults_StreamInjects(t *testing.T) {
	fp := &fakeProvider{
		models: []provider.ModelInfo{{ID: "gpt-oss-120b", MaxTokens: 65536, ReasoningEffort: "high"}},
	}
	c := &defaultModelClient{provider: fp, model: "gpt-oss-120b", models: modelMap(fp.models)}

	req := &provider.CompletionRequest{
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	}
	_, _ = c.Stream(context.Background(), req)

	if fp.streamReq.MaxTokens != 65536 {
		t.Errorf("expected MaxTokens=65536 from model config in stream, got %d", fp.streamReq.MaxTokens)
	}
	if fp.streamReq.ReasoningEffort != "high" {
		t.Errorf("expected ReasoningEffort=high from model config in stream, got %s", fp.streamReq.ReasoningEffort)
	}
}
