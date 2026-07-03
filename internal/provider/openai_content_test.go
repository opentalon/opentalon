package provider

import (
	"encoding/json"
	"testing"
)

// oaiContent must accept both the plain-string content form (OpenAI, OVH
// gpt-oss) and the array-of-parts form Mistral returns when reasoning is on,
// and always marshal back to a plain JSON string.
func TestOaiContentUnmarshal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain string", `"hello world"`, "hello world"},
		{"null", `null`, ""},
		{"empty string", `""`, ""},
		{
			"array text only",
			`[{"type":"text","text":"the answer"}]`,
			"the answer",
		},
		{
			"array thinking only (Mistral tool-call turn)",
			`[{"type":"thinking","thinking":[{"type":"text","text":"cot"}],"closed":true}]`,
			"",
		},
		{
			"array thinking then text",
			`[{"type":"thinking","thinking":[{"type":"text","text":"cot"}]},{"type":"text","text":"final"}]`,
			"final",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c oaiContent
			if err := json.Unmarshal([]byte(tc.in), &c); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			if string(c) != tc.want {
				t.Fatalf("got %q, want %q", string(c), tc.want)
			}
		})
	}
}

// The streaming delta shares oaiContent, so a chunk whose delta.content is the
// array form decodes to text instead of being dropped as a malformed chunk.
func TestOaiStreamDeltaContentArray(t *testing.T) {
	const chunk = `{"choices":[{"index":0,"delta":{"role":"assistant","content":[{"type":"text","text":"Hallo"}]}}]}`
	var c oaiStreamChunk
	if err := json.Unmarshal([]byte(chunk), &c); err != nil {
		t.Fatalf("unmarshal stream chunk: %v", err)
	}
	if len(c.Choices) != 1 || string(c.Choices[0].Delta.Content) != "Hallo" {
		t.Fatalf("got %q, want %q", string(c.Choices[0].Delta.Content), "Hallo")
	}
}

// A message round-trips through a request body as a plain JSON string
// regardless of how Content was populated.
func TestOaiContentMarshalIsString(t *testing.T) {
	msg := oaiMessage{Role: "assistant", Content: oaiContent("hi")}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if len(probe.Content) == 0 || probe.Content[0] != '"' {
		t.Fatalf("content not serialized as JSON string: %s", probe.Content)
	}
}
