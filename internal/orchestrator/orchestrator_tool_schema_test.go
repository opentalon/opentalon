package orchestrator

import (
	"context"
	"encoding/json"
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

// TestBuildToolDefinitions_SuppliedSchemaFragment covers what a bare type name
// cannot say. An enum is the sharp case: a parameter whose allowed values live
// only in its enum is a parameter the model has to guess unless the fragment
// reaches it intact.
//
// The `limit` case is the one that keeps the two branches from becoming
// parallel paths: a usable fragment wins outright, even where it contradicts
// the parameter's own Type. The `broken` and `dangling` cases are the other
// direction — a fragment the host cannot safely announce falls back rather
// than reaching the provider.
func TestBuildToolDefinitions_SuppliedSchemaFragment(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(PluginCapability{
		Name: "fragment-plugin", Description: "supplied schema fixtures",
		Actions: []Action{
			{
				Name: "list", Description: "parameters with supplied fragments", AlwaysInclude: true,
				Parameters: []Parameter{
					{
						Name: "kind", Description: "ignored in favour of the fragment", Type: "string",
						Schema: `{"type":"string","enum":["asset","consumable"],"description":"Which kind"}`,
					},
					{
						Name: "tags", Description: "ignored in favour of the fragment", Type: "json",
						Schema: `{"type":"array","items":{"type":"string"},"description":"Filters"}`,
					},
					{
						// Fragment and Type disagree — the fragment decides.
						Name: "limit", Description: "how many", Type: "string",
						Schema: `{"type":"integer","maximum":500}`,
					},
					{
						// Not an object, so not a schema: falls back, never emitted.
						Name: "broken", Description: "malformed fragment", Type: "boolean",
						Schema: `"just a string"`,
					},
					{
						// The $defs it points at cannot travel with a
						// per-parameter fragment, so the reference would
						// dangle: falls back rather than reaching the provider.
						Name: "dangling", Description: "references a definition", Type: "object",
						Schema: `{"type":"object","properties":{"a":{"$ref":"#/$defs/Filter"}}}`,
					},
					{Name: "plain", Description: "no fragment supplied", Type: "integer"},
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
		if td.Name == toolFQN("fragment-plugin", "list") {
			props, _ = td.Parameters["properties"].(map[string]interface{})
		}
	}
	if props == nil {
		t.Fatalf("list tool missing from the tools array, or its properties are not an object")
	}

	// Assert on the marshalled form: that is what the provider actually sends.
	want := map[string]string{
		"kind":     `{"description":"Which kind","enum":["asset","consumable"],"type":"string"}`,
		"tags":     `{"description":"Filters","items":{"type":"string"},"type":"array"}`,
		"limit":    `{"maximum":500,"type":"integer"}`,
		"broken":   `{"description":"malformed fragment","type":"boolean"}`,
		"dangling": `{"description":"references a definition","type":"object"}`,
		"plain":    `{"description":"no fragment supplied","type":"integer"}`,
	}
	for name, wantJSON := range want {
		prop, ok := props[name]
		if !ok {
			t.Errorf("property %q missing from the emitted schema", name)
			continue
		}
		got, err := json.Marshal(prop)
		if err != nil {
			t.Errorf("property %q does not marshal: %v", name, err)
			continue
		}
		if string(got) != wantJSON {
			t.Errorf("property %q = %s, want %s", name, got, wantJSON)
		}
	}
}

// TestDecodeSchemaFragment pins what counts as a usable fragment. Everything
// rejected here falls back to the synthesised {type, description} form, and
// everything rejected has the same reason: a provider answers a schema it
// cannot read by rejecting the whole tools array, not the one property.
func TestUsableSchemaFragment(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		// Shape.
		{"object", `{"type":"string"}`, true},
		{"empty object", `{}`, true},
		{"leading whitespace", "  \n{\"type\":\"string\"}\n", true},
		{"nothing supplied", "", false},
		{"blank", "   ", false},
		{"json null", `null`, false},
		{"bare true", `true`, false},
		{"bare string", `"string"`, false},
		{"array", `[{"type":"string"}]`, false},
		{"malformed", `{"type":`, false},
		{"trailing content", `{"type":"string"} {"type":"integer"}`, false},

		// Type, held to the same seven names as the synthesised branch.
		{"no type at all", `{"enum":["a","b"]}`, true},
		{"nullable union", `{"type":["string","null"]}`, true},
		{"plugin shorthand", `{"type":"json"}`, false},
		{"unknown name in a union", `{"type":["string","json"]}`, false},
		{"empty union", `{"type":[]}`, false},
		{"non-string union member", `{"type":["string",7]}`, false},
		{"type is an object", `{"type":{"$data":"/x"}}`, false},
		// An explicit null is not the same as an absent keyword: JSON Schema
		// has no such form, so it must not be read as "no type declared" and
		// shipped as `"type": null`.
		{"type is explicitly null", `{"type":null,"enum":["a"]}`, false},
		// The check reaches the fragment's own keyword only. A nested map is
		// not necessarily a schema, so walking into it would reject valid
		// fragments over values that were never type declarations.
		{"nested type is not inspected", `{"type":"array","items":{"type":"json"}}`, true},
		{"a type inside a default value is data", `{"type":"object","default":{"type":"whatever"}}`, true},

		// References, which have no $defs to resolve against once the
		// fragment is on its own.
		{"top-level ref", `{"$ref":"#/$defs/Filter"}`, false},
		{"ref inside items", `{"type":"array","items":{"$ref":"#/$defs/Tag"}}`, false},
		{"ref nested in properties", `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"$ref":"#/$defs/X"}}}}}`, false},
		{"ref inside a list keyword", `{"anyOf":[{"type":"string"},{"$ref":"#/$defs/X"}]}`, false},
		{"dynamic ref", `{"$dynamicRef":"#node"}`, false},
		{"recursive ref", `{"$recursiveRef":"#"}`, false},
		{"the word ref in prose is fine", `{"type":"string","description":"pass a $ref here"}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frag, ok := usableSchemaFragment(c.raw)
			if ok != c.ok {
				t.Fatalf("usableSchemaFragment(%q) ok = %v, want %v", c.raw, ok, c.ok)
			}
			if !ok && frag != nil {
				t.Errorf("rejected fragment must come back nil, got %#v", frag)
			}
		})
	}

	// A large integer constraint must not be rounded through float64 on its
	// way to the provider.
	frag, ok := usableSchemaFragment(`{"type":"integer","maximum":10000000000000001}`)
	if !ok {
		t.Fatal("expected the fragment to be accepted")
	}
	got, err := json.Marshal(frag)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"maximum":10000000000000001,"type":"integer"}`; string(got) != want {
		t.Errorf("re-marshalled = %s, want %s", got, want)
	}
}

// TestParameterSchema_ReportsWhichBranchRan pins the signal buildToolDefinitions
// logs on. A plugin that supplies an unusable fragment and one that supplies
// none produce the same schema, and only this bool tells them apart — without
// it, a dropped enum looks exactly like a parameter that never had one.
func TestParameterSchema_ReportsWhichBranchRan(t *testing.T) {
	if _, fromPlugin := parameterSchema(Parameter{
		Name: "kind", Description: "which kind", Type: "string",
		Schema: `{"type":"string","enum":["asset"]}`,
	}); !fromPlugin {
		t.Error("a usable fragment must report that the plugin's schema was used")
	}
	if _, fromPlugin := parameterSchema(Parameter{
		Name: "kind", Description: "which kind", Type: "string",
		Schema: `{"type":"json"}`,
	}); fromPlugin {
		t.Error("an unusable fragment must report that the host fell back")
	}
	if _, fromPlugin := parameterSchema(Parameter{
		Name: "kind", Description: "which kind", Type: "string",
	}); fromPlugin {
		t.Error("no fragment at all must report that the host fell back")
	}
}
