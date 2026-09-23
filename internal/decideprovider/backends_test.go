package decideprovider

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLaya_Probabilities(t *testing.T) {
	var gotBody layaRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_ = json.NewEncoder(w).Encode(layaResponse{
			Probabilities: map[string]float64{"Spam": 0.8, "Legitimate": 0.2},
		})
	}))
	defer srv.Close()

	p := NewLayaProvider("laya", srv.URL, "")
	dec, err := p.Decide(context.Background(), &Request{
		State:   "Subject: win a prize",
		Choices: []string{"Legitimate", "Spam", "Phishing"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.Text == "" || len(gotBody.Choices) != 3 {
		t.Errorf("server saw request %#v", gotBody)
	}
	if dec.Chosen != "Spam" {
		t.Errorf("chosen = %q, want Spam", dec.Chosen)
	}
	if dec.Probabilities["Phishing"] != 0 {
		t.Errorf("unseen Phishing = %v, want 0", dec.Probabilities["Phishing"])
	}
	if dec.Usage.InputTokens == 0 || dec.Usage.OutputTokens != 0 {
		t.Errorf("laya usage = %#v, want input>0 output=0", dec.Usage)
	}
}

func TestLaya_ScoresAreSoftmaxed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(layaResponse{Scores: map[string]float64{"A": 2, "B": 1}})
	}))
	defer srv.Close()
	dec, err := NewLayaProvider("laya", srv.URL, "").Decide(context.Background(), &Request{Choices: []string{"A", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(dec.Probabilities["A"]-0.7310585786) > 1e-6 {
		t.Errorf("A = %v, want softmax ~0.731", dec.Probabilities["A"])
	}
}

func TestJev_PassesThroughUsageAndBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"chosen":"Spam","confidence":0.9,
			"probabilities":{"Spam":0.9,"Legitimate":0.1},
			"usage":{"input_tokens":12,"output_tokens":1}}`)
	}))
	defer srv.Close()

	dec, err := NewJevProvider("jev", srv.URL, "secret").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"Legitimate", "Spam"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q, want Bearer secret", gotAuth)
	}
	if dec.Chosen != "Spam" || dec.Usage.InputTokens != 12 {
		t.Errorf("decision = %#v", dec)
	}
}

func TestJev_ChosenOnlyBecomesDistribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"chosen":"B"}`)
	}))
	defer srv.Close()
	dec, err := NewJevProvider("jev", srv.URL, "").Decide(context.Background(),
		&Request{Choices: []string{"A", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen != "B" || dec.Probabilities["B"] != 1 {
		t.Errorf("decision = %#v, want B=1", dec)
	}
}

func TestLocalLogits_ReadsFirstTokenLogprobs(t *testing.T) {
	var gotReq completionsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotReq)
		// Leading-space, sub-word tokens — exercises prefix matching.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"logprobs": map[string]any{
					"top_logprobs": []map[string]float64{{
						" Leg":  -0.2, // -> Legitimate
						" Spam": -1.6, // -> Spam
						" the":  -3.0, // no choice
					}},
				},
			}},
			"usage": map[string]int{"prompt_tokens": 20, "completion_tokens": 1},
		})
	}))
	defer srv.Close()

	dec, err := NewLocalLogitsProvider("ll", srv.URL, "", "qwen3-0.6b").Decide(context.Background(),
		&Request{State: "Subject: hi", Choices: []string{"Legitimate", "Spam", "Phishing"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.MaxTokens != 1 || gotReq.Logprobs != defaultLogprobsTopN {
		t.Errorf("request = %#v, want max_tokens=1 logprobs=%d", gotReq, defaultLogprobsTopN)
	}
	if dec.Chosen != "Legitimate" {
		t.Errorf("chosen = %q, want Legitimate (highest logprob)", dec.Chosen)
	}
	if dec.Probabilities["Phishing"] != 0 {
		t.Errorf("Phishing (absent token) = %v, want 0", dec.Probabilities["Phishing"])
	}
	// softmax(-0.2, -1.6) for Legitimate.
	want := math.Exp(-0.2) / (math.Exp(-0.2) + math.Exp(-1.6))
	if math.Abs(dec.Probabilities["Legitimate"]-want) > 1e-6 {
		t.Errorf("Legitimate = %v, want %v", dec.Probabilities["Legitimate"], want)
	}
	if dec.Usage.InputTokens != 20 {
		t.Errorf("usage input = %d, want 20", dec.Usage.InputTokens)
	}
}

func TestMatchChoiceLogits_PrefixAndMax(t *testing.T) {
	// A leading-space sub-word token (" spa") plus a bare one ("Spam") both map
	// to Spam; the max logprob wins. Built programmatically so the space in the
	// key is unambiguous.
	top := map[string]float64{"Spam": -0.5, "leg": -2.0}
	top[" spa"] = -0.9
	got := matchChoiceLogits(top, []string{"Legitimate", "Spam"})
	if got["Spam"] != -0.5 { // "Spam" and " spa" both match; keep max
		t.Errorf("Spam = %v, want -0.5", got["Spam"])
	}
	if got["Legitimate"] != -2.0 {
		t.Errorf("Legitimate = %v, want -2.0", got["Legitimate"])
	}
}

func TestMatchChoiceLogits_SharedPrefixIsDisambiguated(t *testing.T) {
	// "Spam" is a prefix of "Spammy". Exact tokens must credit only their own
	// label; the shared-but-longer token "Spamm" resolves uniquely to Spammy.
	got := matchChoiceLogits(
		map[string]float64{"Spam": -0.3, "Spammy": -1.0, "Spamm": -0.1},
		[]string{"Spam", "Spammy"},
	)
	if got["Spam"] != -0.3 {
		t.Errorf("Spam = %v, want -0.3 (exact match only)", got["Spam"])
	}
	if got["Spammy"] != -0.1 { // max(-1.0 exact, -0.1 unique prefix)
		t.Errorf("Spammy = %v, want -0.1", got["Spammy"])
	}
}

func TestMatchChoiceLogits_AmbiguousPrefixDropped(t *testing.T) {
	// A token that prefixes two labels and matches neither exactly is ambiguous;
	// it must not be split equally across them (the old silent-collision bug).
	got := matchChoiceLogits(map[string]float64{"Spamm": -0.2}, []string{"Spammy", "Spammer"})
	if len(got) != 0 {
		t.Errorf("ambiguous token credited someone: %#v", got)
	}
}

func TestLocalLogits_SharedPrefixChoicesDoNotCollide(t *testing.T) {
	// End-to-end: the decision must reflect the distinguishing tokens, not the
	// declared-order tie-break that a collision would trigger.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"logprobs": map[string]any{
				"top_logprobs": []map[string]float64{{"Spam": -2.0, "Spammy": -0.1}},
			}}},
		})
	}))
	defer srv.Close()
	dec, err := NewLocalLogitsProvider("ll", srv.URL, "", "m").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"Spam", "Spammy"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen != "Spammy" {
		t.Errorf("chosen = %q, want Spammy (its token dominates despite Spam being first)", dec.Chosen)
	}
}

func TestBackends_HTTPErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewLayaProvider("l", srv.URL, "").Decide(context.Background(),
		&Request{Choices: []string{"A"}}); err == nil {
		t.Fatal("expected error on 500")
	}
}
