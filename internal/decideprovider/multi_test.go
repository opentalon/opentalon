package decideprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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

// manyQuestions builds n keeps/violates questions, keyed q00, q01, ….
func manyQuestions(n int) *MultiRequest {
	req := &MultiRequest{State: "guide + file", Questions: map[string]Question{}}
	for i := range n {
		key := fmt.Sprintf("q%02d", i)
		req.Questions[key] = Question{Instructions: "Does the file follow " + key + "?", Choices: []string{"keeps", "violates"}}
		req.Order = append(req.Order, key)
	}
	return req
}

// fakeSystemOne is a System-1 server for chunking tests: it answers every
// question it gets with "keeps" and records the size of each request.
type fakeSystemOne struct {
	fail   string        // a request containing this key gets status
	status int           // the failure status (default 500)
	delay  time.Duration // how long requests without fail take
	drop   string        // this key's answer is left out of the response
	mu     sync.Mutex
	sizes  []int
}

func (f *fakeSystemOne) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]any `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.sizes = append(f.sizes, len(body.Questions))
		f.mu.Unlock()
		if _, ok := body.Questions[f.fail]; ok && f.fail != "" {
			status := f.status
			if status == 0 {
				status = http.StatusInternalServerError
			}
			http.Error(w, "rate limited", status)
			return
		}
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
		answers := map[string]any{}
		for key := range body.Questions {
			if key != f.drop {
				answers[key] = map[string]any{"type": "choice", "choice": "keeps", "probabilities": map[string]float64{"keeps": 0.9, "violates": 0.1}}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers, "usage": map[string]int{"input_tokens": 1000, "output_tokens": 0}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeSystemOne) requestSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	sizes := slices.Clone(f.sizes)
	slices.Sort(sizes)
	return sizes
}

// noRetryWait makes postJSON's retries immediate for the test.
func noRetryWait(t *testing.T) {
	t.Helper()
	old := retryWait
	retryWait = func(int, string) time.Duration { return 0 }
	t.Cleanup(func() { retryWait = old })
}

// SubQ Snap answers at most 16 questions per request: 40 questions go out as
// 16 + 16 + 8, every answer comes back, and usage is the sum of the requests.
func TestSubQ_DecideManySplitsIntoChunksOf16(t *testing.T) {
	f := &fakeSystemOne{}
	dec, err := NewSubQProvider("snap", f.serve(t).URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.requestSizes(); !reflect.DeepEqual(got, []int{8, 16, 16}) {
		t.Errorf("request sizes = %v, want [8 16 16]", got)
	}
	if len(dec.Answers) != 40 {
		t.Errorf("answers = %d, want 40", len(dec.Answers))
	}
	if dec.Usage.InputTokens != 3000 {
		t.Errorf("input tokens = %d, want 3000 (three requests)", dec.Usage.InputTokens)
	}
}

// The boundary: 16 questions stay one request, 17 go out as 16 + 1.
func TestSubQ_DecideManyChunkBoundary(t *testing.T) {
	for n, want := range map[int][]int{1: {1}, 16: {16}, 17: {1, 16}, 32: {16, 16}, 33: {1, 16, 16}} {
		f := &fakeSystemOne{}
		dec, err := NewSubQProvider("snap", f.serve(t).URL, "k", "").DecideMany(context.Background(), manyQuestions(n))
		if err != nil {
			t.Fatalf("%d questions: %v", n, err)
		}
		if got := f.requestSizes(); !reflect.DeepEqual(got, want) {
			t.Errorf("%d questions: request sizes = %v, want %v", n, got, want)
		}
		if len(dec.Answers) != n {
			t.Errorf("%d questions: answers = %d", n, len(dec.Answers))
		}
	}
}

// Jev takes the same 40 questions in one request.
func TestJev_DecideManyKeepsOneRequest(t *testing.T) {
	f := &fakeSystemOne{}
	if _, err := NewJevProvider("jev", f.serve(t).URL, "k", "").DecideMany(context.Background(), manyQuestions(40)); err != nil {
		t.Fatal(err)
	}
	if got := f.requestSizes(); !reflect.DeepEqual(got, []int{40}) {
		t.Errorf("request sizes = %v, want [40]", got)
	}
}

// The chunk that fails decides the error: with the failure in the last chunk
// and the others still in flight, the caller sees the 429, not the "context
// canceled" the cancelled chunks report.
func TestSubQ_DecideManyReturnsTheFailingChunksError(t *testing.T) {
	noRetryWait(t)
	f := &fakeSystemOne{fail: "q35", status: http.StatusTooManyRequests, delay: 300 * time.Millisecond}
	dec, err := NewSubQProvider("snap", f.serve(t).URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
	if err == nil {
		t.Fatalf("want an error, got %d answers", len(dec.Answers))
	}
	if dec != nil {
		t.Errorf("want no decision with the error, got %#v", dec)
	}
	if !strings.Contains(err.Error(), "429") || errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want the chunk's 429", err)
	}
}

// A chunk whose response leaves out one of its questions fails the whole call.
func TestSubQ_DecideManyMissingAnswerFailsAll(t *testing.T) {
	f := &fakeSystemOne{drop: "q17"}
	dec, err := NewSubQProvider("snap", f.serve(t).URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
	if err == nil || !strings.Contains(err.Error(), `"q17"`) {
		t.Fatalf("error = %v, want one naming q17", err)
	}
	if dec != nil {
		t.Errorf("want no decision with the error, got %#v", dec)
	}
}

// modelServer answers every question with "keeps" and reports model, or
// other for any request whose questions include q20.
func modelServer(model, other string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Questions map[string]any `json:"questions"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		m := model
		if _, ok := body.Questions["q20"]; ok {
			m = other
		}
		answers := map[string]any{}
		for key := range body.Questions {
			answers[key] = map[string]any{"type": "choice", "choice": "keeps", "probabilities": map[string]float64{"keeps": 0.9, "violates": 0.1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": m, "answers": answers})
	}))
}

