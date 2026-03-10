package channel

import (
	"testing"
)

func TestNavigatePath(t *testing.T) {
	obj := map[string]interface{}{
		"payload": map[string]interface{}{
			"event": map[string]interface{}{
				"type":    "message",
				"channel": "C123",
				"text":    "hello",
			},
		},
		"top_level": "value",
	}

	tests := []struct {
		name    string
		path    string
		wantNil bool
		wantKey string
		wantVal string
	}{
		{
			name:    "empty path returns root",
			path:    "",
			wantNil: false,
			wantKey: "top_level",
			wantVal: "value",
		},
		{
			name:    "nested path",
			path:    "payload.event",
			wantNil: false,
			wantKey: "channel",
			wantVal: "C123",
		},
		{
			name:    "single level",
			path:    "payload",
			wantNil: false,
		},
		{
			name:    "nonexistent path",
			path:    "payload.nonexistent",
			wantNil: true,
		},
		{
			name:    "path to non-map value",
			path:    "top_level",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := navigatePath(obj, tt.path)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if tt.wantKey != "" {
				val, ok := result[tt.wantKey]
				if !ok {
					t.Errorf("key %q not found in result", tt.wantKey)
				} else if val != tt.wantVal {
					t.Errorf("result[%q] = %v, want %q", tt.wantKey, val, tt.wantVal)
				}
			}
		})
	}
}

