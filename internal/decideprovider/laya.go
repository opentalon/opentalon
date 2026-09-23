package decideprovider

import (
	"context"
	"fmt"
	"net/http"
)

// BackendLaya is the config `backend` value selecting the laya backend.
const BackendLaya = "laya"

// LayaProvider talks to a laya typed-decisions inference server
// (convaiinnovations/laya-typed-decisions — an open-weights ModernBERT model).
// laya has no single canonical HTTP API, so this targets a small documented
// contract the serving side implements:
//
//	POST <base_url>
//	  { "text": "<state>", "choices": ["A","B",...] }
//	-> { "probabilities": { "A": 0.9, "B": 0.1, ... } }   # preferred
//	   or { "scores": { "A": 4.2, ... } }                 # raw logits
//
// Either shape is accepted: "probabilities" is renormalized over the declared
// choices; "scores" is softmaxed. laya is a classifier forward pass, so usage is
// input-only (OutputTokens == 0).
type LayaProvider struct {
	id      string
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewLayaProvider builds a laya backend bound to name, posting to baseURL.
func NewLayaProvider(name, baseURL, apiKey string) *LayaProvider {
	return &LayaProvider{id: name, baseURL: baseURL, apiKey: apiKey, client: newHTTPClient()}
}

func (p *LayaProvider) ID() string { return p.id }

type layaRequest struct {
	Text    string   `json:"text"`
	Choices []string `json:"choices"`
}

type layaResponse struct {
	Probabilities map[string]float64 `json:"probabilities"`
	Scores        map[string]float64 `json:"scores"`
}

func (p *LayaProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	var resp layaResponse
	if err := postJSON(ctx, p.client, p.baseURL, p.apiKey,
		layaRequest{Text: req.State, Choices: req.Choices}, &resp); err != nil {
		return nil, fmt.Errorf("laya %q: %w", p.id, err)
	}
	var dec *Decision
	var err error
	switch {
	case len(resp.Probabilities) > 0:
		dec, err = normalizeProbabilities(resp.Probabilities, req.Choices)
	case len(resp.Scores) > 0:
		dec, err = softmaxLogits(resp.Scores, req.Choices)
	default:
		return nil, fmt.Errorf("laya %q: response has neither probabilities nor scores", p.id)
	}
	if err != nil {
		return nil, fmt.Errorf("laya %q: %w", p.id, err)
	}
	dec.Usage = Usage{InputTokens: approxTokens(req.State)}
	return dec, nil
}

var _ Provider = (*LayaProvider)(nil)
