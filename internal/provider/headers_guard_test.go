package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"
)

const echoSecret = "s3cr3t-gateway-token-value"

func TestRedactionSecrets(t *testing.T) {
	got := redactionSecrets(map[string]string{
		"cf-access-token": echoSecret,
		"Authorization":   "Bearer tokentoken123",
		"X-Short":         "eu-1",
		"X-Empty":         "",
	})
	want := []string{"Bearer tokentoken123", echoSecret, "tokentoken123"}
	if len(got) != len(want) {
		t.Fatalf("secrets = %q, want %q", got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("secrets %q missing %q", got, w)
		}
	}
	for i := 1; i < len(got); i++ {
		if len(got[i]) > len(got[i-1]) {
			t.Errorf("secrets not sorted longest first: %q", got)
		}
	}
}

func TestRedactingReader(t *testing.T) {
	secrets := redactionSecrets(map[string]string{"a": echoSecret, "b": "another-secret-1"})
	cases := []struct {
		name string
		in   string
	}{
		{"no secret", "plain body with nothing to hide\n"},
		{"secret in the middle", "invalid token: " + echoSecret + " (expired)"},
		{"secret at the end", "invalid token: " + echoSecret},
		{"two secrets", echoSecret + "/" + "another-secret-1" + "/" + echoSecret},
		{"partial prefix only", "starts like s3cr3t-gate but is not the secret"},
		{"prefix at EOF", "ends with s3cr3t-gat"},
		{"empty", ""},
	}
	readers := map[string]func(string) io.Reader{
		"whole":      func(s string) io.Reader { return strings.NewReader(s) },
		"one byte":   func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"half reads": func(s string) io.Reader { return iotest.HalfReader(strings.NewReader(s)) },
	}
	for _, tc := range cases {
		for rname, mk := range readers {
			t.Run(tc.name+"/"+rname, func(t *testing.T) {
				r := &redactingReader{src: io.NopCloser(mk(tc.in)), secrets: secrets}
				got, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				want := redactString(tc.in, secrets)
				if string(got) != want {
					t.Errorf("got %q, want %q", got, want)
				}
				if strings.Contains(string(got), echoSecret) {
					t.Errorf("secret survived: %q", got)
				}
			})
		}
	}
}

// A complete line must come out without waiting for more data, so SSE events
// are not delayed by the redaction.
func TestRedactingReaderDoesNotDelayCompleteLines(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	r := &redactingReader{src: pr, secrets: []string{echoSecret}}
	go func() { _, _ = io.WriteString(pw, "data: {\"x\":1}\n") }()

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := r.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if got != "data: {\"x\":1}\n" {
			t.Errorf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a complete line was held back")
	}
}

func TestGuardClientWithoutHeadersIsUnchanged(t *testing.T) {
	c := &http.Client{}
	if got := guardClient(c, nil); got != c {
		t.Error("guardClient without headers must return the same client")
	}
	if got := guardClient(c, map[string]string{}); got != c {
		t.Error("guardClient with an empty map must return the same client")
	}
	g := guardClient(c, map[string]string{"X": echoSecret})
	if g == c || c.Transport != nil || c.CheckRedirect != nil {
		t.Error("guardClient must not modify the client it was given")
	}
}

// leakChecks gathers everything a provider records and asserts the secret is
// in none of it.
type leakChecks struct {
	t      *testing.T
	logBuf interface{ String() string }
	debug  *recordingSink
	events *recordingEventSink
}

func newLeakChecks(t *testing.T) *leakChecks {
	buf, restore := withSlogCapture(t, slog.LevelDebug)
	t.Cleanup(restore)
	return &leakChecks{t: t, logBuf: buf, debug: &recordingSink{}, events: &recordingEventSink{}}
}

func (lc *leakChecks) openAI(url string, headers map[string]string) *OpenAIProvider {
	return NewOpenAIProvider("gw", url, "sk", nil,
		WithOpenAIHeaders(headers),
		WithOpenAIDebugSink(lc.debug),
		WithOpenAIDebugResolver(alwaysOnResolver("sess", "trace")),
		WithOpenAISessionEventSink(lc.events),
		fastRetry())
}

