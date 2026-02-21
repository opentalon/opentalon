package channel

import (
	"testing"
)

func TestDetectMode(t *testing.T) {
	tests := []struct {
		input string
		want  PluginMode
	}{
		{"./plugins/opentalon-slack", ModeBinary},
		{"/usr/local/bin/opentalon-slack", ModeBinary},
		{"plugins/my-plugin", ModeBinary},
		{"grpc://localhost:9001", ModeGRPC},
		{"grpc://telegram-bot.internal:443", ModeGRPC},
		{"GRPC://UPPER.CASE:9001", ModeGRPC},
		{"docker://ghcr.io/opentalon/plugin-teams:latest", ModeDocker},
		{"docker://my-registry/plugin:v2", ModeDocker},
		{"DOCKER://IMAGE:TAG", ModeDocker},
		{"http://localhost:8080/webhook", ModeWebhook},
		{"https://us-central1-proj.cloudfunctions.net/wa", ModeWebhook},
		{"HTTP://UPPERCASE.COM/path", ModeWebhook},
		{"ws://localhost:9090/channel", ModeWebSocket},
		{"wss://custom-bridge.example.com/channel", ModeWebSocket},
		{"WSS://UPPER.CASE/WS", ModeWebSocket},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := DetectMode(tt.input)
			if got != tt.want {
				t.Errorf("DetectMode(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectModeString(t *testing.T) {
	tests := []struct {
		mode PluginMode
		want string
	}{
		{ModeBinary, "binary"},
		{ModeGRPC, "grpc"},
		{ModeDocker, "docker"},
		{ModeWebhook, "webhook"},
		{ModeWebSocket, "websocket"},
		{PluginMode(99), "unknown"},
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
		wantMode PluginMode
		wantAddr string
	}{
		{"./plugins/slack", ModeBinary, "./plugins/slack"},
		{"grpc://host:9001", ModeGRPC, "host:9001"},
		{"docker://img:tag", ModeDocker, "img:tag"},
		{"https://example.com/hook", ModeWebhook, "https://example.com/hook"},
		{"wss://ws.example.com/ch", ModeWebSocket, "wss://ws.example.com/ch"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			mode, addr := ParsePluginAddress(tt.input)
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
	ref, err := ParsePluginRef("grpc://myhost:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Raw != "grpc://myhost:443" {
		t.Errorf("Raw = %q, want %q", ref.Raw, "grpc://myhost:443")
	}
	if ref.Mode != ModeGRPC {
		t.Errorf("Mode = %v, want %v", ref.Mode, ModeGRPC)
	}
	if ref.Address != "myhost:443" {
		t.Errorf("Address = %q, want %q", ref.Address, "myhost:443")
	}
}

func TestParsePluginRefEmpty(t *testing.T) {
	_, err := ParsePluginRef("")
	if err == nil {
		t.Fatal("expected error for empty plugin ref")
	}
}

func TestParsePluginRefWhitespace(t *testing.T) {
	ref, err := ParsePluginRef("  ./my-plugin  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ref.Raw != "./my-plugin" {
		t.Errorf("Raw = %q, want %q", ref.Raw, "./my-plugin")
	}
	if ref.Mode != ModeBinary {
		t.Errorf("Mode = %v, want %v", ref.Mode, ModeBinary)
	}
}
