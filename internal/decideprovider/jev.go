package decideprovider

import (
	"context"
	"fmt"
	"net/http"
	"slices"
)

// BackendJev is the config `backend` value selecting the Jev backend.
const BackendJev = "jev"

const defaultJevModel = "jev-latest"

const jevQuestion = "decision"

// JevProvider talks to the Jev hosted System-1 decision API (typesafe.ai,
// https://docs.typesafe.ai/introduction/quickstart). Each decision is one
// "choice" question whose criteria are the declared choices:
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
// base_url is the full endpoint, e.g. https://api.typesafe.ai/v1/systemone.
// The response is renormalized over the declared choices locally, so a hosted
// answer that omits a choice, or whose probabilities don't sum to 1, is made
// consistent before it reaches the executor. `usage` is passed through when
// present; otherwise input is estimated.
type JevProvider struct {
	id      string
	baseURL string
	apiKey  string
	model   string
	client  *http.Client
}

// NewJevProvider builds a Jev backend bound to name, posting to baseURL with
// apiKey as a bearer token and asking model (jev-latest when empty).
func NewJevProvider(name, baseURL, apiKey, model string) *JevProvider {
	if model == "" {
		model = defaultJevModel
	}
	return &JevProvider{id: name, baseURL: baseURL, apiKey: apiKey, model: model, client: newHTTPClient()}
}

func (p *JevProvider) ID() string { return p.id }

type jevChoiceQuestion struct {
	Type     string            `json:"type"`
	Criteria map[string]string `json:"criteria"`
}

type jevRequest struct {
	State     string                       `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]jevChoiceQuestion `json:"questions"`
}

type jevAnswer struct {
	Type   string `json:"type"`
	Choice string `json:"choice"`
	// Confidence is part of Jev's wire shape but intentionally NOT trusted:
	// Decision.Confidence is recomputed from the renormalized distribution in
	// finalize (calibration is honored host-side, not taken on faith).
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *JevProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if p.baseURL == "" {
		return nil, fmt.Errorf("jev %q: base_url is required", p.id)
	}
	if len(req.Choices) == 0 {
		return nil, fmt.Errorf("jev %q: %w", p.id, errNoChoices)
	}
	criteria := make(map[string]string, len(req.Choices))
	for _, c := range req.Choices {
		criteria[c] = c
	}
	body := jevRequest{
		State:     req.State,
		Model:     p.model,
		Questions: map[string]jevChoiceQuestion{jevQuestion: {Type: "choice", Criteria: criteria}},
	}
	var resp jevResponse
	if err := postJSON(ctx, p.client, p.baseURL, p.apiKey, body, &resp); err != nil {
		return nil, fmt.Errorf("jev %q: %w", p.id, err)
	}
	ans, ok := resp.Answers[jevQuestion]
	if !ok {
		return nil, fmt.Errorf("jev %q: response has no answer for question %q", p.id, jevQuestion)
	}
	if ans.Type != "choice" {
		return nil, fmt.Errorf("jev %q: answer %q has type %q, want choice", p.id, jevQuestion, ans.Type)
	}
	if ans.Choice != "" && !slices.Contains(req.Choices, ans.Choice) {
		return nil, fmt.Errorf("jev %q: answer chose %q, which is not one of %v", p.id, ans.Choice, req.Choices)
	}
	probs := ans.Probabilities
	if len(probs) == 0 {
		if ans.Choice == "" {
			return nil, fmt.Errorf("jev %q: answer has neither probabilities nor choice", p.id)
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
		return nil, fmt.Errorf("jev %q: answer puts no probability mass on any of %v: %v", p.id, req.Choices, probs)
	}
	if ans.Choice != "" && probs[ans.Choice] < top {
		return nil, fmt.Errorf("jev %q: answer chose %q, which disagrees with its probabilities %v", p.id, ans.Choice, probs)
	}
	dec, err := normalizeProbabilities(probs, req.Choices)
	if err != nil {
		return nil, fmt.Errorf("jev %q: %w", p.id, err)
	}
	if resp.Usage != nil {
		dec.Usage = Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	} else {
		dec.Usage = Usage{InputTokens: approxTokens(req.State), OutputTokens: 1}
	}
	return dec, nil
}

var _ Provider = (*JevProvider)(nil)
