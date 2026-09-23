package decideprovider

import (
	"context"
	"fmt"
	"net/http"
)

// BackendJev is the config `backend` value selecting the Jev backend.
const BackendJev = "jev"

// JevProvider talks to the Jev hosted System-1 decision API (typesafe.ai). The
// request carries the state and declared choices; the response is expected in
// the canonical decision shape:
//
//	POST <base_url>            (Authorization: Bearer <api_key>)
//	  { "state": "<state>", "choices": ["A","B",...] }
//	-> { "chosen": "A", "confidence": 0.92,
//	     "probabilities": { "A": 0.92, "B": 0.08 },
//	     "usage": { "input_tokens": 12, "output_tokens": 1 } }
//
// The response is still renormalized over the declared choices locally, so a
// hosted answer that omits a choice, or whose probabilities don't sum to 1, is
// made consistent before it reaches the executor. `usage` is passed through when
// present; otherwise input is estimated.
type JevProvider struct {
	id      string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewJevProvider builds a Jev backend bound to name, posting to baseURL with
// apiKey as a bearer token.
func NewJevProvider(name, baseURL, apiKey string) *JevProvider {
	return &JevProvider{id: name, baseURL: baseURL, apiKey: apiKey, client: newHTTPClient()}
}

func (p *JevProvider) ID() string { return p.id }

type jevRequest struct {
	State   string   `json:"state"`
	Choices []string `json:"choices"`
}

type jevResponse struct {
	Chosen        string             `json:"chosen"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Usage         *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *JevProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if p.baseURL == "" {
		return nil, fmt.Errorf("jev %q: base_url is required", p.id)
	}
	var resp jevResponse
	if err := postJSON(ctx, p.client, p.baseURL, p.apiKey,
		jevRequest{State: req.State, Choices: req.Choices}, &resp); err != nil {
		return nil, fmt.Errorf("jev %q: %w", p.id, err)
	}
	if len(resp.Probabilities) == 0 {
		// A hosted answer with only a label still yields a valid distribution:
		// give the chosen label all mass and let normalization spread the rest.
		if resp.Chosen == "" {
			return nil, fmt.Errorf("jev %q: response has neither probabilities nor chosen", p.id)
		}
		resp.Probabilities = map[string]float64{resp.Chosen: 1}
	}
	dec, err := normalizeProbabilities(resp.Probabilities, req.Choices)
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
