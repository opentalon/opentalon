package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A refused redirect must not quote a Location that carries the token,
// neither raw in the query nor URL-escaped in the path.
func TestRefusedRedirectErrorHidesLocation(t *testing.T) {
	const odd = "tok/en+with space&x=12345"
	headers := map[string]string{"cf-access-token": echoSecret, "X-Odd": odd}
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loc := target.URL + "/" + url.PathEscape(odd) + r.URL.Path + "?token=" + echoSecret + "&k=" + url.QueryEscape(odd)
		http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
	}))
	defer front.Close()

	calls := map[string]func() error{
		"openai complete": func() error {
			_, err := NewOpenAIProvider("gw", front.URL, "", nil, WithOpenAIHeaders(headers)).Complete(context.Background(), testCompletion)
			return err
		},
		"openai stream": func() error {
			_, err := NewOpenAIProvider("gw", front.URL, "", nil, WithOpenAIHeaders(headers)).Stream(context.Background(), testCompletion)
			return err
		},
		"anthropic complete": func() error {
			_, err := NewAnthropicProvider("gw", front.URL, "", nil, WithAnthropicHeaders(headers)).Complete(context.Background(), testCompletion)
			return err
		},
		"health probe": func() error {
			return NewHTTPHealthProbeWithHeaders(front.URL, "", ProbeAuthBearer, headers, nil)(context.Background())
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
				t.Fatalf("error = %v, want a refused redirect", err)
			}
			for _, leak := range []string{echoSecret, odd, url.PathEscape(odd), url.QueryEscape(odd), "token="} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error quotes %q: %v", leak, err)
				}
			}
			var ue *url.Error
			if !errors.As(err, &ue) {
				t.Errorf("errors.As(*url.Error) must keep working: %v", err)
			}
		})
	}
	if targetHits.Load() != 0 {
		t.Errorf("redirect target contacted %d times", targetHits.Load())
	}
}

// Cancellation and deadlines stay recognisable through the cleaned errors.
func TestCleanedErrorsKeepContextErrors(t *testing.T) {
	headers := map[string]string{"cf-access-token": echoSecret}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client going away.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewOpenAIProvider("gw", slow.URL, "", nil, WithOpenAIHeaders(headers)).Complete(ctx, testCompletion)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: errors.Is(DeadlineExceeded) = false for %v", err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel2() }()
	_, err = NewAnthropicProvider("gw", slow.URL, "", nil, WithAnthropicHeaders(headers)).Complete(ctx2, testCompletion)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: errors.Is(Canceled) = false for %v", err)
	}
}

func TestCleanError(t *testing.T) {
	secrets := []string{echoSecret}
	if got := cleanError(context.Canceled, secrets); got != context.Canceled {
		t.Errorf("an error without a secret must be returned unchanged, got %#v", got)
	}
	err := cleanError(fmt.Errorf("bad trailer %s: %w", echoSecret, context.DeadlineExceeded), secrets)
	if strings.Contains(err.Error(), echoSecret) || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("message not redacted: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("errors.Is(DeadlineExceeded) must keep working")
	}
	if errors.Unwrap(err) != nil {
		t.Error("the unredacted cause must not be reachable through Unwrap")
	}
	var te interface{ Timeout() bool }
	if !errors.As(err, &te) || !te.Timeout() {
		t.Error("a deadline must still report Timeout() = true")
	}
}

// errAfterReader returns data, then err.
type errAfterReader struct {
	data string
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestRedactingReaderCleansReadErrors(t *testing.T) {
	src := &errAfterReader{data: "partial body ", err: fmt.Errorf("malformed chunked trailer %q: %w", echoSecret, context.DeadlineExceeded)}
	r := &redactingReader{src: io.NopCloser(src), secrets: []string{echoSecret}}
	body, err := io.ReadAll(r)
	if string(body) != "partial body " {
		t.Errorf("body = %q", body)
	}
	if err == nil || strings.Contains(err.Error(), echoSecret) {
		t.Fatalf("read error = %v, want it without the secret", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(DeadlineExceeded) = false for %v", err)
	}

	eof := &redactingReader{src: io.NopCloser(strings.NewReader("x")), secrets: []string{echoSecret}}
	_, _ = io.ReadAll(eof)
	if _, err := eof.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("end of body must be io.EOF itself, got %#v", err)
	}
}

// Go sends " token " as "token", so a padded value must be redacted in its
// trimmed form, and a value of only spaces must not be sent.
func TestPaddedHeaderValues(t *testing.T) {
	var gotBlank bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, gotBlank = r.Header["X-Blank"]
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "invalid token: "+r.Header.Get("cf-access-token"))
	}))
	defer srv.Close()
	headers := map[string]string{"cf-access-token": " \t" + echoSecret + "  ", "X-Blank": "  \t "}
	_, err := NewOpenAIProvider("gw", srv.URL, "", nil, WithOpenAIHeaders(headers)).Complete(context.Background(), testCompletion)
	if err == nil || strings.Contains(err.Error(), echoSecret) || !strings.Contains(err.Error(), "invalid token: [redacted]") {
		t.Fatalf("error = %v, want the trimmed value redacted", err)
	}
	if gotBlank {
		t.Error("a value of only spaces must not be sent")
	}
}