func TestSystemOne_DecideManyReportsModel(t *testing.T) {
	cases := map[string]struct {
		p     func(url string) *SystemOneProvider
		n     int
		model string
	}{
		"jev one request":       {func(url string) *SystemOneProvider { return NewJevProvider("jev", url, "k", "jev-latest") }, 40, "jev-1.13.0"},
		"subq 16 questions":     {func(url string) *SystemOneProvider { return NewSubQProvider("snap", url, "k", "") }, 16, "subq-snap-preview"},
		"subq chunks one model": {func(url string) *SystemOneProvider { return NewSubQProvider("snap", url, "k", "") }, 40, "subq-snap-preview"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := modelServer(tc.model, tc.model)
			defer srv.Close()
			dec, err := tc.p(srv.URL).DecideMany(context.Background(), manyQuestions(tc.n))
			if err != nil {
				t.Fatal(err)
			}
			if dec.Model != tc.model {
				t.Errorf("model = %q, want %q", dec.Model, tc.model)
			}
		})
	}
}

// Chunks answered by different models say so: every distinct model, sorted,
// so a caller keying thresholds on the model finds none and a log shows both.
func TestSubQ_DecideManyChunksFromDifferentModels(t *testing.T) {
	srv := modelServer("subq-snap-preview", "subq-snap-2")
	defer srv.Close()
	dec, err := NewSubQProvider("snap", srv.URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "subq-snap-2,subq-snap-preview" {
		t.Errorf("model = %q, want both models", dec.Model)
	}
}

// A backend that reports no model leaves it empty rather than guessing the
// requested one.
func TestSubQ_DecideManyNoModelReported(t *testing.T) {
	srv := modelServer("", "")
	defer srv.Close()
	dec, err := NewSubQProvider("snap", srv.URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Model != "" {
		t.Errorf("model = %q, want empty", dec.Model)
	}
}

// A chunk that reports no model while another does leaves the model empty, so
// no answer is labelled with a model nobody confirmed for it.
func TestSubQ_DecideManySilentChunkLeavesModelEmpty(t *testing.T) {
	cases := map[string]struct{ model, other string }{
		"q20 chunk silent":       {"subq-snap-preview", ""},
		"only q20 chunk reports": {"", "subq-snap-preview"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := modelServer(tc.model, tc.other)
			defer srv.Close()
			dec, err := NewSubQProvider("snap", srv.URL, "k", "").DecideMany(context.Background(), manyQuestions(40))
			if err != nil {
				t.Fatal(err)
			}
			if dec.Model != "" {
				t.Errorf("model = %q, want empty", dec.Model)
			}
		})
	}
}
