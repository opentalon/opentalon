package plugin

import (
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

func TestParseHandshakeValid(t *testing.T) {
	tests := []struct {
		input   string
		version int
		network string
		address string
	}{
		{"1|unix|/tmp/plug.sock", 1, "unix", "/tmp/plug.sock"},
		{"1|tcp|127.0.0.1:9001", 1, "tcp", "127.0.0.1:9001"},
	}
	for _, tc := range tests {
		hs, err := pkg.ParseHandshake(tc.input)
		if err != nil {
			t.Errorf("ParseHandshake(%q): %v", tc.input, err)
			continue
		}
		if hs.Version != tc.version {
			t.Errorf("version = %d, want %d", hs.Version, tc.version)
		}
		if hs.Network != tc.network {
			t.Errorf("network = %q, want %q", hs.Network, tc.network)
		}
		if hs.Address != tc.address {
			t.Errorf("address = %q, want %q", hs.Address, tc.address)
		}
	}
}

func TestParseHandshakeInvalid(t *testing.T) {
	bad := []string{
		"",
		"garbage",
		"2|unix|/tmp/x.sock",   // wrong version
		"1|http|localhost:8080", // unsupported network
		"1|unix",               // missing address
	}
	for _, input := range bad {
		_, err := pkg.ParseHandshake(input)
		if err == nil {
			t.Errorf("ParseHandshake(%q): expected error", input)
		}
	}
}

func TestHandshakeString(t *testing.T) {
	hs := pkg.Handshake{Version: 1, Network: "unix", Address: "/tmp/p.sock"}
	got := hs.String()
	if got != "1|unix|/tmp/p.sock" {
		t.Errorf("String() = %q", got)
	}
}
