package config

import (
	"strings"
	"testing"
)

// TestParseCacheDiagnostics covers R8's config errors: cache_diagnostics is
// true or false only, and true needs prompt_cache: true. Each error names the
// provider.
func TestParseCacheDiagnostics(t *testing.T) {
	cases := []struct {
		name    string
		fields  string // lines under the provider, after api
		wantErr string // "" = must parse
	}{
		{"without prompt_cache", "cache_diagnostics: true", "models.providers.p.cache_diagnostics: needs prompt_cache: true"},
		{"with prompt_cache: false", "prompt_cache: false\n      cache_diagnostics: true", "models.providers.p.cache_diagnostics: needs prompt_cache: true"},
		{"yes is not a boolean", "prompt_cache: true\n      cache_diagnostics: yes", "models.providers.p.cache_diagnostics: line 6: must be true or false"},
		{"null is not a boolean", "prompt_cache: true\n      cache_diagnostics: null", "models.providers.p.cache_diagnostics: line 6: must be true or false"},
		{"with prompt_cache", "prompt_cache: true\n      cache_diagnostics: true", ""},
		{"off without prompt_cache", "cache_diagnostics: false", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "models:\n  providers:\n    p:\n      api: anthropic-messages\n      " + tc.fields + "\n"
			cfg, err := Parse([]byte(yaml))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got, want := cfg.Models.Providers["p"].CacheDiagnostics.Value, strings.Contains(tc.fields, "cache_diagnostics: true"); got != want {
					t.Errorf("cache_diagnostics = %t, want %t", got, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
