package provider

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
)

type selectingProvider struct {
	def       Provider
	model     string
	providers map[string]*selected
	ctx       context.Context
	log       *slog.Logger
}

type SelectableProvider struct {
	Prov  Provider
	Probe HealthProbe
}

type selected struct {
	prov    Provider
	health  *endpointHealth
	probing sync.Once
}

func NewSelectingProvider(ctx context.Context, def Provider, defaultModel string, providers map[string]SelectableProvider, health HealthGateConfig, log *slog.Logger) Provider {
	if log == nil {
		log = slog.Default()
	}
	sel := make(map[string]*selected, len(providers))
	for id, p := range providers {
		e := &selected{prov: p.Prov}
		if p.Probe != nil {
			e.health = newEndpointHealth("selected llm provider", p.Probe, health, log.With("provider", id))
		}
		sel[id] = e
	}
	return &selectingProvider{def: def, model: defaultModel, providers: sel, ctx: ctx, log: log}
}

func (s *selectingProvider) ID() string { return s.def.ID() }

func (s *selectingProvider) Models() []ModelInfo { return s.def.Models() }

func (s *selectingProvider) SupportsFeature(f Feature) bool { return s.def.SupportsFeature(f) }

func (s *selectingProvider) route(req *CompletionRequest) (*selected, ModelInfo, *CompletionRequest) {
	ref := string(req.ModelRef)
	if ref == "" {
		return nil, ModelInfo{}, req
	}
	def := *req
	def.Model = s.model
	if _, _, qualified := strings.Cut(ref, "/"); !qualified && findModel(s.def.Models(), ref).ID != "" {
		return nil, ModelInfo{}, req
	}
	p, m, why := s.lookup(ref)
	if why != "" {
		s.log.Warn(why+"; using the default model", "requested", ref, "model", s.model)
		return nil, ModelInfo{}, &def
	}
	if p == nil {
		return nil, ModelInfo{}, &def
	}
	if p.health != nil && !p.health.isHealthy() {
		s.log.Debug("requested model's provider is unhealthy; using the default model",
			"provider", m.ProviderID, "model", m.ID)
		return nil, ModelInfo{}, &def
	}
	return p, m, &def
}

// lookup resolves ref to a model of a configured provider other than the
// default model; why says what is wrong with a ref that names none.
func (s *selectingProvider) lookup(ref string) (p *selected, m ModelInfo, why string) {
	providerID, modelID, qualified := strings.Cut(ref, "/")
	switch {
	case !qualified:
		return nil, ModelInfo{}, "requested model is not a model of the default provider"
	case providerID == "" || modelID == "":
		return nil, ModelInfo{}, "requested model is not a <provider>/<model> reference"
	case providerID == s.def.ID() && modelID == s.model:
		return nil, ModelInfo{}, ""
	}
	p, ok := s.providers[providerID]
	if !ok {
		return nil, ModelInfo{}, "requested model names a provider the config does not define"
	}
	if m = findModel(p.prov.Models(), modelID); m.ID == "" {
		return nil, ModelInfo{}, "requested model is not a model of its provider"
	}
	return p, m, ""
}

func selectedRequest(req *CompletionRequest, m ModelInfo) *CompletionRequest {
	cp := *req
	cp.Model = m.ID
	cp.Reasoning = m.SupportsFeature(FeatureReasoning)
	return &cp
}

func (s *selectingProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	sel, m, def := s.route(req)
	if sel != nil {
		resp, err := sel.prov.Complete(ctx, selectedRequest(req, m))
		if err == nil {
			return resp, nil
		}
		if err := s.failed(ctx, sel, m, err); err != nil {
			return nil, err
		}
	}
	resp, err := s.def.Complete(ctx, def)
	if err == nil && def.Model != req.Model {
		resp.Fallback = true
	}
	return resp, err
}

// Stream returns the selected provider's error when that provider cannot
// stream, so the caller's Complete serves the request there.
func (s *selectingProvider) Stream(ctx context.Context, req *CompletionRequest) (ResponseStream, error) {
	sel, m, def := s.route(req)
	if sel != nil {
		cp := selectedRequest(req, m)
		cp.Stream = true
		stream, err := sel.prov.Stream(ctx, cp)
		if err == nil || errors.Is(err, ErrStreamingNotImplemented) {
			return stream, err
		}
		if err := s.failed(ctx, sel, m, err); err != nil {
			return nil, err
		}
	}
	stream, err := s.def.Stream(ctx, def)
	if err == nil && def.Model != req.Model {
		stream = fallbackStream{stream}
	}
	return stream, err
}

type fallbackStream struct{ ResponseStream }

func (f fallbackStream) Recv() (StreamChunk, error) {
	c, err := f.ResponseStream.Recv()
	c.Fallback = true
	return c, err
}

// EventID is the wrapped stream's, for the caller that reads it after the
// stream ends.
func (f fallbackStream) EventID() string {
	if e, ok := f.ResponseStream.(interface{ EventID() string }); ok {
		return e.EventID()
	}
	return ""
}

func (s *selectingProvider) failed(ctx context.Context, sel *selected, m ModelInfo, err error) error {
	if ctx.Err() != nil || IsContextOverflow(err) {
		return err
	}
	unhealthy := sel.health != nil && providerFailure(err)
	if unhealthy {
		sel.health.trip()
		sel.probing.Do(func() { go sel.health.run(s.ctx) })
	}
	s.log.Warn("requested model failed; answering with the default model",
		"provider", sel.prov.ID(), "model", m.ID, "unhealthy", unhealthy, "error", err)
	return nil
}

func providerFailure(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusBadRequest:
			// Anthropic answers a reached spend limit and an empty credit
			// balance with 400; the message is the way to tell them from an
			// invalid request.
			body := strings.ToLower(se.Body)
			return strings.Contains(body, "you have reached your specified") ||
				strings.Contains(body, "credit balance is too low")
		case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return false
		}
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
