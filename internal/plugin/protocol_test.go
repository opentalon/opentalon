package plugin

import (
	"net"
	"testing"
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
		hs, err := ParseHandshake(tc.input)
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
		"2|unix|/tmp/x.sock",    // wrong version
		"1|http|localhost:8080", // unsupported network
		"1|unix",                // missing address
	}
	for _, input := range bad {
		_, err := ParseHandshake(input)
		if err == nil {
			t.Errorf("ParseHandshake(%q): expected error", input)
		}
	}
}

func TestHandshakeString(t *testing.T) {
	hs := Handshake{Version: 1, Network: "unix", Address: "/tmp/p.sock"}
	got := hs.String()
	if got != "1|unix|/tmp/p.sock" {
		t.Errorf("String() = %q", got)
	}
}

func TestWriteReadMessage(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	sent := Request{Method: "execute", ID: "call-1", Plugin: "gitlab", Action: "list_mrs", Args: map[string]string{"project": "myapp"}}

	errCh := make(chan error, 1)
	go func() {
		errCh <- WriteMessage(client, &sent)
	}()

	var received Request
	if err := ReadMessage(server, &received); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}

	if received.Method != "execute" {
		t.Errorf("method = %q", received.Method)
	}
	if received.ID != "call-1" {
		t.Errorf("id = %q", received.ID)
	}
	if received.Plugin != "gitlab" {
		t.Errorf("plugin = %q", received.Plugin)
	}
	if received.Action != "list_mrs" {
		t.Errorf("action = %q", received.Action)
	}
	if received.Args["project"] != "myapp" {
		t.Errorf("args[project] = %q", received.Args["project"])
	}
}

func TestWriteReadResponse(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	sent := Response{
		CallID:  "call-1",
		Content: "merge request created",
	}

	go func() { _ = WriteMessage(server, &sent) }()

	var received Response
	if err := ReadMessage(client, &received); err != nil {
		t.Fatal(err)
	}
	if received.CallID != "call-1" {
		t.Errorf("call_id = %q", received.CallID)
	}
	if received.Content != "merge request created" {
		t.Errorf("content = %q", received.Content)
	}
}

func TestCapabilitiesRoundTrip(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	sent := Response{
		Caps: &CapabilitiesMsg{
			Name:        "gitlab",
			Description: "GitLab integration",
			Actions: []ActionMsg{
				{
					Name:        "list_mrs",
					Description: "List merge requests",
					Parameters: []ParameterMsg{
						{Name: "project", Description: "Project ID", Type: "string", Required: true},
					},
				},
			},
		},
	}

	go func() { _ = WriteMessage(server, &sent) }()

	var received Response
	if err := ReadMessage(client, &received); err != nil {
		t.Fatal(err)
	}

	if received.Caps == nil {
		t.Fatal("caps is nil")
	}
	if received.Caps.Name != "gitlab" {
		t.Errorf("name = %q", received.Caps.Name)
	}
	if len(received.Caps.Actions) != 1 {
		t.Fatalf("actions = %d", len(received.Caps.Actions))
	}
	if received.Caps.Actions[0].Parameters[0].Required != true {
		t.Error("parameter should be required")
	}
}
