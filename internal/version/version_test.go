package version

import (
	"strings"
	"testing"
)

func TestGetReturnsDefaults(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Errorf("expected Version=dev, got %s", info.Version)
	}
	if info.Commit != "none" {
		t.Errorf("expected Commit=none, got %s", info.Commit)
	}
	if info.Date != "unknown" {
		t.Errorf("expected Date=unknown, got %s", info.Date)
	}
}

func TestInfoString(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "defaults",
			info: Info{Version: "dev", Commit: "none", Date: "unknown"},
			want: "OpenTalon dev (commit: none, built: unknown)",
		},
		{
			name: "release",
			info: Info{Version: "v1.0.0", Commit: "abc1234", Date: "2026-01-01T00:00:00Z"},
			want: "OpenTalon v1.0.0 (commit: abc1234, built: 2026-01-01T00:00:00Z)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.info.String()
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInfoStringContainsAllFields(t *testing.T) {
	info := Info{Version: "test-ver", Commit: "test-commit", Date: "test-date"}
	s := info.String()
	for _, field := range []string{"test-ver", "test-commit", "test-date"} {
		if !strings.Contains(s, field) {
			t.Errorf("String() output %q missing field %q", s, field)
		}
	}
}