func (lc *leakChecks) assertClean(errs ...error) {
	t := lc.t
	t.Helper()
	check := func(where, s string) {
		t.Helper()
		if strings.Contains(s, echoSecret) {
			t.Errorf("secret leaked into %s: %s", where, s)
		}
	}
	check("slog output", lc.logBuf.String())
	for _, e := range lc.debug.snapshot() {
		check("debug capture", fmt.Sprintf("%+v", e))
	}
	for _, e := range lc.events.snapshot() {
		raw, _ := json.Marshal(e)
		check("session event", string(raw)+" "+string(e.Payload))
	}
	for _, err := range errs {
		if err != nil {
			check("returned error", err.Error())
		}
	}
}

func TestEchoedCredentialIsRedacted(t *testing.T) {
	headers := map[string]string{"cf-access-token": echoSecret}
	echo := func(w http.ResponseWriter, r *http.Request, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"invalid token: %s"}}`, r.Header.Get("cf-access-token"))
	}

	t.Run("openai http error", func(t *testing.T) {
		lc := newLeakChecks(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { echo(w, r, http.StatusUnauthorized) }))
		defer srv.Close()
		_, err := lc.openAI(srv.URL, headers).Complete(context.Background(), testCompletion)
		if err == nil || !strings.Contains(err.Error(), "invalid token: [redacted]") {
			t.Fatalf("error = %v, want the echoed token redacted", err)
		}
		lc.assertClean(err)
	})

	t.Run("openai streaming http error", func(t *testing.T) {
		lc := newLeakChecks(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { echo(w, r, http.StatusForbidden) }))
		defer srv.Close()
		_, err := lc.openAI(srv.URL, headers).Stream(context.Background(), testCompletion)
		if err == nil || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("error = %v, want the echoed token redacted", err)
		}
		lc.assertClean(err)
	})

	t.Run("openai error inside a stream, split across writes", func(t *testing.T) {
		lc := newLeakChecks(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			tok := r.Header.Get("cf-access-token")
			_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
			f.Flush()
			_, _ = fmt.Fprintf(w, `data: {"error":{"type":"auth","message":"token %s`, tok[:7])
			f.Flush()
			time.Sleep(20 * time.Millisecond)
			_, _ = fmt.Fprintf(w, "%s expired\"}}\n", tok[7:])
			f.Flush()
		}))
		defer srv.Close()
		s, err := lc.openAI(srv.URL, headers).Stream(context.Background(), testCompletion)
		if err != nil {
			t.Fatal(err)
		}
		var streamErr error
		for {
			c, err := s.Recv()
			if err != nil {
				streamErr = err
				break
			}
			if c.Done {
				break
			}
		}
		closeErr := s.Close()
		if streamErr == nil || !strings.Contains(streamErr.Error(), "token [redacted] expired") {
			t.Fatalf("stream error = %v, want the echoed token redacted", streamErr)
		}
		lc.assertClean(streamErr, closeErr)
	})

	t.Run("openai retried response", func(t *testing.T) {
		lc := newLeakChecks(t)
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				echo(w, r, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
		}))
		defer srv.Close()
		if _, err := lc.openAI(srv.URL, headers).Complete(context.Background(), testCompletion); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 2 {
			t.Fatalf("server calls = %d, want 2 (one retry)", calls.Load())
		}
		var retryEvents int
		for _, e := range lc.events.snapshot() {
			if e.EventType == "retry" {
				retryEvents++
				if !strings.Contains(string(e.Payload), "[redacted]") {
					t.Errorf("retry event should carry the redacted snippet: %s", e.Payload)
				}
			}
		}
		if retryEvents == 0 {
			t.Error("expected a retry session event to inspect")
		}
		if !strings.Contains(lc.logBuf.String(), "invalid token: [redacted]") {
			t.Errorf("retry log should show the redacted snippet: %s", lc.logBuf.String())
		}
		lc.assertClean()
	})

	t.Run("anthropic http error", func(t *testing.T) {
		lc := newLeakChecks(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { echo(w, r, http.StatusUnauthorized) }))
		defer srv.Close()
		p := NewAnthropicProvider("gw", srv.URL, "sk", nil,
			WithAnthropicHeaders(headers), WithAnthropicSessionEventSink(lc.events))
		_, err := p.Complete(context.Background(), testCompletion)
		if err == nil || !strings.Contains(err.Error(), "[redacted]") {
			t.Fatalf("error = %v, want the echoed token redacted", err)
		}
		lc.assertClean(err)
	})

	t.Run("bearer token alone", func(t *testing.T) {
		lc := newLeakChecks(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "bad token "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		}))
		defer srv.Close()
		_, err := lc.openAI(srv.URL, map[string]string{"Authorization": "Bearer " + echoSecret}).
			Complete(context.Background(), testCompletion)
		if err == nil || !strings.Contains(err.Error(), "bad token [redacted]") {
			t.Fatalf("error = %v, want the token redacted", err)
		}
		lc.assertClean(err)
	})

	t.Run("short value is left alone", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "unknown region eu-1")
		}))
		defer srv.Close()
		_, err := NewOpenAIProvider("gw", srv.URL, "", nil, WithOpenAIHeaders(map[string]string{"X-Region": "eu-1"})).
			Complete(context.Background(), testCompletion)
		if err == nil || !strings.Contains(err.Error(), "unknown region eu-1") {
			t.Fatalf("error = %v, want the body untouched for a value shorter than %d", err, minRedactLen)
		}
	})
}

