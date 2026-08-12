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

// TestBuildToolDefinitions_ParameterTypes is the test whose absence let every
// parameter be announced to the model as a string no matter what its plugin
// declared: nothing fed a non-string parameter through to the tool definition
// and checked what came out.
//
// A declared JSON Schema type must survive verbatim. Two things must NOT: an
// undeclared type, and a plugin's own shorthand for a shape the wire cannot
// carry ("json"). Both become "string" — announcing "json" would make a
// provider reject the entire tools array, not just that one property.
func TestBuildToolDefinitions_ParameterTypes(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "typed-plugin", Description: "parameter type fixtures",
		Actions: []Action{
			{
				Name: "search", Description: "typed parameters", AlwaysInclude: true,
				Parameters: []Parameter{
					{Name: "query", Description: "text", Type: "string", Required: true},
					{Name: "limit", Description: "how many", Type: "integer"},
					{Name: "ratio", Description: "fraction", Type: "number"},
					{Name: "verbose", Description: "chatty", Type: "boolean"},
					{Name: "tags", Description: "filters", Type: "array"},
					{Name: "filter", Description: "nested", Type: "object"},
					{Name: "payload", Description: "plugin shorthand", Type: "json"},
					{Name: "legacy", Description: "no type declared"},
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

	var props map[string]interface{}
	for _, td := range orch.buildToolDefinitions(ctx) {
		if td.Name == toolFQN("typed-plugin", "search") {
			props, _ = td.Parameters["properties"].(map[string]interface{})
		}
	}
	if props == nil {
		t.Fatalf("search tool missing from the tools array, or its properties are not an object")
	}

	want := map[string]string{
		"query":   "string",
		"limit":   "integer",
		"ratio":   "number",
		"verbose": "boolean",
		"tags":    "array",
		"filter":  "object",
		"payload": "string", // "json" is not a JSON Schema type
		"legacy":  "string", // nothing declared
	}
	for name, wantType := range want {
		prop, ok := props[name].(map[string]interface{})
		if !ok {
			t.Errorf("property %q missing from the emitted schema", name)
			continue
		}
		if prop["type"] != wantType {
			t.Errorf("property %q type = %v, want %q", name, prop["type"], wantType)
		}
	}
}
