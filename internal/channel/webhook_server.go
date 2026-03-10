package channel

import (
	"context"
	"fmt"
	"log"
	"net/http"
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

// RegisterWebhookRoute registers an HTTP handler at path on the shared server.
// The server is started lazily on the first registration.
// If a different port is already in use, an error is returned.
func RegisterWebhookRoute(port int, path string, handler http.HandlerFunc) error {
	if port <= 0 {
		port = 3978
	}
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
		s.server = &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: s.mux,
		}
		s.started = true
		go func() {
			log.Printf("webhook-server: listening on :%d", port)
			if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("webhook-server: error: %v", err)
			}
		}()
	}

	return nil
}

// Shutdown gracefully stops the webhook server.
func (s *WebhookServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}
