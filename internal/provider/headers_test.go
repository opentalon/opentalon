package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// capturedRequest is what a test server saw for one request.
type capturedRequest struct {
	header http.Header
	body   []byte
}

// headerServer records every request and answers with a valid OpenAI-style
// reply: an SSE stream when the request asked for one, plain JSON otherwise,
// or an Anthropic reply when the path is /v1/messages.
type headerServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []capturedRequest
}

func newHeaderServer(t *testing.T) *headerServer {
	t.Helper()
	hs := &headerServer{}
	hs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hs.mu.Lock()
		hs.reqs = append(hs.reqs, capturedRequest{header: r.Header.Clone(), body: body})
		hs.mu.Unlock()

		switch {
		case r.URL.Path == anthropicMessagesPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"ok"}}]}`)
			_, _ = fmt.Fprintln(w, `data: [DONE]`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
		}
	}))
	t.Cleanup(hs.Close)
	return hs
}

func (hs *headerServer) last(t *testing.T) capturedRequest {
	t.Helper()
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if len(hs.reqs) == 0 {
		t.Fatal("server saw no request")
	}
	return hs.reqs[len(hs.reqs)-1]
}

var testCompletion = &CompletionRequest{
	Model:    "m",
	Messages: []Message{{Role: RoleUser, Content: "hi"}},
}

// callProvider runs one request through p, streaming or not, and drains it.
func callProvider(t *testing.T, p Provider, stream bool) error {
	t.Helper()
	ctx := context.Background()
	if !stream {
		_, err := p.Complete(ctx, testCompletion)
		return err
	}
	s, err := p.Stream(ctx, testCompletion)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	for {
		c, err := s.Recv()
		if err != nil {
			return err
		}
		if c.Done {
			return nil
		}
	}
}

func TestOpenAIHeaders(t *testing.T) {
	cases := []struct {
		name    string
		apiKey  string
		headers map[string]string
		want    map[string]string // header -> value; "" means must be absent
	}{
		{
			name:    "configured header is sent next to the bearer token",
			apiKey:  "sk-test",
			headers: map[string]string{"cf-access-token": "gateway-token"},
			want:    map[string]string{"Cf-Access-Token": "gateway-token", "Authorization": "Bearer sk-test", "Content-Type": "application/json"},
		},
		{
			name:    "configured Authorization replaces the default",
			apiKey:  "sk-test",
			headers: map[string]string{"Authorization": "Basic dXNlcjpwdw=="},
			want:    map[string]string{"Authorization": "Basic dXNlcjpwdw=="},
		},
		{
			name:    "empty api_key still sends no Authorization",
			apiKey:  "",
			headers: map[string]string{"X-Gateway": "g"},
			want:    map[string]string{"X-Gateway": "g", "Authorization": ""},
		},
		{
			name:    "empty value is not sent",
			apiKey:  "sk-test",
			headers: map[string]string{"cf-access-token": "", "X-Other": "o"},
			want:    map[string]string{"Cf-Access-Token": "", "X-Other": "o", "Authorization": "Bearer sk-test"},
		},
		{
			name:    "empty Authorization value keeps the default",
			apiKey:  "sk-test",
			headers: map[string]string{"Authorization": ""},
			want:    map[string]string{"Authorization": "Bearer sk-test"},
		},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				hs := newHeaderServer(t)
				p := NewOpenAIProvider("gw", hs.URL, tc.apiKey, nil, WithOpenAIHeaders(tc.headers))
				if err := callProvider(t, p, stream); err != nil {
					t.Fatalf("request: %v", err)
				}
				got := hs.last(t).header
				for name, want := range tc.want {
					if want == "" {
						if v, ok := got[http.CanonicalHeaderKey(name)]; ok {
							t.Errorf("%s = %q, want absent", name, v)
						}
						continue
					}
					if g := got.Get(name); g != want {
						t.Errorf("%s = %q, want %q", name, g, want)
					}
					if n := len(got.Values(name)); n != 1 {
						t.Errorf("%s sent %d times, want once", name, n)
					}
				}
			})
		}
	}
}

func TestAnthropicHeaders(t *testing.T) {
	// The Anthropic provider has no streaming path yet (Stream returns an
	// error), so Complete is its only request.
	cases := []struct {
		name    string
		headers map[string]string
		want    map[string]string // "" means must be absent
	}{
		{
			name:    "configured header is sent next to the defaults",
			headers: map[string]string{"cf-access-token": "gateway-token"},
			want: map[string]string{
				"Cf-Access-Token":   "gateway-token",
				"X-Api-Key":         "sk-ant-test",
				"Anthropic-Version": anthropicAPIVersion,
				"Content-Type":      "application/json",
			},
		},
		{
			name:    "configured x-api-key replaces the default",
			headers: map[string]string{"x-api-key": "other-key"},
			want:    map[string]string{"X-Api-Key": "other-key"},
		},
		{
			name:    "empty value is not sent",
			headers: map[string]string{"cf-access-token": ""},
			want:    map[string]string{"Cf-Access-Token": "", "X-Api-Key": "sk-ant-test"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHeaderServer(t)
			p := NewAnthropicProvider("gw", hs.URL, "sk-ant-test", nil, WithAnthropicHeaders(tc.headers))
			if err := callProvider(t, p, false); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			got := hs.last(t).header
			for name, want := range tc.want {
				if want == "" {
					if v, ok := got[http.CanonicalHeaderKey(name)]; ok {
						t.Errorf("%s = %q, want absent", name, v)
					}
					continue
				}
				if g := got.Get(name); g != want {
					t.Errorf("%s = %q, want %q", name, g, want)
				}
			}
		})
	}
}

// TestNoHeadersLeavesRequestsUnchanged pins that a provider without headers,
// with an empty map, or with only empty values sends exactly the same request
// (headers and body) as a provider built without the option.
func TestNoHeadersLeavesRequestsUnchanged(t *testing.T) {
	variants := map[string]map[string]string{
		"nil map":          nil,
		"empty map":        {},
		"only empty value": {"cf-access-token": ""},
	}
	type build func(url string, h map[string]string, with bool) Provider
	providers := []struct {
		name    string
		build   build
		streams []bool
	}{
		{"openai", func(url string, h map[string]string, with bool) Provider {
			if !with {
				return NewOpenAIProvider("p", url, "sk", nil)
			}
			return NewOpenAIProvider("p", url, "sk", nil, WithOpenAIHeaders(h))
		}, []bool{false, true}},
		{"anthropic", func(url string, h map[string]string, with bool) Provider {
			if !with {
				return NewAnthropicProvider("p", url, "sk", nil)
			}
			return NewAnthropicProvider("p", url, "sk", nil, WithAnthropicHeaders(h))
		}, []bool{false}},
	}
	for _, pv := range providers {
		for _, stream := range pv.streams {
			hs := newHeaderServer(t)
			if err := callProvider(t, pv.build(hs.URL, nil, false), stream); err != nil {
				t.Fatalf("%s baseline: %v", pv.name, err)
			}
			baseline := hs.last(t)
			for vname, h := range variants {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", pv.name, stream, vname), func(t *testing.T) {
					if err := callProvider(t, pv.build(hs.URL, h, true), stream); err != nil {
						t.Fatalf("request: %v", err)
					}
					got := hs.last(t)
					if !reflect.DeepEqual(got.header, baseline.header) {
						t.Errorf("headers changed:\n got  %v\n want %v", got.header, baseline.header)
					}
					if string(got.body) != string(baseline.body) {
						t.Errorf("body changed:\n got  %s\n want %s", got.body, baseline.body)
					}
				})
			}
		}
	}
}

func TestWithHeadersCopiesTheMap(t *testing.T) {
	hs := newHeaderServer(t)
	h := map[string]string{"X-Gateway": "before"}
	p := NewOpenAIProvider("p", hs.URL, "", nil, WithOpenAIHeaders(h))
	h["X-Gateway"] = "after"
	if err := callProvider(t, p, false); err != nil {
		t.Fatal(err)
	}
	if got := hs.last(t).header.Get("X-Gateway"); got != "before" {
		t.Errorf("X-Gateway = %q, want the value at construction time", got)
	}
}

func TestFromConfigPassesHeaders(t *testing.T) {
	for _, api := range []string{APIOpenAI, APIAnthropic} {
		t.Run(api, func(t *testing.T) {
			hs := newHeaderServer(t)
			p, err := FromConfig(ProviderConfig{
				ID: "gw", BaseURL: hs.URL, APIKey: "k", API: api,
				Headers: HeaderMap{"cf-access-token": "gateway-token"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := callProvider(t, p, false); err != nil {
				t.Fatal(err)
			}
			if got := hs.last(t).header.Get("cf-access-token"); got != "gateway-token" {
				t.Errorf("cf-access-token = %q, want gateway-token", got)
			}
		})
	}
}

func TestHTTPHealthProbeSendsHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	probe := NewHTTPHealthProbeWithHeaders(srv.URL, "sk", ProbeAuthBearer,
		map[string]string{"cf-access-token": "gateway-token", "X-Empty": ""}, nil)
	if err := probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if v := got.Get("cf-access-token"); v != "gateway-token" {
		t.Errorf("cf-access-token = %q, want gateway-token", v)
	}
	if v := got.Get("Authorization"); v != "Bearer sk" {
		t.Errorf("Authorization = %q, want Bearer sk", v)
	}
	if _, ok := got["X-Empty"]; ok {
		t.Error("X-Empty sent, want absent for an empty value")
	}
}

// TestHeaderValuesNeverLeak drives every path that records or reports a
// request (slog at debug level, /debug capture, session events, errors,
// printed configs) and checks the configured header value appears nowhere.
func TestHeaderValuesNeverLeak(t *testing.T) {
	const secret = "s3cr3t-gateway-token-value"
	headers := map[string]string{"cf-access-token": secret}

	logBuf, restore := withSlogCapture(t, slog.LevelDebug)
	defer restore()

	assertNoSecret := func(t *testing.T, where, s string) {
		t.Helper()
		if strings.Contains(s, secret) {
			t.Errorf("header value leaked into %s: %s", where, s)
		}
	}

	ok := newHeaderServer(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer failing.Close()

	debug := &recordingSink{}
	events := &recordingEventSink{}
	newOAI := func(url string) Provider {
		return NewOpenAIProvider("gw", url, "sk", nil,
			WithOpenAIHeaders(headers),
			WithOpenAIDebugSink(debug),
			WithOpenAIDebugResolver(alwaysOnResolver("sess", "trace")),
			WithOpenAISessionEventSink(events),
			fastRetry())
	}
	newAnth := func(url string) Provider {
		return NewAnthropicProvider("gw", url, "sk", nil,
			WithAnthropicHeaders(headers),
			WithAnthropicSessionEventSink(events),
			WithAnthropicRetryPolicy(RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxTotalWait: time.Second}))
	}

	var errs []string
	for _, stream := range []bool{false, true} {
		if err := callProvider(t, newOAI(ok.URL), stream); err != nil {
			t.Fatalf("openai ok stream=%v: %v", stream, err)
		}
		if err := callProvider(t, newOAI(failing.URL), stream); err == nil {
			t.Fatalf("openai failing stream=%v: want an error", stream)
		} else {
			errs = append(errs, err.Error())
		}
	}
	if err := callProvider(t, newAnth(ok.URL), false); err != nil {
		t.Fatalf("anthropic ok: %v", err)
	}
	if err := callProvider(t, newAnth(failing.URL), false); err == nil {
		t.Fatal("anthropic failing: want an error")
	} else {
		errs = append(errs, err.Error())
	}
	// Transport failure: the error names the URL, never the headers.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if err := callProvider(t, newAnth(closed.URL), false); err == nil {
		t.Fatal("closed server: want an error")
	} else {
		errs = append(errs, err.Error())
	}
	probe := NewHTTPHealthProbeWithHeaders(failing.URL, "sk", ProbeAuthBearer, headers, nil)
	if err := probe(context.Background()); err == nil {
		t.Fatal("probe against failing server: want an error")
	} else {
		errs = append(errs, err.Error())
	}

	// The requests really carried the header, so the checks below are meaningful.
	if got := ok.last(t).header.Get("cf-access-token"); got != secret {
		t.Fatalf("test server did not receive the header (got %q)", got)
	}

	if logBuf.Len() == 0 {
		t.Fatal("expected debug log output to inspect")
	}
	assertNoSecret(t, "slog output", logBuf.String())
	if len(debug.snapshot()) == 0 {
		t.Fatal("expected /debug capture events to inspect")
	}
	for _, e := range debug.snapshot() {
		assertNoSecret(t, "debug capture", fmt.Sprintf("%+v", e))
	}
	if len(events.snapshot()) == 0 {
		t.Fatal("expected session events to inspect")
	}
	for _, e := range events.snapshot() {
		raw, _ := json.Marshal(e)
		assertNoSecret(t, "session event", string(raw)+" "+string(e.Payload))
	}
	for _, e := range errs {
		assertNoSecret(t, "error message", e)
	}

	cfg := ProviderConfig{ID: "gw", APIKey: "sk", Headers: HeaderMap(headers)}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		out := fmt.Sprintf(verb, cfg)
		assertNoSecret(t, "printed ProviderConfig ("+verb+")", out)
		if !strings.Contains(out, "cf-access-token:[redacted]") {
			t.Errorf("printed ProviderConfig (%s) should still name the header: %s", verb, out)
		}
	}
}