// redirectPair starts a target server and a front server that redirects
// every request with 307 (which keeps method and body) either to the target
// (another origin: different port) or to a path on itself.
func redirectPair(t *testing.T, crossOrigin bool) (front *httptest.Server, targetHits *atomic.Int32, seen *atomic.Value) {
	t.Helper()
	targetHits = &atomic.Int32{}
	seen = &atomic.Value{}
	seen.Store("")
	reply := func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("cf-access-token"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_1","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
		}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		reply(w, r)
	}))
	t.Cleanup(target.Close)
	front = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/moved") {
			reply(w, r)
			return
		}
		dest := "/moved" + r.URL.Path
		if crossOrigin {
			dest = target.URL + r.URL.Path
		}
		http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(front.Close)
	return front, targetHits, seen
}

func TestCrossOriginRedirectIsRefusedWithHeaders(t *testing.T) {
	headers := map[string]string{"cf-access-token": echoSecret}
	calls := map[string]func(url string, h map[string]string) error{
		"openai complete": func(url string, h map[string]string) error {
			_, err := NewOpenAIProvider("gw", url, "", nil, WithOpenAIHeaders(h)).Complete(context.Background(), testCompletion)
			return err
		},
		"openai stream": func(url string, h map[string]string) error {
			s, err := NewOpenAIProvider("gw", url, "", nil, WithOpenAIHeaders(h)).Stream(context.Background(), testCompletion)
			if err == nil {
				_ = s.Close()
			}
			return err
		},
		"anthropic complete": func(url string, h map[string]string) error {
			_, err := NewAnthropicProvider("gw", url, "", nil, WithAnthropicHeaders(h)).Complete(context.Background(), testCompletion)
			return err
		},
		"health probe": func(url string, h map[string]string) error {
			return NewHTTPHealthProbeWithHeaders(url, "", ProbeAuthBearer, h, nil)(context.Background())
		},
	}
	for name, call := range calls {
		t.Run(name+"/cross origin with headers is refused", func(t *testing.T) {
			front, hits, _ := redirectPair(t, true)
			err := call(front.URL, headers)
			if err == nil || !strings.Contains(err.Error(), "refusing redirect to a different origin") {
				t.Fatalf("error = %v, want the redirect refused", err)
			}
			if strings.Contains(err.Error(), echoSecret) {
				t.Errorf("error leaks the header value: %v", err)
			}
			if hits.Load() != 0 {
				t.Errorf("redirect target was contacted %d times, want 0", hits.Load())
			}
		})
		t.Run(name+"/cross origin without headers is followed as before", func(t *testing.T) {
			front, hits, _ := redirectPair(t, true)
			if err := call(front.URL, nil); err != nil {
				t.Fatalf("error = %v, want the redirect followed", err)
			}
			if hits.Load() != 1 {
				t.Errorf("redirect target hits = %d, want 1", hits.Load())
			}
		})
		t.Run(name+"/same origin with headers is followed", func(t *testing.T) {
			front, _, seen := redirectPair(t, false)
			if err := call(front.URL, headers); err != nil {
				t.Fatalf("error = %v, want the redirect followed", err)
			}
			if got := seen.Load().(string); got != echoSecret {
				t.Errorf("header on the redirected request = %q, want it kept", got)
			}
		})
	}
}
