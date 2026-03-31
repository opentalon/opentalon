package plugin

import "testing"

func TestConfigJSON(t *testing.T) {
	tests := []struct {
		name  string
		entry PluginEntry
		want  string
	}{
		{
			name:  "nil config returns empty JSON object",
			entry: PluginEntry{Name: "test"},
			want:  "{}",
		},
		{
			name:  "empty config map returns empty JSON object",
			entry: PluginEntry{Name: "test", Config: map[string]interface{}{}},
			want:  "{}",
		},
		{
			name: "config with values returns JSON",
			entry: PluginEntry{
				Name: "mcp",
				Config: map[string]interface{}{
					"servers": []interface{}{
						map[string]interface{}{
							"name": "myserver",
							"url":  "https://example.com/mcp",
						},
					},
				},
			},
			want: `{"servers":[{"name":"myserver","url":"https://example.com/mcp"}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configJSON(tt.entry)
			if got != tt.want {
				t.Errorf("configJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}
