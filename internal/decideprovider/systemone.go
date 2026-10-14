package decideprovider

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

// Backends that speak the hosted System-1 decision API.
const (
	// BackendJev selects Jev (typesafe.ai).
	BackendJev = "jev"
	// BackendSubQ selects SubQ Snap (subq.ai).
	BackendSubQ = "subq"
)

const (
	defaultJevModel  = "jev-latest"
	defaultSubQModel = "subq-snap-preview"
)

const systemOneQuestion = "decision"

// SystemOneProvider talks to a hosted System-1 decision API. Jev (typesafe.ai,
// https://docs.typesafe.ai/introduction/quickstart) and SubQ Snap (subq.ai,
// https://docs.subq.ai/endpoints/decisions-api/) share the wire shape; only the
// endpoint and the default model differ. Each decision is one "choice" question
// whose criteria are the declared choices:
//
//	POST <base_url>            (Authorization: Bearer <api_key>)
//	  { "state": "<state>", "model": "jev-latest",
//	    "questions": { "decision": { "type": "choice",
//	                                 "criteria": { "A": "A", "B": "B" } } } }
//	-> { "model": "jev-1.13.0",
//	     "answers": { "decision": { "type": "choice", "choice": "A",
//	                                "confidence": 0.92,
//	                                "probabilities": { "A": 0.92, "B": 0.08 } } },
//	     "usage": { "input_tokens": 12, "output_tokens": 1 } }
//
// base_url is the full endpoint, e.g. https://api.typesafe.ai/v1/systemone or
// https://api.subq.ai/v1/systemone.
// The response is renormalized over the declared choices locally, so a hosted
// answer that omits a choice, or whose probabilities don't sum to 1, is made
// consistent before it reaches the executor. `usage` is passed through when
// present; otherwise input is estimated.
type SystemOneProvider struct {
	backend string // error prefix: "jev" or "subq"
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewJevProvider builds a Jev backend bound to name, posting to baseURL with
// apiKey as a bearer token and asking model (jev-latest when empty).
func NewJevProvider(name, baseURL, apiKey, model string) *SystemOneProvider {
	return newSystemOneProvider(BackendJev, defaultJevModel, name, baseURL, apiKey, model)
}

// NewSubQProvider builds a SubQ Snap backend bound to name, posting to baseURL
// with apiKey as a bearer token and asking model (subq-snap-preview when empty).
func NewSubQProvider(name, baseURL, apiKey, model string) *SystemOneProvider {
	return newSystemOneProvider(BackendSubQ, defaultSubQModel, name, baseURL, apiKey, model)
}

func newSystemOneProvider(backend, defaultModel, name, baseURL, apiKey, model string) *SystemOneProvider {
	if model == "" {
		model = defaultModel
	}
	return &SystemOneProvider{backend: backend, id: name, baseURL: baseURL, apiKey: apiKey, model: model, client: newHTTPClient()}
}

func (p *SystemOneProvider) ID() string { return p.id }

type systemOneChoiceQuestion struct {
	Type     string            `json:"type"`
	Criteria map[string]string `json:"criteria"`
}

type systemOneRequest struct {
	State     string                             `json:"state"`
	Model     string                             `json:"model"`
	Questions map[string]systemOneChoiceQuestion `json:"questions"`
}

type systemOneAnswer struct {
	Type   string `json:"type"`
	Choice string `json:"choice"`
	// Confidence is part of the wire shape but intentionally NOT trusted:
	// Decision.Confidence is recomputed from the renormalized distribution in
	// finalize (calibration is honored host-side, not taken on faith).
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *SystemOneProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if p.baseURL == "" {
		return nil, fmt.Errorf("%s %q: base_url is required", p.backend, p.id)
	}
	if len(req.Choices) == 0 {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, errNoChoices)
	}
	criteria := make(map[string]string, len(req.Choices))
	for _, c := range req.Choices {
		criteria[c] = c
	}
	body := systemOneRequest{
		State:     req.State,
		Model:     p.model,
		Questions: map[string]systemOneChoiceQuestion{systemOneQuestion: {Type: "choice", Criteria: criteria}},
	}
	var resp systemOneResponse
	if err := postJSON(ctx, p.client, p.baseURL, p.apiKey, body, &resp); err != nil {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, err)
	}
	ans, ok := resp.Answers[systemOneQuestion]
	if !ok {
		return nil, fmt.Errorf("%s %q: response has no answer for question %q", p.backend, p.id, systemOneQuestion)
	}
	if ans.Type != "choice" {
		return nil, fmt.Errorf("%s %q: answer %q has type %q, want choice", p.backend, p.id, systemOneQuestion, ans.Type)
	}
	if ans.Choice != "" && !slices.Contains(req.Choices, ans.Choice) {
		return nil, fmt.Errorf("%s %q: answer chose %q, which is not one of %v", p.backend, p.id, ans.Choice, req.Choices)
	}
	probs := ans.Probabilities
	if len(probs) == 0 {
		if ans.Choice == "" {
			return nil, fmt.Errorf("%s %q: answer has neither probabilities nor choice", p.backend, p.id)
		}
		probs = map[string]float64{ans.Choice: 1}
	}
	var mass, top float64
	for _, c := range req.Choices {
		if v := probs[c]; v > 0 {
			mass += v
			top = max(top, v)
		}
	}
	if mass == 0 {
		return nil, fmt.Errorf("%s %q: answer puts no probability mass on any of %v: %v", p.backend, p.id, req.Choices, probs)
	}
	if ans.Choice != "" && probs[ans.Choice] < top {
		return nil, fmt.Errorf("%s %q: answer chose %q, which disagrees with its probabilities %v", p.backend, p.id, ans.Choice, probs)
	}
	dec, err := normalizeProbabilities(probs, req.Choices)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, err)
	}
	if resp.Usage != nil {
		dec.Usage = Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	} else {
		dec.Usage = Usage{InputTokens: approxTokens(req.State), OutputTokens: 1}
	}
	return dec, nil
}

var _ Provider = (*SystemOneProvider)(nil)
