package health

import (
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Server is a lightweight gRPC server that exposes the standard
// grpc.health.v1.Health service for Kubernetes probes.
type Server struct {
	addr string
	gs   *grpc.Server
	hs   *health.Server
}

// New creates a health server on the given address (e.g. ":8086").
// The empty-string service ("") is set to SERVING immediately (liveness).
func New(addr string) *Server {
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	gs := grpc.NewServer()
	healthpb.RegisterHealthServer(gs, hs)

	return &Server{addr: addr, gs: gs, hs: hs}
}

// ListenAndServe starts the gRPC server. Blocks until Shutdown is called.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	slog.Info("health server listening", "addr", s.addr)
	return s.gs.Serve(ln)
}

// SetReady sets the serving status for a named service.
// Use service "opentalon" for the overall readiness probe.
func (s *Server) SetReady(service string, ready bool) {
	status := healthpb.HealthCheckResponse_NOT_SERVING
	if ready {
		status = healthpb.HealthCheckResponse_SERVING
	}
	s.hs.SetServingStatus(service, status)
	slog.Info("health status changed", "service", service, "ready", ready)
}

// Shutdown gracefully stops the gRPC server.
func (s *Server) Shutdown() {
	s.hs.Shutdown()
	s.gs.GracefulStop()
}
