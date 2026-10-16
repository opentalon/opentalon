package decideprovider

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// subQMaxQuestions is the most questions SubQ Snap answers in one request; a
// larger MultiRequest is split into requests of this size. Jev has no such cap
// below MaxQuestions.
const subQMaxQuestions = 16

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
	// maxQuestions caps the questions per request (0 = MaxQuestions).
	maxQuestions int
}

// NewJevProvider builds a Jev backend bound to name, posting to baseURL with
// apiKey as a bearer token and asking model (jev-latest when empty).
func NewJevProvider(name, baseURL, apiKey, model string) *SystemOneProvider {
	return newSystemOneProvider(BackendJev, defaultJevModel, name, baseURL, apiKey, model)
}

// NewSubQProvider builds a SubQ Snap backend bound to name, posting to baseURL
// with apiKey as a bearer token and asking model (subq-snap-preview when empty).
func NewSubQProvider(name, baseURL, apiKey, model string) *SystemOneProvider {
	p := newSystemOneProvider(BackendSubQ, defaultSubQModel, name, baseURL, apiKey, model)
	p.maxQuestions = subQMaxQuestions
	return p
}

func newSystemOneProvider(backend, defaultModel, name, baseURL, apiKey, model string) *SystemOneProvider {
	if model == "" {
		model = defaultModel
	}
	return &SystemOneProvider{backend: backend, id: name, baseURL: baseURL, apiKey: apiKey, model: model, client: newHTTPClient()}
}

func (p *SystemOneProvider) ID() string { return p.id }

type systemOneChoiceQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions,omitempty"`
	Criteria     map[string]string `json:"criteria"`
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

// Decide asks one choice question whose criteria are the bare choice labels —
// the shape tln's `decide` block sends.
func (p *SystemOneProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if len(req.Choices) == 0 {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, errNoChoices)
	}
	criteria := make(map[string]string, len(req.Choices))
	for _, c := range req.Choices {
		criteria[c] = c
	}
	resp, err := p.ask(ctx, req.State, map[string]systemOneChoiceQuestion{systemOneQuestion: {Type: "choice", Criteria: criteria}})
	if err != nil {
		return nil, err
	}
	dec, err := p.decision(resp, systemOneQuestion, req.Choices)
	if err != nil {
		return nil, err
	}
	dec.Usage = p.usage(resp, req.State)
	return dec, nil
}

// DecideMany asks every question of req in one request: each becomes a
// "choice" question with its instructions and per-choice descriptions as
// criteria, so the state is sent and billed once. A backend with a lower
// per-request cap (SubQ Snap: 16) gets the questions in chunks of that size,
// sent in parallel; usage is the sum of the chunks, and any failing chunk fails
// the whole request, so callers never act on a partial set.
func (p *SystemOneProvider) DecideMany(ctx context.Context, req *MultiRequest) (*MultiDecision, error) {
	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, err)
	}
	size := p.maxQuestions
	if size <= 0 || size >= len(req.Order) {
		return p.decideChunk(ctx, req.State, req.Questions, req.Order)
	}
	chunks := slices.Collect(slices.Chunk(req.Order, size))
	results := make([]*MultiDecision, len(chunks))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The first chunk to fail cancels the others; its error is the one
	// returned, not the "context canceled" the others then report.
	var (
		wg       sync.WaitGroup
		failOnce sync.Once
		failed   error
	)
	for i, keys := range chunks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.decideChunk(ctx, req.State, req.Questions, keys)
			if err != nil {
				failOnce.Do(func() { failed = err; cancel() })
				return
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if failed != nil {
		return nil, failed
	}
	out := &MultiDecision{Answers: make(map[string]*Decision, len(req.Order))}
	var models []string
	silent := false
	for _, res := range results {
		if res.Model == "" {
			silent = true
		} else if !slices.Contains(models, res.Model) {
			models = append(models, res.Model)
		}
		out.Usage.InputTokens += res.Usage.InputTokens
		out.Usage.OutputTokens += res.Usage.OutputTokens
		for key, dec := range res.Answers {
			out.Answers[key] = dec
		}
	}
	if !silent {
		slices.Sort(models)
		out.Model = strings.Join(models, ",")
	}
	return out, nil
}

