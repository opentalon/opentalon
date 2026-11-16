package provider

import (
	"context"
	"errors"
	"log/slog"
)

// selectingProvider serves a request whose ModelRef names a provider and a
// model the config defines at that provider, with the default provider (the
// health gate, or the single routing provider) behind it. Every other
// request goes to the default provider.
type selectingProvider struct {
	def       Provider
	model     string              // the default model id
	providers map[string]Provider // every provider the config defines, by id
	log       *slog.Logger
}

// NewSelectingProvider puts the providers in front of def, whose model is
// defaultModel. ID, Models and SupportsFeature are def's.
func NewSelectingProvider(def Provider, defaultModel string, providers map[string]Provider, log *slog.Logger) Provider {
	if log == nil {
		log = slog.Default()
	}
	return &selectingProvider{def: def, model: defaultModel, providers: providers, log: log}
}

func (s *selectingProvider) ID() string { return s.def.ID() }

func (s *selectingProvider) Models() []ModelInfo { return s.def.Models() }

func (s *selectingProvider) SupportsFeature(f Feature) bool { return s.def.SupportsFeature(f) }

// route resolves req.ModelRef. A reference to a model of a defined provider
// returns that provider and model; one that names no defined provider or
// model returns none and logs a warning. The request for the default
// provider carries the default model whenever the reference has a provider
// part, and is req itself otherwise.
func (s *selectingProvider) route(req *CompletionRequest) (Provider, ModelInfo, *CompletionRequest) {
	providerID, modelID := req.ModelRef.Provider(), req.ModelRef.Model()
	if providerID == "" {
		return nil, ModelInfo{}, req
	}
	def := *req
	def.Model = s.model
	if providerID == s.def.ID() && modelID == s.model {
		return nil, ModelInfo{}, &def
	}
	p, ok := s.providers[providerID]
	if !ok {
		s.log.Warn("requested model names a provider the config does not define; using the default model",
			"requested", string(req.ModelRef), "model", s.model)
		return nil, ModelInfo{}, &def
	}
	m := findModel(p.Models(), modelID)
	if m.ID == "" {
		s.log.Warn("requested model is not a model of its provider; using the default model",
			"requested", string(req.ModelRef), "model", s.model)
		return nil, ModelInfo{}, &def
	}
	return p, m, &def
}

// selectedRequest is req for model m: its id, and reasoning as m has it
// rather than as the default model's flag set it.
func selectedRequest(req *CompletionRequest, m ModelInfo) *CompletionRequest {
	cp := *req
	cp.Model = m.ID
	cp.Reasoning = m.SupportsFeature(FeatureReasoning)
	return &cp
}

func (s *selectingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	sel, m, def := s.route(req)
	if sel == nil {
		return s.def.Complete(ctx, def)
	}
	resp, err := sel.Complete(ctx, selectedRequest(req, m))
	if err == nil {
		return resp, nil
	}
	s.log.Warn("requested model failed; answering with the default model",
		"provider", sel.ID(), "model", m.ID, "error", err)
	return s.def.Complete(ctx, def)
}

// Stream returns the selected provider's error when that provider cannot
// stream, so the caller's Complete serves the request there.
func (s *selectingProvider) Stream(ctx context.Context, req *CompletionRequest) (ResponseStream, error) {
	sel, m, def := s.route(req)
	if sel == nil {
		return s.def.Stream(ctx, def)
	}
	cp := selectedRequest(req, m)
	cp.Stream = true
	stream, err := sel.Stream(ctx, cp)
	if err == nil || errors.Is(err, ErrStreamingNotImplemented) {
		return stream, err
	}
	s.log.Warn("requested model failed to stream; answering with the default model",
		"provider", sel.ID(), "model", m.ID, "error", err)
	return s.def.Stream(ctx, def)
}
