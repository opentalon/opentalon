package channel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// WebhookServer is a shared HTTP server that multiple YAML channels can
// register routes on. Only a single port is supported for MVP.
type WebhookServer struct {
	mu      sync.Mutex
	mux     *http.ServeMux
	server  *http.Server
	port    int
	started bool
	// handlers indexes each registered pattern by its swappable handler, so a
	// re-registration (e.g. a plugin reloaded after a crash) updates the route
	// in place. http.ServeMux PANICS on a duplicate pattern — calling
	// mux.Handle twice for the same path would crash the whole host, so a route
	// is only ever added to the mux once.
	handlers map[string]*swappableHandler
}

// swappableHandler is a route target whose underlying handler can be replaced
// atomically. The pattern is registered on the mux once (pointing here); a
// re-registration swaps `h` instead of re-adding the pattern.
type swappableHandler struct {
	mu sync.RWMutex
	h  http.HandlerFunc
}

func (s *swappableHandler) set(h http.HandlerFunc) {
	s.mu.Lock()
	s.h = h
	s.mu.Unlock()
}

func (s *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	h := s.h
	s.mu.RUnlock()
	if h == nil {
		http.Error(w, "handler not ready", http.StatusServiceUnavailable)
		return
	}
	h(w, r)
}

var globalWebhookServer = &WebhookServer{
	mux:      http.NewServeMux(),
	handlers: map[string]*swappableHandler{},
}

func webhookPortOrDefault(port int) int {
	if port <= 0 {
		return 3978
	}
	return port
}

// RegisterWebhookRoute registers an HTTP handler at path on the shared server.
// The server is started lazily on the first registration.
// If a different port is already in use, an error is returned.
func RegisterWebhookRoute(port int, path string, handler http.HandlerFunc) error {
	port = webhookPortOrDefault(port)
	return globalWebhookServer.register(port, path, handler)
}

func (s *WebhookServer) register(port int, path string, handler http.HandlerFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started && s.port != port {
		return fmt.Errorf("webhook server already started on port %d; cannot use port %d", s.port, port)
	}

	if s.handlers == nil {
		s.handlers = map[string]*swappableHandler{}
	}
	// Register each pattern on the mux ONCE. A repeat registration (a plugin
	// reloaded after exiting) swaps the handler in place — http.ServeMux panics
	// on a duplicate pattern, which previously crashed the host whenever a
	// killed plugin was retried.
	if sh, ok := s.handlers[path]; ok {
		sh.set(handler)
	} else {
		sh := &swappableHandler{h: handler}
		s.handlers[path] = sh
		s.mux.Handle(path, sh)
	}

	if !s.started {
		s.port = port
		server := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: s.mux,
		}
		s.server = server
		s.started = true
		go func(srv *http.Server, p int) {
			slog.Info("webhook-server listening", "port", p)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("webhook-server error", "error", err)
			}
		}(server, port)
	}

	return nil
}

// RegisterReverseProxy registers a reverse proxy on the shared webhook server.
// All requests to /{prefix}/* are forwarded to http://{targetAddr}/* with the
// prefix stripped. The server is started lazily on the first registration.
func RegisterReverseProxy(port int, prefix, targetAddr string) error {
	port = webhookPortOrDefault(port)
	target, err := url.Parse("http://" + targetAddr)
	if err != nil {
		return fmt.Errorf("invalid proxy target %q: %w", targetAddr, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	stripPrefix := "/" + strings.Trim(prefix, "/")
	pattern := stripPrefix + "/"
	// http.StripPrefix handles both URL.Path and URL.RawPath (percent-encoded
	// paths like %2F) correctly and avoids mutating the original request.
	handler := http.HandlerFunc(http.StripPrefix(stripPrefix, proxy).ServeHTTP)
	return globalWebhookServer.register(port, pattern, handler)
}

// Shutdown gracefully stops the webhook server.
func (s *WebhookServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.server == nil {
		return nil
	}
	if err := s.server.Shutdown(ctx); err != nil {
		return err
	}
	// Reset server state so routes can be registered again after shutdown.
	s.started = false
	s.server = nil
	s.port = 0
	s.mux = http.NewServeMux()
	return nil
}
