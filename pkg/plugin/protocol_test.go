package plugin_test

import (
	"testing"

	"github.com/opentalon/opentalon/pkg/plugin"
)

func TestParseHandshakeValid(t *testing.T) {
	tests := []struct {
		input    string
		version  int
		network  string
		address  string
		httpAddr string
	}{
		{"1|unix|/tmp/plug.sock", 1, "unix", "/tmp/plug.sock", ""},
		{"1|tcp|127.0.0.1:9001", 1, "tcp", "127.0.0.1:9001", ""},
		{"1|unix|/tmp/plug.sock|127.0.0.1:9091", 1, "unix", "/tmp/plug.sock", "127.0.0.1:9091"},
		{"1|tcp|127.0.0.1:9001|127.0.0.1:9091", 1, "tcp", "127.0.0.1:9001", "127.0.0.1:9091"},
		{"1|unix|/tmp/plug.sock|[::1]:9091", 1, "unix", "/tmp/plug.sock", "[::1]:9091"}, // IPv6 loopback
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			hs, err := plugin.ParseHandshake(tc.input)
			if err != nil {
				t.Fatalf("ParseHandshake(%q): %v", tc.input, err)
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
			if hs.HTTPAddr != tc.httpAddr {
				t.Errorf("http_addr = %q, want %q", hs.HTTPAddr, tc.httpAddr)
			}
		})
	}
}

func TestParseHandshakeInvalid(t *testing.T) {
	bad := []string{
		"",
		"garbage",
		"2|unix|/tmp/x.sock",                 // wrong version
		"1|http|localhost:8080",              // unsupported network
		"1|unix",                             // missing address
		"1|unix|/tmp/p.sock|bad:addr:format",  // too many colons
		"1|unix|/tmp/p.sock|noport",           // missing port
		"1|unix|/tmp/p.sock|0.0.0.0:9091",    // all-interfaces, not loopback
		"1|unix|/tmp/p.sock|:9091",            // empty host (all-interfaces), not loopback
		"1|unix|/tmp/p.sock|192.168.1.1:9091", // external host, not loopback
	}
	for _, input := range bad {
		t.Run(input, func(t *testing.T) {
			_, err := plugin.ParseHandshake(input)
			if err == nil {
				t.Errorf("ParseHandshake(%q): expected error", input)
			}
		})
	}
}

func TestHandshakeString(t *testing.T) {
	tests := []struct {
		hs   plugin.Handshake
		want string
	}{
		{
			plugin.Handshake{Version: 1, Network: "unix", Address: "/tmp/p.sock"},
			"1|unix|/tmp/p.sock",
		},
		{
			plugin.Handshake{Version: 1, Network: "tcp", Address: "127.0.0.1:9000"},
			"1|tcp|127.0.0.1:9000",
		},
		{
			plugin.Handshake{Version: 1, Network: "unix", Address: "/tmp/p.sock", HTTPAddr: "127.0.0.1:9091"},
			"1|unix|/tmp/p.sock|127.0.0.1:9091",
		},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.hs.String()
			if got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHandshakeRoundtrip(t *testing.T) {
	originals := []plugin.Handshake{
		{Version: 1, Network: "unix", Address: "/tmp/plugin.sock"},
		{Version: 1, Network: "tcp", Address: "127.0.0.1:9000"},
		{Version: 1, Network: "unix", Address: "/tmp/plugin.sock", HTTPAddr: "127.0.0.1:9091"},
	}
	for _, orig := range originals {
		t.Run(orig.String(), func(t *testing.T) {
			parsed, err := plugin.ParseHandshake(orig.String())
			if err != nil {
				t.Fatalf("ParseHandshake(%q): %v", orig.String(), err)
			}
			if parsed != orig {
				t.Errorf("roundtrip mismatch: got %+v, want %+v", parsed, orig)
			}
		})
	}
}
