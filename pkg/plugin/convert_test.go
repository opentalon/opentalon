package plugin

import (
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

func TestRequestFromProto_CredentialHeaders(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{
		Id:     "r1",
		Plugin: "mcp",
		Action: "call",
		CredentialHeaders: map[string]*pluginpb.CredentialHeader{
			"myapp": {Header: "X-App-User", Value: "user-123"},
			"jira":  {Header: "Authorization", Value: "Bearer jira-xyz"},
		},
	}
	req := requestFromProto(pb)

	if c := req.CredentialHeaders["myapp"]; c.Header != "X-App-User" || c.Value != "user-123" {
		t.Errorf("CredentialHeaders[myapp] = %+v, want {X-App-User user-123}", c)
	}
	if c := req.CredentialHeaders["jira"]; c.Header != "Authorization" || c.Value != "Bearer jira-xyz" {
		t.Errorf("CredentialHeaders[jira] = %+v, want {Authorization Bearer jira-xyz}", c)
	}
}

func TestRequestFromProto_NoCredentialHeaders(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{Id: "r2", Plugin: "mcp", Action: "call"}
	req := requestFromProto(pb)
	if len(req.CredentialHeaders) != 0 {
		t.Errorf("CredentialHeaders = %v, want empty", req.CredentialHeaders)
	}
}

func TestRequestFromProto_NilProto(t *testing.T) {
	req := requestFromProto(nil)
	if req.Method != "" || req.ID != "" || req.CredentialHeaders != nil {
		t.Errorf("nil proto should return zero Request, got %+v", req)
	}
}

func TestRequestFromProto_CredentialHeadersWithArgs(t *testing.T) {
	pb := &pluginpb.ToolCallRequest{
		Id:     "r3",
		Plugin: "mcp",
		Action: "search",
		Args:   map[string]string{"query": "hello"},
		CredentialHeaders: map[string]*pluginpb.CredentialHeader{
			"myapp": {Header: "X-App-User", Value: "u1"},
		},
	}
	req := requestFromProto(pb)

	if req.Args["query"] != "hello" {
		t.Errorf("Args[query] = %q, want hello", req.Args["query"])
	}
	if c := req.CredentialHeaders["myapp"]; c.Header != "X-App-User" || c.Value != "u1" {
		t.Errorf("CredentialHeaders[myapp] = %+v, want {X-App-User u1}", c)
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

func TestCapsToProto_AlwaysInclude(t *testing.T) {
	// Locks in the AlwaysInclude flow plugin SDK -> proto. Previously the
	// flag existed in the proto + the host-side `orchestrator.Action`
	// type, but `pkg/plugin.ActionMsg` and `capsToProto` did not surface
	// it — so an external plugin built on this SDK had no way to declare
	// AlwaysInclude=true. The host therefore could never pin a plugin-
	// supplied action to Tier 0, regardless of what the plugin intended.
	msg := CapabilitiesMsg{
		Name:        "myplugin",
		Description: "Test",
		Actions: []ActionMsg{
			{Name: "critical_action", Description: "Pinned", AlwaysInclude: true},
			{Name: "regular_action", Description: "Normal"},
		},
	}
	pb := capsToProto(msg)

	if !pb.Actions[0].AlwaysInclude {
		t.Error("critical_action should propagate AlwaysInclude=true to proto")
	}
	if pb.Actions[1].AlwaysInclude {
		t.Error("regular_action should default to AlwaysInclude=false")
	}
}

func TestCapsToProto_ReadOnly(t *testing.T) {
	// Locks in the ReadOnly flow plugin SDK -> proto. The host short-
	// circuits the confirmation gate when this is set: read_only actions
	// execute without an "I'm about to execute X" prompt. Default is
	// false so any action that doesn't opt in stays gated.
	msg := CapabilitiesMsg{
		Name:        "myplugin",
		Description: "Test",
		Actions: []ActionMsg{
			{Name: "list_things", Description: "Pure query", ReadOnly: true},
			{Name: "delete_thing", Description: "Mutation"},
		},
	}
	pb := capsToProto(msg)

	if !pb.Actions[0].ReadOnly {
		t.Error("list_things should propagate ReadOnly=true to proto")
	}
	if pb.Actions[1].ReadOnly {
		t.Error("delete_thing should default to ReadOnly=false")
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

// TestResponseToProto_StructuredContent verifies that the structured
// payload travels alongside the textual content over the gRPC boundary.
func TestResponseToProto_StructuredContent(t *testing.T) {
	r := Response{
		CallID:            "call-1",
		Content:           "Items: 1 total",
		StructuredContent: `{"items":[{"id":42}]}`,
	}
	pb := responseToProto(r)
	if pb.Content != r.Content {
		t.Errorf("Content = %q, want %q", pb.Content, r.Content)
	}
	if pb.StructuredContent != r.StructuredContent {
		t.Errorf("StructuredContent = %q, want %q", pb.StructuredContent, r.StructuredContent)
	}
}

// TestResponseToProto_OmittedStructuredContent guards backwards compat:
// a plugin that doesn't set StructuredContent must produce a proto with
// the field empty so old hosts decode it as a no-op.
func TestResponseToProto_OmittedStructuredContent(t *testing.T) {
	r := Response{CallID: "call-1", Content: "ok"}
	pb := responseToProto(r)
	if pb.StructuredContent != "" {
		t.Errorf("StructuredContent should be empty when unset, got %q", pb.StructuredContent)
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
