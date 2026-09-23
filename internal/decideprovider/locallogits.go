package decideprovider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// BackendLocalLogits is the config `backend` value selecting the local-logits
// backend.
const BackendLocalLogits = "local-logits"

// defaultLogprobsTopN is how many top tokens to request at the first generated
// position. It must be wide enough that every choice's leading token appears.
const defaultLogprobsTopN = 20

// LocalLogitsProvider implements the "Jev in 25 lines" recipe against any
// OpenAI-compatible completions server (llama.cpp `llama-server`, vLLM, Ollama's
// OpenAI shim, etc.): it prompts the model for a single token, reads the top
// logprobs at that position, keeps the logprob of each declared choice's leading
// token, and softmaxes over the choices here — so calibration doesn't depend on
// the model being well-behaved.
//
//	POST <base_url>/completions
//	  { "model": "...", "prompt": "<state>\n\nAnswer with one of: A, B.\nAnswer:",
//	    "max_tokens": 1, "temperature": 0, "logprobs": 20 }
//	-> choices[0].logprobs.top_logprobs[0] : { " A": -0.2, " B": -1.9, ... }
//
// Unlike laya/Jev this backend does the distribution math itself; the server
// only has to return first-token logprobs.
type LocalLogitsProvider struct {
	id      string
	baseURL string
	apiKey  string
	model   string
	topN    int
	client  *http.Client
}

// NewLocalLogitsProvider builds a local-logits backend bound to name. baseURL is
// the OpenAI-compatible root (e.g. http://localhost:8080/v1); model is the
// served model id; apiKey is optional.
func NewLocalLogitsProvider(name, baseURL, apiKey, model string) *LocalLogitsProvider {
	return &LocalLogitsProvider{
		id:      name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		topN:    defaultLogprobsTopN,
		client:  newHTTPClient(),
	}
}

func (p *LocalLogitsProvider) ID() string { return p.id }

type completionsRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	Logprobs    int     `json:"logprobs"`
}

type completionsResponse struct {
	Choices []struct {
		Logprobs struct {
			TopLogprobs []map[string]float64 `json:"top_logprobs"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (p *LocalLogitsProvider) Decide(ctx context.Context, req *Request) (*Decision, error) {
	if len(req.Choices) == 0 {
		return nil, fmt.Errorf("local-logits %q: %w", p.id, errNoChoices)
	}
	prompt := buildLogitsPrompt(req.State, req.Choices)
	var resp completionsResponse
	if err := postJSON(ctx, p.client, p.baseURL+"/completions", p.apiKey, completionsRequest{
		Model:       p.model,
		Prompt:      prompt,
		MaxTokens:   1,
		Temperature: 0,
		Logprobs:    p.topN,
	}, &resp); err != nil {
		return nil, fmt.Errorf("local-logits %q: %w", p.id, err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Logprobs.TopLogprobs) == 0 {
		return nil, fmt.Errorf("local-logits %q: response carried no first-token logprobs", p.id)
	}
	logits := matchChoiceLogits(resp.Choices[0].Logprobs.TopLogprobs[0], req.Choices)
	dec, err := softmaxLogits(logits, req.Choices)
	if err != nil {
		return nil, fmt.Errorf("local-logits %q: %w", p.id, err)
	}
	in := resp.Usage.PromptTokens
	if in == 0 {
		in = approxTokens(prompt)
	}
	dec.Usage = Usage{InputTokens: in, OutputTokens: max(resp.Usage.CompletionTokens, 1)}
	return dec, nil
}

// buildLogitsPrompt frames the state as a single-token typed decision.
func buildLogitsPrompt(state string, choices []string) string {
	var b strings.Builder
	b.WriteString(state)
	b.WriteString("\n\nAnswer with exactly one of: ")
	b.WriteString(strings.Join(choices, ", "))
	b.WriteString(".\nAnswer:")
	return b.String()
}

// matchChoiceLogits keeps, for each declared choice, the highest logprob among
// returned tokens that identify it (case- and whitespace-insensitive).
// Tokenizers emit leading-space and sub-word tokens, so matching a token back to
// a choice label needs care when labels share a prefix:
//
//   - An exact match (token == label) always wins and is unambiguous — so with
//     choices ["Spam","Spammy"] the token "Spam" credits only "Spam".
//   - A non-exact prefix match ("Spamm" -> "Spammy") counts only when exactly
//     one label is prefixed by that token. A token that prefixes two or more
//     labels (and matches none exactly) is genuinely ambiguous from a single
//     position and is dropped rather than split equally across them — which the
//     old code did, letting finalize's tie-break silently pick the first.
//
// Choices no token identifies are left absent (softmaxLogits reads that as
// probability 0; if every choice is absent it falls back to uniform — honest for
// deeply prefix-colliding labels the model can't distinguish in one token).
func matchChoiceLogits(top map[string]float64, choices []string) map[string]float64 {
	lower := make([]string, len(choices))
	for i, c := range choices {
		lower[i] = strings.ToLower(strings.TrimSpace(c))
	}
	out := make(map[string]float64, len(choices))
	credit := func(i int, lp float64) {
		c := choices[i]
		if cur, ok := out[c]; !ok || lp > cur {
			out[c] = lp
		}
	}
	for tok, lp := range top {
		t := strings.ToLower(strings.TrimSpace(tok))
		if t == "" {
			continue
		}
		exact, prefixed := -1, -1
		prefixCount := 0
		for i, cl := range lower {
			switch {
			case cl == "" || !strings.HasPrefix(cl, t):
				continue
			case cl == t:
				exact = i
			default:
				prefixed = i
				prefixCount++
			}
		}
		switch {
		case exact >= 0:
			credit(exact, lp) // exact match is unambiguous
		case prefixCount == 1:
			credit(prefixed, lp) // unique prefix
		}
		// prefixCount > 1 with no exact match: ambiguous → drop.
	}
	return out
}

var _ Provider = (*LocalLogitsProvider)(nil)
