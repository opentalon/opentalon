package channel

import (
	"os"
	"testing"
)

func TestSubstituteTemplate(t *testing.T) {
	contexts := map[string]map[string]string{
		"self":   {"bot_user_id": "U123", "ws_url": "wss://example.com"},
		"config": {"ack_reaction": "eyes"},
		"event":  {"channel": "C456", "ts": "1234567890.123456"},
		"msg":    {"conversation_id": "C456", "metadata.ts": "1234567890.123456"},
	}

	tests := []struct {
		name   string
		input  string
		want   string
	}{
		{
			name:  "self namespace",
			input: "Hello {{self.bot_user_id}}",
			want:  "Hello U123",
		},
		{
			name:  "config namespace",
			input: "React with {{config.ack_reaction}}",
			want:  "React with eyes",
		},
		{
			name:  "multiple namespaces",
			input: "{{event.channel}}:{{event.ts}}",
			want:  "C456:1234567890.123456",
		},
		{
			name:  "nested metadata key",
			input: "ts={{msg.metadata.ts}}",
			want:  "ts=1234567890.123456",
		},
		{
			name:  "missing key returns empty",
			input: "val={{self.nonexistent}}",
			want:  "val=",
		},
		{
			name:  "missing namespace returns empty",
			input: "val={{unknown.key}}",
			want:  "val=",
		},
		{
			name:  "no templates",
			input: "plain string",
			want:  "plain string",
		},
		{
			name:  "JSON body with templates",
			input: `{"channel":"{{msg.conversation_id}}","name":"{{config.ack_reaction}}"}`,
			want:  `{"channel":"C456","name":"eyes"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := substituteTemplate(tt.input, contexts)
			if got != tt.want {
				t.Errorf("substituteTemplate(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSubstituteTemplateEnv(t *testing.T) {
	os.Setenv("TEST_YAML_TEMPLATE_VAR", "secret123")
	defer os.Unsetenv("TEST_YAML_TEMPLATE_VAR")

	got := substituteTemplate("Bearer {{env.TEST_YAML_TEMPLATE_VAR}}", nil)
	want := "Bearer secret123"
	if got != want {
		t.Errorf("env substitution = %q, want %q", got, want)
	}
}

func TestSubstituteTemplateEnvMissing(t *testing.T) {
	os.Unsetenv("NONEXISTENT_YAML_VAR_12345")
	got := substituteTemplate("val={{env.NONEXISTENT_YAML_VAR_12345}}", nil)
	want := "val="
	if got != want {
		t.Errorf("missing env = %q, want %q", got, want)
	}
}

func TestSubstituteTemplateJSON(t *testing.T) {
	contexts := map[string]map[string]string{
		"msg": {
			"content":    `He said "hello" and it's fine` + "\nnew line",
			"channel_id": "C123",
		},
	}

	input := `{"text":"{{msg.content}}","channel":"{{msg.channel_id}}"}`
	got := substituteTemplateJSON(input, contexts)
	want := `{"text":"He said \"hello\" and it's fine\nnew line","channel":"C123"}`
	if got != want {
		t.Errorf("substituteTemplateJSON:\ngot  %s\nwant %s", got, want)
	}
}

func TestJsonEscapeString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`hello`, `hello`},
		{`say "hi"`, `say \"hi\"`},
		{"line1\nline2", `line1\nline2`},
		{`back\slash`, `back\\slash`},
		{`tab	here`, `tab\there`},
	}
	for _, tt := range tests {
		got := jsonEscapeString(tt.input)
		if got != tt.want {
			t.Errorf("jsonEscapeString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
