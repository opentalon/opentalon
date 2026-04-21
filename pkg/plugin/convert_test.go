package plugin

import (
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

func TestRequestFromProto_Credentials(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{
		Id:     "r1",
		Plugin: "mcp",
		Action: "call",
		Credentials: map[string]string{
			"mymcp": "user-token-abc",
			"jira":  "user-jira-xyz",
		},
	}
	req := requestFromProto(pb)

	if req.Credentials["mymcp"] != "user-token-abc" {
		t.Errorf("Credentials[mymcp] = %q, want user-token-abc", req.Credentials["mymcp"])
	}
	if req.Credentials["jira"] != "user-jira-xyz" {
		t.Errorf("Credentials[jira] = %q, want user-jira-xyz", req.Credentials["jira"])
	}
}

func TestRequestFromProto_NoCredentials(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{Id: "r2", Plugin: "mcp", Action: "call"}
	req := requestFromProto(pb)
	if len(req.Credentials) != 0 {
		t.Errorf("Credentials = %v, want empty", req.Credentials)
	}
}

func TestRequestFromProto_NilProto(t *testing.T) {
	req := requestFromProto(nil)
	if req.Method != "" || req.ID != "" || req.Credentials != nil {
		t.Errorf("nil proto should return zero Request, got %+v", req)
	}
}

func TestRequestFromProto_CredentialsWithArgs(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{
		Id:     "r3",
		Plugin: "mcp",
		Action: "search",
		Args:   map[string]string{"query": "hello"},
		Credentials: map[string]string{
			"mymcp": "tok",
		},
	}
	req := requestFromProto(pb)

	if req.Args["query"] != "hello" {
		t.Errorf("Args[query] = %q, want hello", req.Args["query"])
	}
	if req.Credentials["mymcp"] != "tok" {
		t.Errorf("Credentials[mymcp] = %q, want tok", req.Credentials["mymcp"])
	}
}

func TestCapsToProto_InjectContextArgs(t *testing.T) {
	msg := CapabilitiesMsg{
		Name:        "myplugin",
		Description: "Test",
		Actions: []ActionMsg{
			{
				Name:              "save_cred",
				Description:       "Save credentials",
				InjectContextArgs: []string{"actor_id"},
			},
			{
				Name:        "navigate",
				Description: "Navigate",
			},
		},
	}
	pb := capsToProto(msg)

	if len(pb.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(pb.Actions))
	}
	if len(pb.Actions[0].InjectContextArgs) != 1 || pb.Actions[0].InjectContextArgs[0] != "actor_id" {
		t.Errorf("InjectContextArgs = %v, want [actor_id]", pb.Actions[0].InjectContextArgs)
	}
	if len(pb.Actions[1].InjectContextArgs) != 0 {
		t.Errorf("InjectContextArgs should be empty for navigate, got %v", pb.Actions[1].InjectContextArgs)
	}
}

func TestCapsToProto_UserOnly(t *testing.T) {
	msg := CapabilitiesMsg{
		Name:        "myplugin",
		Description: "Test",
		Actions: []ActionMsg{
			{Name: "admin_action", Description: "Admin", UserOnly: true},
			{Name: "public_action", Description: "Public", UserOnly: false},
		},
	}
	pb := capsToProto(msg)

	if len(pb.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(pb.Actions))
	}
	if !pb.Actions[0].UserOnly {
		t.Error("admin_action should have UserOnly=true")
	}
	if pb.Actions[1].UserOnly {
		t.Error("public_action should have UserOnly=false")
	}
}

func TestCapsToProto_Parameters(t *testing.T) {
	msg := CapabilitiesMsg{
		Name:        "myplugin",
		Description: "Test",
		Actions: []ActionMsg{
			{
				Name:        "act",
				Description: "Action with params",
				Parameters: []ParameterMsg{
					{Name: "url", Description: "URL", Type: "string", Required: true},
					{Name: "selector", Description: "Selector", Type: "string", Required: false},
				},
			},
		},
	}
	pb := capsToProto(msg)

	if len(pb.Actions[0].Parameters) != 2 {
		t.Fatalf("expected 2 parameters, got %d", len(pb.Actions[0].Parameters))
	}
	if !pb.Actions[0].Parameters[0].Required {
		t.Error("url parameter should be required")
	}
	if pb.Actions[0].Parameters[1].Required {
		t.Error("selector parameter should not be required")
	}
}

func TestCapsToProto_Roundtrip_Name(t *testing.T) {
	msg := CapabilitiesMsg{
		Name:                 "myplugin",
		Description:          "A plugin",
		SystemPromptAddition: "Extra context",
	}
	pb := capsToProto(msg)

	if pb.Name != "myplugin" {
		t.Errorf("Name = %q, want myplugin", pb.Name)
	}
	if pb.Description != "A plugin" {
		t.Errorf("Description = %q, want A plugin", pb.Description)
	}
	if pb.SystemPromptAddition != "Extra context" {
		t.Errorf("SystemPromptAddition = %q, want Extra context", pb.SystemPromptAddition)
	}
}
