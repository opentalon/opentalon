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

func reviewQuestions() *MultiRequest {
	return &MultiRequest{
		State: "guide + file",
		Questions: map[string]Question{
			"refund_cap": {
				Instructions: `Does the file follow "Money > Refund cap"?`,
				Choices:      []string{"keeps", "violates"},
				Descriptions: map[string]string{"keeps": "The file follows it", "violates": "The file breaks it"},
			},
			"secrets": {
				Instructions: `Does the file follow "Security > Secrets"?`,
				Choices:      []string{"keeps", "violates"},
			},
		},
		Order: []string{"refund_cap", "secrets"},
	}
}

// Jev and SubQ answer every question in one request: instructions and choice
// descriptions are sent as System-1 criteria (a choice without a description
// is sent with its label), each answer is normalized over its own choices,
// and usage is the request's, once.
func TestSystemOne_DecideManySendsEveryQuestionOnce(t *testing.T) {
	calls := 0
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"model":"subq-snap-preview","answers":{
			"refund_cap":{"type":"choice","choice":"violates","probabilities":{"keeps":0.02,"violates":0.98}},
			"secrets":{"type":"choice","choice":"keeps","probabilities":{"keeps":0.6,"violates":0.2}}},
			"usage":{"input_tokens":4800,"output_tokens":0}}`)
	}))
	defer srv.Close()

	dec, err := NewSubQProvider("snap", srv.URL, "k", "").DecideMany(context.Background(), reviewQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	want := map[string]any{
		"state": "guide + file",
		"model": "subq-snap-preview",
		"questions": map[string]any{
			"refund_cap": map[string]any{
				"type":         "choice",
				"instructions": `Does the file follow "Money > Refund cap"?`,
				"criteria":     map[string]any{"keeps": "The file follows it", "violates": "The file breaks it"},
			},
			"secrets": map[string]any{
				"type":         "choice",
				"instructions": `Does the file follow "Security > Secrets"?`,
				"criteria":     map[string]any{"keeps": "keeps", "violates": "violates"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body = %#v\nwant %#v", got, want)
	}
	if a := dec.Answers["refund_cap"]; a.Chosen != "violates" || math.Abs(a.Probabilities["violates"]-0.98) > 1e-9 {
		t.Errorf("refund_cap = %#v", a)
	}
	// 0.6 / 0.2 renormalizes to 0.75 / 0.25.
	if a := dec.Answers["secrets"]; a.Chosen != "keeps" || math.Abs(a.Confidence-0.75) > 1e-9 {
		t.Errorf("secrets = %#v, want keeps at 0.75", a)
	}
	if dec.Usage != (Usage{InputTokens: 4800}) {
		t.Errorf("usage = %#v, want 4800/0", dec.Usage)
	}
}

// One bad answer fails the whole request and names its question, so a caller
// never acts on a partial set.
func TestSystemOne_DecideManyBadAnswerNamesQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"refund_cap":{"type":"choice","choice":"keeps","probabilities":{"keeps":1}}}}`)
	}))
	defer srv.Close()
	_, err := NewJevProvider("jev", srv.URL, "", "").DecideMany(context.Background(), reviewQuestions())
	if err == nil || !strings.Contains(err.Error(), `no answer for question "secrets"`) {
		t.Fatalf("err = %v, want the missing question named", err)
	}
}

type singleOnly struct {
	states []string
	n      int
}

func (s *singleOnly) ID() string { return "laya" }

func (s *singleOnly) Decide(_ context.Context, req *Request) (*Decision, error) {
	s.states = append(s.states, req.State)
	s.n++
	return &Decision{Chosen: req.Choices[0], Confidence: 1, Probabilities: map[string]float64{req.Choices[0]: 1},
		Usage: Usage{InputTokens: 100 * s.n, OutputTokens: 1}}, nil
}

// A backend that only answers {state, choices} gets one call per question, with
// the instruction and the choice descriptions in front of the state; usage is
// summed and answers carry none of their own.
func TestDecideManyFallsBackToOneCallPerQuestion(t *testing.T) {
	p := &singleOnly{}
	dec, err := DecideMany(context.Background(), p, reviewQuestions())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.states) != 2 {
		t.Fatalf("calls = %d, want 2", len(p.states))
	}
	wantFirst := "Does the file follow \"Money > Refund cap\"?\n- keeps: The file follows it\n- violates: The file breaks it\n\nguide + file"
	if p.states[0] != wantFirst {
		t.Errorf("first state = %q\nwant %q", p.states[0], wantFirst)
	}
	if !strings.HasPrefix(p.states[1], "Does the file follow \"Security > Secrets\"?\n\nguide + file") {
		t.Errorf("second state = %q", p.states[1])
	}
	if dec.Usage != (Usage{InputTokens: 300, OutputTokens: 2}) {
		t.Errorf("usage = %#v, want 300/2", dec.Usage)
	}
	if dec.Answers["secrets"].Usage != (Usage{}) {
		t.Errorf("answer carries usage %#v", dec.Answers["secrets"].Usage)
	}
}

func TestMultiRequestValidate(t *testing.T) {
	two := []string{"keeps", "violates"}
	many := map[string]Question{}
	var order []string
	for i := 0; i <= MaxQuestions; i++ {
		k := strings.Repeat("q", i+1)
		many[k] = Question{Instructions: "x", Choices: two}
		order = append(order, k)
	}
	cases := map[string]struct {
		req  MultiRequest
		want string
	}{
		"no state":         {MultiRequest{Questions: map[string]Question{"a": {Instructions: "x", Choices: two}}, Order: []string{"a"}}, "state"},
		"no questions":     {MultiRequest{State: "s"}, "at least one question"},
		"too many":         {MultiRequest{State: "s", Questions: many, Order: order}, "at most"},
		"order mismatch":   {MultiRequest{State: "s", Questions: map[string]Question{"a": {Instructions: "x", Choices: two}}, Order: []string{"b"}}, "unknown question"},
		"no instructions":  {MultiRequest{State: "s", Questions: map[string]Question{"a": {Choices: two}}, Order: []string{"a"}}, "instructions"},
		"one choice":       {MultiRequest{State: "s", Questions: map[string]Question{"a": {Instructions: "x", Choices: []string{"keeps"}}}, Order: []string{"a"}}, "two choices"},
		"duplicate choice": {MultiRequest{State: "s", Questions: map[string]Question{"a": {Instructions: "x", Choices: []string{"keeps", "keeps"}}}, Order: []string{"a"}}, "twice"},
		"stray description": {MultiRequest{State: "s", Questions: map[string]Question{"a": {Instructions: "x", Choices: two,
			Descriptions: map[string]string{"maybe": "?"}}}, Order: []string{"a"}}, "not one of its choices"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := tc.req.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if err := reviewQuestions().Validate(); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
}
