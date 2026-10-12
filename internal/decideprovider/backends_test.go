package decideprovider

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestJev_SendsSystemOneChoiceQuestion(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"decision":{"type":"choice","choice":"Spam",
			"confidence":1.0,"probabilities":{"Legitimate":0.0,"Spam":1.0}}},
			"usage":{"input_tokens":315,"output_tokens":35}}`)
	}))
	defer srv.Close()

	dec, err := NewJevProvider("jev", srv.URL+"/v1/systemone", "secret", "").Decide(context.Background(),
		&Request{State: "Subject: WIN A FREE IPHONE", Choices: []string{"Legitimate", "Spam"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Errorf("auth = %q, want Bearer secret", gotAuth)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want the base_url as given", gotPath)
	}
	want := map[string]any{
		"state": "Subject: WIN A FREE IPHONE",
		"model": "jev-latest",
		"questions": map[string]any{
			"decision": map[string]any{
				"type":     "choice",
				"criteria": map[string]any{"Legitimate": "Legitimate", "Spam": "Spam"},
			},
		},
	}
	if !reflect.DeepEqual(gotBody, want) {
		t.Errorf("request body = %#v, want %#v", gotBody, want)
	}
	if dec.Chosen != "Spam" || dec.Probabilities["Spam"] != 1 || dec.Probabilities["Legitimate"] != 0 {
		t.Errorf("decision = %#v, want Spam=1", dec)
	}
	if dec.Usage.InputTokens != 315 || dec.Usage.OutputTokens != 35 {
		t.Errorf("usage = %#v, want 315/35", dec.Usage)
	}
}

func TestJev_SendsConfiguredModel(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = io.WriteString(w, `{"answers":{"decision":{"type":"choice","choice":"A","probabilities":{"A":0.6,"B":0.4}}}}`)
	}))
	defer srv.Close()

	if _, err := NewJevProvider("jev", srv.URL, "", "jev-1.13.0").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"A", "B"}}); err != nil {
		t.Fatal(err)
	}
	if gotBody["model"] != "jev-1.13.0" {
		t.Errorf("model = %v, want jev-1.13.0", gotBody["model"])
	}
}

func TestJev_RenormalizesInconsistentProbabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"decision":{"type":"choice","choice":"A","confidence":0.78,
			"probabilities":{"A":0.89,"B":0.11,"C":0.5}}},"usage":{"input_tokens":284,"output_tokens":31}}`)
	}))
	defer srv.Close()

	dec, err := NewJevProvider("jev", srv.URL, "", "").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"A", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen != "A" || math.Abs(dec.Confidence-0.89) > 1e-9 || len(dec.Probabilities) != 2 {
		t.Errorf("decision = %#v, want A at 0.89 over [A B] only", dec)
	}
}

func TestJev_ChoiceOnlyBecomesDistribution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"decision":{"type":"choice","choice":"B"}}}`)
	}))
	defer srv.Close()
	dec, err := NewJevProvider("jev", srv.URL, "", "").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"A", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Chosen != "B" || dec.Probabilities["B"] != 1 {
		t.Errorf("decision = %#v, want B=1", dec)
	}
	if dec.Usage.InputTokens != 1 || dec.Usage.OutputTokens != 1 {
		t.Errorf("usage = %#v, want estimated 1/1 without reported usage", dec.Usage)
	}
}

func TestJev_Errors(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		choices []string
		want    string
	}{
		{"http 400", http.StatusBadRequest, `{"detail":{"error_type":"api_usage_error","message":"Invalid request."}}`, []string{"A", "B"}, "Invalid request."},
		{"http 422", http.StatusUnprocessableEntity, `{"detail":[{"type":"missing","loc":["body","model"]}]}`, []string{"A", "B"}, "422"},
		{"no answers", http.StatusOK, `{"model":"jev-1.13.0"}`, []string{"A", "B"}, "no answer"},
		{"other question only", http.StatusOK, `{"answers":{"urgency":{"type":"noul","noul":0.97}}}`, []string{"A", "B"}, "no answer"},
		{"wrong answer type", http.StatusOK, `{"answers":{"decision":{"type":"score","score":1.0}}}`, []string{"A", "B"}, "type"},
		{"empty answer", http.StatusOK, `{"answers":{"decision":{"type":"choice"}}}`, []string{"A", "B"}, "neither"},
		{"choice outside declared", http.StatusOK, `{"answers":{"decision":{"type":"choice","choice":"Z"}}}`, []string{"A", "B"}, "not one of"},
		{"choice outside declared with probabilities", http.StatusOK, `{"answers":{"decision":{"type":"choice","choice":"Z","probabilities":{"Z":1}}}}`, []string{"A", "B"}, "not one of"},
		{"probabilities only on undeclared labels", http.StatusOK, `{"answers":{"decision":{"type":"choice","probabilities":{"Z":1}}}}`, []string{"A", "B"}, "no probability mass"},
		{"declared choices all zero", http.StatusOK, `{"answers":{"decision":{"type":"choice","choice":"A","probabilities":{"A":0,"B":0}}}}`, []string{"A", "B"}, "no probability mass"},
		{"declared choices negative", http.StatusOK, `{"answers":{"decision":{"type":"choice","probabilities":{"A":-0.5,"B":-0.5}}}}`, []string{"A", "B"}, "no probability mass"},
		{"choice disagrees with probabilities", http.StatusOK, `{"answers":{"decision":{"type":"choice","choice":"A","probabilities":{"A":0.2,"B":0.8}}}}`, []string{"A", "B"}, "disagrees"},
		{"not json", http.StatusOK, `<html>bad gateway</html>`, []string{"A", "B"}, "decode"},
		{"no choices", http.StatusOK, `{"answers":{"decision":{"type":"choice","choice":"A"}}}`, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := NewJevProvider("jev", srv.URL, "", "").Decide(context.Background(),
				&Request{State: "x", Choices: tc.choices})
			if err == nil {
				t.Fatal("Decide succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `jev "jev"`) {
				t.Errorf("error = %q, want it to name the decider and contain %q", err, tc.want)
			}
		})
	}
}

func TestJev_ChoiceTiedWithArgmaxIsAccepted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"decision":{"type":"choice","choice":"B","probabilities":{"A":0.5,"B":0.5}}}}`)
	}))
	defer srv.Close()
	dec, err := NewJevProvider("jev", srv.URL, "", "").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"A", "B"}})
	if err != nil {
		t.Fatalf("Decide: %v, want a tie with the argmax accepted", err)
	}
	if dec.Confidence != 0.5 {
		t.Errorf("confidence = %v, want 0.5", dec.Confidence)
	}
}

func TestJev_RequiresBaseURL(t *testing.T) {
	_, err := NewJevProvider("jev", "", "", "").Decide(context.Background(),
		&Request{State: "x", Choices: []string{"A", "B"}})
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error = %v, want base_url required", err)
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
