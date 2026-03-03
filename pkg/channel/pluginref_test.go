package channel

import (
	"testing"
)

func TestDetectMode(t *testing.T) {
	tests := []struct {
		input string
		want  PluginMode
	}{
		{"grpc://localhost:50051", ModeGRPC},
		{"docker://myimage:latest", ModeDocker},
		{"http://example.com/hook", ModeWebhook},
		{"https://example.com/hook", ModeWebhook},
		{"ws://example.com/ws", ModeWebSocket},
		{"wss://example.com/ws", ModeWebSocket},
		{"./channels/slack/channel.yaml", ModeYAML},
		{"./channels/slack/channel.yml", ModeYAML},
		{"/absolute/path/channel.YAML", ModeYAML},
		{"./binary", ModeBinary},
		{"/usr/local/bin/channel", ModeBinary},
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

func TestModeYAMLString(t *testing.T) {
	if ModeYAML.String() != "yaml" {
		t.Errorf("ModeYAML.String() = %q, want %q", ModeYAML.String(), "yaml")
	}
}