// decideChunk asks the questions named by keys in one request.
func (p *SystemOneProvider) decideChunk(ctx context.Context, state string, all map[string]Question, keys []string) (*MultiDecision, error) {
	questions := make(map[string]systemOneChoiceQuestion, len(keys))
	for _, key := range keys {
		q := all[key]
		criteria := make(map[string]string, len(q.Choices))
		for _, c := range q.Choices {
			criteria[c] = c
			if d := q.Descriptions[c]; d != "" {
				criteria[c] = d
			}
		}
		questions[key] = systemOneChoiceQuestion{Type: "choice", Instructions: q.Instructions, Criteria: criteria}
	}
	resp, err := p.ask(ctx, state, questions)
	if err != nil {
		return nil, err
	}
	out := &MultiDecision{Answers: make(map[string]*Decision, len(keys)), Usage: p.usage(resp, state), Model: resp.Model}
	for _, key := range keys {
		dec, err := p.decision(resp, key, all[key].Choices)
		if err != nil {
			return nil, err
		}
		out.Answers[key] = dec
	}
	return out, nil
}

// ask posts one System-1 request.
func (p *SystemOneProvider) ask(ctx context.Context, state string, questions map[string]systemOneChoiceQuestion) (*systemOneResponse, error) {
	if p.baseURL == "" {
		return nil, fmt.Errorf("%s %q: base_url is required", p.backend, p.id)
	}
	body := systemOneRequest{State: state, Model: p.model, Questions: questions}
	var resp systemOneResponse
	if err := postJSON(ctx, p.client, p.baseURL, p.apiKey, body, &resp); err != nil {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, err)
	}
	return &resp, nil
}

// decision turns the answer to question key into a normalized Decision over
// choices, rejecting an answer that is missing, of another type, outside the
// choices, without mass on them, or whose choice contradicts its probabilities.
func (p *SystemOneProvider) decision(resp *systemOneResponse, key string, choices []string) (*Decision, error) {
	ans, ok := resp.Answers[key]
	if !ok {
		return nil, fmt.Errorf("%s %q: response has no answer for question %q", p.backend, p.id, key)
	}
	if ans.Type != "choice" {
		return nil, fmt.Errorf("%s %q: answer %q has type %q, want choice", p.backend, p.id, key, ans.Type)
	}
	if ans.Choice != "" && !slices.Contains(choices, ans.Choice) {
		return nil, fmt.Errorf("%s %q: answer %q chose %q, which is not one of %v", p.backend, p.id, key, ans.Choice, choices)
	}
	probs := ans.Probabilities
	if len(probs) == 0 {
		if ans.Choice == "" {
			return nil, fmt.Errorf("%s %q: answer %q has neither probabilities nor choice", p.backend, p.id, key)
		}
		probs = map[string]float64{ans.Choice: 1}
	}
	var mass, top float64
	for _, c := range choices {
		if v := probs[c]; v > 0 {
			mass += v
			top = max(top, v)
		}
	}
	if mass == 0 {
		return nil, fmt.Errorf("%s %q: answer %q puts no probability mass on any of %v: %v", p.backend, p.id, key, choices, probs)
	}
	if ans.Choice != "" && probs[ans.Choice] < top {
		return nil, fmt.Errorf("%s %q: answer %q chose %q, which disagrees with its probabilities %v", p.backend, p.id, key, ans.Choice, probs)
	}
	dec, err := normalizeProbabilities(probs, choices)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", p.backend, p.id, err)
	}
	return dec, nil
}

// usage is the reported usage of resp, or an estimate from the state when the
// backend reports none.
func (p *SystemOneProvider) usage(resp *systemOneResponse, state string) Usage {
	if resp.Usage != nil {
		return Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}
	return Usage{InputTokens: approxTokens(state), OutputTokens: 1}
}

var (
	_ Provider      = (*SystemOneProvider)(nil)
	_ MultiProvider = (*SystemOneProvider)(nil)
)
