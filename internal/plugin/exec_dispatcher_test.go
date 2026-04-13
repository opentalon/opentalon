package plugin

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestParseExecRequestValid(t *testing.T) {
	tests := []struct {
		name       string
		values     map[string]interface{}
		wantID     string
		wantPlugin string
		wantAction string
		wantArgs   map[string]string
	}{
		{
			name: "minimal",
			values: map[string]interface{}{
				"id":     "req-1",
				"plugin": "jira",
				"action": "create_issue",
			},
			wantID:     "req-1",
			wantPlugin: "jira",
			wantAction: "create_issue",
		},
		{
			name: "with context fields",
			values: map[string]interface{}{
				"id":         "req-2",
				"plugin":     "slack",
				"action":     "post_message",
				"entity_id":  "user-abc",
				"group_id":   "acme",
				"channel_id": "C12345",
				"session_id": "sess-xyz",
			},
			wantID:     "req-2",
			wantPlugin: "slack",
			wantAction: "post_message",
		},
		{
			name: "with args JSON",
			values: map[string]interface{}{
				"id":     "req-3",
				"plugin": "jira",
				"action": "create_issue",
				"args":   `{"project":"OT","summary":"hello"}`,
			},
			wantID:     "req-3",
			wantPlugin: "jira",
			wantAction: "create_issue",
			wantArgs:   map[string]string{"project": "OT", "summary": "hello"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := redis.XMessage{ID: "0-1", Values: tc.values}
			req, err := parseExecRequest(msg)
			if err != nil {
				t.Fatalf("parseExecRequest: %v", err)
			}
			if req.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", req.ID, tc.wantID)
			}
			if req.Plugin != tc.wantPlugin {
				t.Errorf("Plugin = %q, want %q", req.Plugin, tc.wantPlugin)
			}
			if req.Action != tc.wantAction {
				t.Errorf("Action = %q, want %q", req.Action, tc.wantAction)
			}
			for k, want := range tc.wantArgs {
				if got := req.Args[k]; got != want {
					t.Errorf("Args[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestParseExecRequestInvalid(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]interface{}
	}{
		{
			name:   "missing id",
			values: map[string]interface{}{"plugin": "jira", "action": "create"},
		},
		{
			name:   "missing plugin",
			values: map[string]interface{}{"id": "req-1", "action": "create"},
		},
		{
			name:   "missing action",
			values: map[string]interface{}{"id": "req-1", "plugin": "jira"},
		},
		{
			name: "invalid args JSON",
			values: map[string]interface{}{
				"id":     "req-1",
				"plugin": "jira",
				"action": "create",
				"args":   "not-json",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := redis.XMessage{ID: "0-1", Values: tc.values}
			_, err := parseExecRequest(msg)
			if err == nil {
				t.Errorf("expected error for %q", tc.name)
			}
		})
	}
}

func TestParseExecRequestContextFields(t *testing.T) {
	msg := redis.XMessage{
		ID: "0-1",
		Values: map[string]interface{}{
			"id":         "req-ctx",
			"plugin":     "slack",
			"action":     "post",
			"entity_id":  "user-1",
			"group_id":   "org-1",
			"channel_id": "C99",
			"session_id": "sess-42",
		},
	}
	req, err := parseExecRequest(msg)
	if err != nil {
		t.Fatalf("parseExecRequest: %v", err)
	}
	if req.EntityID != "user-1" {
		t.Errorf("EntityID = %q, want %q", req.EntityID, "user-1")
	}
	if req.GroupID != "org-1" {
		t.Errorf("GroupID = %q, want %q", req.GroupID, "org-1")
	}
	if req.ChannelID != "C99" {
		t.Errorf("ChannelID = %q, want %q", req.ChannelID, "C99")
	}
	if req.SessionID != "sess-42" {
		t.Errorf("SessionID = %q, want %q", req.SessionID, "sess-42")
	}
}
