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
}

var globalWebhookServer = &WebhookServer{
	mux: http.NewServeMux(),
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

	s.mux.HandleFunc(path, handler)

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
