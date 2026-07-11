package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/state"
)

// buildToolDefinitions must emit a JSON-Schema-valid `required`: present as an
// array when a tool has required params, and OMITTED (never `null`) when it has
// none. A nil []string serialized as `"required": null` is exactly what strict
// providers (Mistral) reject with 400 invalid_request_tool_schema. Guards the
// omit-when-empty fix, which is otherwise only verified by an external 400.
func TestBuildToolDefinitions_RequiredKeyShape(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "schema-plugin", Description: "schema shape fixtures",
		Actions: []Action{
			{
				Name: "with-required", Description: "has a required param", AlwaysInclude: true,
				Parameters: []Parameter{
					{Name: "q", Description: "required", Required: true},
					{Name: "opt", Description: "optional", Required: false},
				},
			},
			{
				Name: "no-required", Description: "only optional params", AlwaysInclude: true,
				Parameters: []Parameter{
					{Name: "opt", Description: "optional", Required: false},
				},
			},
		},
	}, &fixedResultExecutor{content: "ok"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	sessions := state.NewSessionStore("")
	sessions.Create("s1", "", "", "")
	orch := NewWithRules(nativeToolsLLM{&fakeLLM{}}, &fakeParser{}, registry,
		state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	ctx := actor.WithSessionID(context.Background(), "s1")

	params := map[string]map[string]interface{}{}
	for _, td := range orch.buildToolDefinitions(ctx) {
		params[td.Name] = td.Parameters
	}

	// Tool WITH a required param: `required` present as a []string.
	withReq, ok := params[toolFQN("schema-plugin", "with-required")]
	if !ok {
		t.Fatalf("with-required tool missing from the tools array")
	}
	req, present := withReq["required"]
	if !present {
		t.Fatalf("with-required: expected a `required` key, got %#v", withReq)
	}
	if arr, isArr := req.([]string); !isArr || len(arr) != 1 || arr[0] != "q" {
		t.Fatalf(`with-required: expected required=["q"], got %#v`, req)
	}

	// Tool with NO required params: `required` key omitted entirely (never null).
	noReq, ok := params[toolFQN("schema-plugin", "no-required")]
	if !ok {
		t.Fatalf("no-required tool missing from the tools array")
	}
	if v, present := noReq["required"]; present {
		t.Fatalf("no-required: `required` must be omitted when empty (not null/[]), got %#v", v)
	}
}