func TestGetStringField(t *testing.T) {
	m := map[string]interface{}{
		"str":    "hello",
		"num":    float64(42),
		"bool":   true,
		"nil":    nil,
		"nested": map[string]interface{}{"a": "b"},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"str", "hello"},
		{"num", "42"},
		{"bool", "true"},
		{"nil", ""},
		{"missing", ""},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := getStringField(m, tt.key)
			if got != tt.want {
				t.Errorf("getStringField(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestFlattenToStringMap(t *testing.T) {
	m := map[string]interface{}{
		"str":  "hello",
		"num":  float64(3.14),
		"bool": false,
		"nil":  nil,
	}

	result := flattenToStringMap(m)

	if result["str"] != "hello" {
		t.Errorf("str = %q, want %q", result["str"], "hello")
	}
	if result["num"] != "3.14" {
		t.Errorf("num = %q, want %q", result["num"], "3.14")
	}
	if result["bool"] != "false" {
		t.Errorf("bool = %q, want %q", result["bool"], "false")
	}
	if result["nil"] != "" {
		t.Errorf("nil = %q, want empty", result["nil"])
	}
}

func TestShouldSkip(t *testing.T) {
	notEmpty := true

	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Skip: []SkipRule{
					{Field: "user", Equals: "{{self.bot_id}}"},
					{Field: "bot_id", NotEmpty: &notEmpty},
					{Field: "subtype", NotEmpty: &notEmpty, Except: []string{"thread_broadcast"}},
				},
			},
		},
		selfVars: map[string]string{"bot_id": "UBOT"},
		config:   make(map[string]string),
	}

	tests := []struct {
		name  string
		event map[string]interface{}
		want  bool
	}{
		{
			name:  "skip bot user",
			event: map[string]interface{}{"user": "UBOT", "text": "hi"},
			want:  true,
		},
		{
			name:  "allow regular user",
			event: map[string]interface{}{"user": "U123", "text": "hi"},
			want:  false,
		},
		{
			name:  "skip bot_id not empty",
			event: map[string]interface{}{"user": "U123", "bot_id": "B123"},
			want:  true,
		},
		{
			name:  "allow empty bot_id",
			event: map[string]interface{}{"user": "U123"},
			want:  false,
		},
		{
			name:  "skip subtype not empty",
			event: map[string]interface{}{"user": "U123", "subtype": "channel_join"},
			want:  true,
		},
		{
			name:  "allow thread_broadcast exception",
			event: map[string]interface{}{"user": "U123", "subtype": "thread_broadcast"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.shouldSkip(tt.event)
			if got != tt.want {
				t.Errorf("shouldSkip = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldProcess(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				EventTypes: []string{"message", "app_mention"},
				AlwaysProcessWhen: &FieldMatch{
					Field:  "channel_type",
					Equals: "im",
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	tests := []struct {
		name      string
		event     map[string]interface{}
		eventType string
		want      bool
	}{
		{
			name:      "matching event type",
			event:     map[string]interface{}{},
			eventType: "message",
			want:      true,
		},
		{
			name:      "non-matching event type",
			event:     map[string]interface{}{},
			eventType: "reaction_added",
			want:      false,
		},
		{
			name:      "always_process_when match",
			event:     map[string]interface{}{"channel_type": "im"},
			eventType: "unknown_type",
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ch.shouldProcess(tt.event, tt.eventType)
			if got != tt.want {
				t.Errorf("shouldProcess = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyTransforms(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Transforms: []Transform{
					{Type: "replace", Pattern: "<@{{self.bot_id}}>", Replacement: ""},
					{Type: "trim"},
				},
			},
		},
		selfVars: map[string]string{"bot_id": "UBOT"},
		config:   make(map[string]string),
	}

	input := "  <@UBOT> hello world  "
	got := ch.applyTransforms(input, map[string]string{})
	want := "hello world"
	if got != want {
		t.Errorf("applyTransforms = %q, want %q", got, want)
	}
}

func TestApplyTransforms_RegexReplace(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Transforms: []Transform{
					{Type: "replace", Pattern: `<at>[^<]*</at>\s*`, Replacement: "", Regex: true},
					{Type: "trim"},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	tests := []struct {
		input string
		want  string
	}{
		{"<at>MyBot</at> hello there", "hello there"},
		{"<at>My Bot Name</at>   please help", "please help"},
		{"no mention here", "no mention here"},
		{"  <at>Bot</at> ", ""},
	}

	for _, tt := range tests {
		got := ch.applyTransforms(tt.input, map[string]string{})
		if got != tt.want {
			t.Errorf("applyTransforms(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestApplyTransforms_InvalidRegex(t *testing.T) {
	// An invalid regex should log and skip the transform, not panic.
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			Inbound: InboundSpec{
				Transforms: []Transform{
					{Type: "replace", Pattern: `[invalid`, Replacement: "", Regex: true},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	input := "unchanged"
	got := ch.applyTransforms(input, map[string]string{})
	if got != input {
		t.Errorf("applyTransforms with invalid regex changed content: got %q, want %q", got, input)
	}
}

func TestExtractMessage(t *testing.T) {
	ch := &YAMLChannel{
		spec: &YAMLChannelSpec{
			ID: "test",
			Inbound: InboundSpec{
				Mapping: MappingSpec{
					ConversationID: MappingField{Field: "channel"},
					SenderID:       MappingField{Field: "user"},
					Content:        MappingField{Field: "text"},
					ThreadID:       MappingField{Field: "thread_ts", Fallback: "ts"},
					Metadata:       map[string]string{"ts": "ts"},
				},
			},
		},
		selfVars: make(map[string]string),
		config:   make(map[string]string),
	}

	event := map[string]interface{}{
		"channel": "C123",
		"user":    "U456",
		"text":    "hello",
		"ts":      "1234567890.123456",
	}
	eventCtx := flattenToStringMap(event)

	msg := ch.extractMessage(event, eventCtx)

	if msg.ChannelID != "test" {
		t.Errorf("ChannelID = %q, want %q", msg.ChannelID, "test")
	}
	if msg.ConversationID != "C123" {
		t.Errorf("ConversationID = %q, want %q", msg.ConversationID, "C123")
	}
	if msg.SenderID != "U456" {
		t.Errorf("SenderID = %q, want %q", msg.SenderID, "U456")
	}
	if msg.Content != "hello" {
		t.Errorf("Content = %q, want %q", msg.Content, "hello")
	}
	// thread_ts is empty, should fall back to ts
	if msg.ThreadID != "1234567890.123456" {
		t.Errorf("ThreadID = %q, want %q (fallback to ts)", msg.ThreadID, "1234567890.123456")
	}
	if msg.Metadata["ts"] != "1234567890.123456" {
		t.Errorf("Metadata[ts] = %q, want %q", msg.Metadata["ts"], "1234567890.123456")
	}
}
