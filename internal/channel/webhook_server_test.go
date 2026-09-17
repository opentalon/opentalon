package channel

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// buildProxyHandler replicates the handler construction in RegisterReverseProxy
// so tests can use it without starting a real listener.
func buildProxyHandler(t *testing.T, prefix, backendURL string) (pattern string, handler http.Handler) {
	t.Helper()
	target, err := url.Parse(backendURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", backendURL, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	stripPrefix := "/" + strings.Trim(prefix, "/")
	return stripPrefix + "/", http.StripPrefix(stripPrefix, proxy)
}

func TestReverseProxy_PathStripping(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
	}))
	defer backend.Close()

	pattern, handler := buildProxyHandler(t, "myplugin", backend.URL)
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/myplugin/foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "/foo/bar" {
		t.Errorf("backend received path %q, want /foo/bar", got)
	}
}

func TestReverseProxy_RootPath(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
	}))
	defer backend.Close()

	pattern, handler := buildProxyHandler(t, "myplugin", backend.URL)
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/myplugin/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got != "/" {
		t.Errorf("backend received path %q, want /", got)
	}
}

// TestReverseProxy_NoTrailingSlashRedirect confirms that Go's ServeMux issues
// a redirect for /{prefix} → /{prefix}/ so plugin users don't get a 404.
// Go 1.22+ ServeMux uses 307 (rather than 301) for these redirects.
func TestReverseProxy_NoTrailingSlashRedirect(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	pattern, handler := buildProxyHandler(t, "myplugin", backend.URL)
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Disable automatic redirect following so we can inspect the response.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(srv.URL + "/myplugin")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 3 {
		t.Errorf("status = %d, want a 3xx redirect", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if loc != "/myplugin/" {
		t.Errorf("Location = %q, want /myplugin/", loc)
	}
}

// TestReverseProxy_EncodedPath confirms that percent-encoded segments
// (e.g. a path containing %2F) are forwarded without double-decoding.
func TestReverseProxy_EncodedPath(t *testing.T) {
	var gotRaw string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" {
			gotRaw = r.URL.RawPath
		} else {
			gotRaw = r.URL.Path
		}
	}))
	defer backend.Close()

	pattern, handler := buildProxyHandler(t, "myplugin", backend.URL)
	mux := http.NewServeMux()
	mux.Handle(pattern, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/myplugin/a%2Fb")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotRaw != "/a%2Fb" {
		t.Errorf("backend RawPath = %q, want /a%%2Fb", gotRaw)
	}
}

// TestWebhookServerReRegisterSwapsWithoutPanic is a regression guard: a plugin
// reloaded after exiting re-registers its reverse-proxy pattern. http.ServeMux
// panics on a duplicate pattern, which previously crashed the whole host every
// time a killed plugin was retried. Re-registration must swap in place instead.
func TestWebhookServerReRegisterSwapsWithoutPanic(t *testing.T) {
	s := &WebhookServer{
		mux:      http.NewServeMux(),
		handlers: map[string]*swappableHandler{},
		started:  true, // pre-started so register() doesn't bind a real port
		port:     9999,
	}
	got := 0
	first := func(w http.ResponseWriter, r *http.Request) { got = 1 }
	second := func(w http.ResponseWriter, r *http.Request) { got = 2 }

	if err := s.register(9999, "/tln-plugin/", first); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Re-registering the SAME pattern must NOT panic (would crash the host).
	if err := s.register(9999, "/tln-plugin/", second); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	s.mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/tln-plugin/x", nil))
	if got != 2 {
		t.Fatalf("expected the re-registered handler to win (got=2), got=%d", got)
	}
}
