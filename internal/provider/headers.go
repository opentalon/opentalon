package provider

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// HeaderMap holds extra HTTP request headers configured for a provider
// (models.providers.<id>.headers). It is a plain map[string]string underneath;
// the named type only exists so that printing a ProviderConfig with fmt shows
// the header names but never their values, which usually carry credentials.
type HeaderMap map[string]string

// Format prints the header names with every value replaced by [redacted].
func (h HeaderMap) Format(f fmt.State, _ rune) {
	if h == nil {
		_, _ = io.WriteString(f, "map[]")
		return
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("map[")
	for i, k := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteString(":[redacted]")
	}
	b.WriteByte(']')
	_, _ = io.WriteString(f, b.String())
}

// copyExtraHeaders returns a private copy of h without the entries whose value
// is empty, so an unset credential sends no header instead of an empty one.
// It returns nil when nothing is left, which keeps requests unchanged.
func copyExtraHeaders(h map[string]string) map[string]string {
	var out map[string]string
	for k, v := range h {
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(h))
		}
		out[k] = v
	}
	return out
}

// applyExtraHeaders sets the configured headers on req. Callers run it after
// setting their default headers, so a configured header with the same name
// replaces the default one.
func applyExtraHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		req.Header.Set(k, v)
	}
}

// minRedactLen is the shortest configured header value that is removed from
// response bodies and errors. Shorter values are not treated as credentials:
// replacing every occurrence of, say, "eu" or "v2" would mangle replies
// without protecting anything.
const minRedactLen = 8

const redactedMarker = "[redacted]"

// redactionSecrets returns the strings to remove from anything the provider
// reads back from the endpoint: each configured header value, and for values
// of the form "Bearer <token>", "Basic <token>" or "Token <token>" also the
// token alone, since a gateway may echo either. Longest first, so a value
// that contains another one is replaced whole.
func redactionSecrets(headers map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len(s) >= minRedactLen && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, v := range headers {
		add(v)
		if scheme, token, ok := strings.Cut(v, " "); ok {
			switch strings.ToLower(scheme) {
			case "bearer", "basic", "token":
				add(strings.TrimSpace(token))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func redactString(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, redactedMarker)
	}
	return s
}

// guardClient prepares an HTTP client for a provider with extra headers. It
// returns c itself when there are none, so such providers behave exactly as
// before. Otherwise it returns a copy whose transport removes the header
// values from every response body (and transport error) before any caller
// reads it, and whose redirect policy refuses to follow a redirect to a
// different origin.
//
// Wrap the result with withRetry afterwards: retry then sits above the
// redaction and only ever sees redacted bodies.
func guardClient(c *http.Client, headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return c
	}
	cp := *c
	base := cp.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	cp.Transport = &redactingTransport{base: base, secrets: redactionSecrets(headers)}
	cp.CheckRedirect = sameOriginRedirects(c.CheckRedirect)
	return &cp
}

// sameOriginRedirects refuses a redirect whose target has a different scheme,
// host or port than the original request. Go's client forwards custom
// headers to any redirect target (it only drops Authorization and Cookie
// across hosts), so following one would hand the configured credentials to
// another server. Same-origin redirects are still followed under the previous
// policy (Go's default: at most 10).
func sameOriginRedirects(prev func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > 0 && originOf(req.URL) != originOf(via[0].URL) {
			return fmt.Errorf("refusing redirect to a different origin (%s): extra headers are configured for this provider", originOf(req.URL))
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
}

func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// redactingTransport replaces the configured header values in response bodies
// and transport errors.
type redactingTransport struct {
	base    http.RoundTripper
	secrets []string
}

func (t *redactingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		if msg := err.Error(); len(t.secrets) > 0 && redactString(msg, t.secrets) != msg {
			return nil, errors.New(redactString(msg, t.secrets))
		}
		return nil, err
	}
	if len(t.secrets) > 0 && resp.Body != nil && resp.Body != http.NoBody {
		resp.Body = &redactingReader{src: resp.Body, secrets: t.secrets}
		// The length can change; let readers go by EOF instead.
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	}
	return resp, nil
}

// redactingReader streams src with every secret replaced. It holds back only
// a trailing part that could still turn into a secret once more data
// arrives; header values cannot contain line breaks, so a complete line (an
// SSE event) is never delayed.
type redactingReader struct {
	src     io.ReadCloser
	secrets []string
	pending string // read from src, not yet safe to hand out
	out     string // redacted, ready to hand out
	err     error
	buf     []byte
}

func (r *redactingReader) Read(p []byte) (int, error) {
	for r.out == "" && r.err == nil {
		if r.buf == nil {
			r.buf = make([]byte, 32*1024)
		}
		n, err := r.src.Read(r.buf)
		r.pending += string(r.buf[:n])
		if err != nil {
			r.err = err
		}
		r.process(r.err != nil)
	}
	if r.out != "" {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *redactingReader) process(final bool) {
	s := redactString(r.pending, r.secrets)
	hold := 0
	if !final {
		hold = longestSecretPrefixSuffix(s, r.secrets)
	}
	r.out += s[:len(s)-hold]
	r.pending = s[len(s)-hold:]
}

// longestSecretPrefixSuffix returns the length of the longest end of s that
// is the start of some secret.
func longestSecretPrefixSuffix(s string, secrets []string) int {
	best := 0
	for _, sec := range secrets {
		for k := min(len(sec)-1, len(s)); k > best; k-- {
			if strings.HasSuffix(s, sec[:k]) {
				best = k
				break
			}
		}
	}
	return best
}

func (r *redactingReader) Close() error { return r.src.Close() }
