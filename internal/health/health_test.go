package health

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func startTestServer(t *testing.T) (*Server, healthpb.HealthClient) {
	t.Helper()

	// Use a random available port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	srv := New(addr)
	go func() {
		_ = srv.ListenAndServe()
	}()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Shutdown()
	})

	return srv, healthpb.NewHealthClient(conn)
}

func TestLivenessServingOnStart(t *testing.T) {
	_, client := startTestServer(t)

	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{Service: ""})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.Status)
	}
}

func TestReadinessNotFoundByDefault(t *testing.T) {
	_, client := startTestServer(t)

	_, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "opentalon"})
	if err == nil {
		t.Fatal("expected error for unknown service")
	}
	if s, ok := status.FromError(err); !ok || s.Code() != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestReadinessTransition(t *testing.T) {
	srv, client := startTestServer(t)

	// Initially not ready.
	srv.SetReady("opentalon", false)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "opentalon"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING, got %v", resp.Status)
	}

	// Mark ready.
	srv.SetReady("opentalon", true)
	resp, err = client.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "opentalon"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %v", resp.Status)
	}
}
