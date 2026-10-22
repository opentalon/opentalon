package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/provider"
)

// TestCacheDiagnosticsReachTheProvider checks that cache_diagnostics set in
// the YAML config reaches the Anthropic provider built by buildProviderRef:
// its chat-loop request carries the beta header and the diagnostics field.
func TestCacheDiagnosticsReachTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		want  bool
	}{
		{"on", "\n      cache_diagnostics: true", true},
		{"off", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var header http.Header
			var body map[string]json.RawMessage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				header = r.Header.Clone()
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &body)
				_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()
			cfg, err := config.Parse([]byte("models:\n  providers:\n    anth:\n      api: anthropic-messages\n      base_url: " + server.URL +
				"\n      prompt_cache: true" + tc.field + "\n      models:\n        - id: claude-x\n"))
			if err != nil {
				t.Fatal(err)
			}
			prov, _, _, err := buildProviderRef(cfg, "anth/claude-x", nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			segs := provider.SystemSegments{Fixed: "Fixed."}
			if _, err := prov.Complete(actor.WithSessionID(context.Background(), "s1"), &provider.CompletionRequest{
				Model:    "claude-x",
				Messages: []provider.Message{{Role: provider.RoleSystem, Content: segs.Join()}, {Role: provider.RoleUser, Content: "Hi"}},
				Segments: &segs,
			}); err != nil {
				t.Fatal(err)
			}
			_, diagnosed := body["diagnostics"]
			beta := header.Get("Anthropic-Beta") == "cache-diagnosis-2026-04-07"
			if diagnosed != tc.want || beta != tc.want {
				t.Errorf("diagnostics field sent = %t, beta header sent = %t, want %t", diagnosed, beta, tc.want)
			}
		})
	}
}
