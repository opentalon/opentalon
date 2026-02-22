package channel

import (
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/channel"
)

func TestDetectMode(t *testing.T) {
	tests := []struct {
		input string
		want  pkg.PluginMode
	}{
		{"./plugins/opentalon-slack", pkg.ModeBinary},
		{"/usr/local/bin/opentalon-slack", pkg.ModeBinary},
		{"plugins/my-plugin", pkg.ModeBinary},
		{"grpc://localhost:9001", pkg.ModeGRPC},
		{"grpc://telegram-bot.internal:443", pkg.ModeGRPC},
		{"GRPC://UPPER.CASE:9001", pkg.ModeGRPC},
		{"docker://ghcr.io/opentalon/plugin-teams:latest", pkg.ModeDocker},
		{"docker://my-registry/plugin:v2", pkg.ModeDocker},
		{"DOCKER://IMAGE:TAG", pkg.ModeDocker},
		{"http://localhost:8080/webhook", pkg.ModeWebhook},
		{"https://us-central1-proj.cloudfunctions.net/wa", pkg.ModeWebhook},
		{"HTTP://UPPERCASE.COM/path", pkg.ModeWebhook},
		{"ws://localhost:9090/channel", pkg.ModeWebSocket},
		{"wss://custom-bridge.example.com/channel", pkg.ModeWebSocket},
		{"WSS://UPPER.CASE/WS", pkg.ModeWebSocket},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := pkg.DetectMode(tt.input)
			if got != tt.want {
				t.Errorf("DetectMode(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectModeString(t *testing.T) {
	tests := []struct {
		mode pkg.PluginMode
		want string
	}{
		{pkg.ModeBinary, "binary"},
		{pkg.ModeGRPC, "grpc"},
		{pkg.ModeDocker, "docker"},
		{pkg.ModeWebhook, "webhook"},
		{pkg.ModeWebSocket, "websocket"},
		{pkg.PluginMode(99), "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := tt.mode.String(); got != tt.want {
				t.Errorf("PluginMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestParsePluginAddress(t *testing.T) {
	tests := []struct {
		input    string
		wantMode pkg.PluginMode
		wantAddr string
	}{
		{"./plugins/slack", pkg.ModeBinary, "./plugins/slack"},
		{"grpc://host:9001", pkg.ModeGRPC, "host:9001"},
		{"docker://img:tag", pkg.ModeDocker, "img:tag"},
		{"https://example.com/hook", pkg.ModeWebhook, "https://example.com/hook"},
		{"wss://ws.example.com/ch", pkg.ModeWebSocket, "wss://ws.example.com/ch"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			mode, addr := pkg.ParsePluginAddress(tt.input)
			if mode != tt.wantMode {
				t.Errorf("ParsePluginAddress(%q) mode = %v, want %v", tt.input, mode, tt.wantMode)
			}
			if addr != tt.wantAddr {
				t.Errorf("ParsePluginAddress(%q) addr = %q, want %q", tt.input, addr, tt.wantAddr)
			}
		})
	}
}

func TestParsePluginRef(t *testing.T) {
	ref, err := pkg.ParsePluginRef("grpc://myhost:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Raw != "grpc://myhost:443" {
		t.Errorf("Raw = %q, want %q", ref.Raw, "grpc://myhost:443")
	}
	if ref.Mode != pkg.ModeGRPC {
		t.Errorf("Mode = %v, want %v", ref.Mode, pkg.ModeGRPC)
	}
	if ref.Address != "myhost:443" {
		t.Errorf("Address = %q, want %q", ref.Address, "myhost:443")
	}
}

func TestParsePluginRefEmpty(t *testing.T) {
	_, err := pkg.ParsePluginRef("")
	if err == nil {
		t.Fatal("expected error for empty plugin ref")
	}
}

func TestParsePluginRefWhitespace(t *testing.T) {
	ref, err := pkg.ParsePluginRef("  ./my-plugin  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Raw != "./my-plugin" {
		t.Errorf("Raw = %q, want %q", ref.Raw, "./my-plugin")
	}
	if ref.Mode != pkg.ModeBinary {
		t.Errorf("Mode = %v, want %v", ref.Mode, pkg.ModeBinary)
	}
}
