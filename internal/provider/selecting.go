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

// selectingProvider serves a request whose ModelRef names a provider and a
// model the config defines at that provider, with the default provider (the
// health gate, or the single routing provider) behind it. Every other
// request goes to the default provider, and so does a request the selected
// provider fails or would get while it is unhealthy.
type selectingProvider struct {
	def       Provider
	model     string               // the default model id
	providers map[string]*selected // every provider the config defines, by id
	ctx       context.Context      // ends the probes
	log       *slog.Logger
}

// SelectableProvider is a provider the config defines, and the probe that
// tells when it serves again after a failure that marked it unhealthy. A
// provider without a probe is never marked: each failed request falls back
// on its own.
type SelectableProvider struct {
	Prov  Provider
	Probe HealthProbe
}

type selected struct {
	prov    Provider
	health  *endpointHealth // nil without a probe
	probing sync.Once
}

// NewSelectingProvider puts the providers in front of def, whose model is
// defaultModel. ID, Models and SupportsFeature are def's. A provider's probes
// start when a failure first marks it unhealthy, run as health sets them and
// stop when ctx is done.
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

// route resolves req.ModelRef. A reference to a model of a defined provider
// returns that provider and model. Every other request goes to the default
// provider: unchanged without a reference or with a bare id of a model the
// default provider lists, with the default model for a reference to that
// model, and with the default model and a warning for anything else.
func (s *selectingProvider) route(req *CompletionRequest) (*selected, ModelInfo, *CompletionRequest) {
	ref := string(req.ModelRef)
	if ref == "" {
		return nil, ModelInfo{}, req
	}
	def := *req
	def.Model = s.model
	providerID, modelID, qualified := strings.Cut(ref, "/")
	if !qualified {
		if findModel(s.def.Models(), ref).ID != "" {
			return nil, ModelInfo{}, req
		}
		s.log.Warn("requested model is not a model of the default provider; using the default model",
			"requested", ref, "model", s.model)
		return nil, ModelInfo{}, &def
	}
	if providerID == "" || modelID == "" {
		s.log.Warn("requested model is not a <provider>/<model> reference; using the default model",
			"requested", ref, "model", s.model)
		return nil, ModelInfo{}, &def
	}
	if providerID == s.def.ID() && modelID == s.model {
		return nil, ModelInfo{}, &def
	}
	p, ok := s.providers[providerID]
	if !ok {
		s.log.Warn("requested model names a provider the config does not define; using the default model",
			"requested", ref, "model", s.model)
		return nil, ModelInfo{}, &def
	}
	m := findModel(p.prov.Models(), modelID)
	if m.ID == "" {
		s.log.Warn("requested model is not a model of its provider; using the default model",
			"requested", ref, "model", s.model)
		return nil, ModelInfo{}, &def
	}
	if p.health != nil && !p.health.isHealthy() {
		s.log.Debug("requested model's provider is unhealthy; using the default model",
			"provider", providerID, "model", modelID)
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

// fallbackStream marks every chunk of the default model's stream for a
// request for another model.
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

// failed handles err of the selected provider for model m. It returns err
// when the default model cannot do better: the turn was cancelled, or the
// prompt is longer than the window. Otherwise it marks the provider unhealthy
// when err is about the provider rather than this request, warns once and
// returns nil for the default model to answer.
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

// providerFailure reports whether err says the provider serves no request
// right now: it refuses the key, the account, the spent usage limit or the
// load, cannot be reached, or took longer than its time limit. A 400, 413 or
// 422 without a usage limit refuses this request only.
func providerFailure(err error) bool {
	var se *StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusBadRequest:
			// Anthropic answers a reached spend limit with 400; the message
			// is the documented way to tell it from an invalid request.
			return strings.Contains(strings.ToLower(se.Body), "you have reached your specified")
		case http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return false
		}
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
