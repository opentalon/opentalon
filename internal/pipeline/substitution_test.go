package pipeline

import (
	"strings"
	"testing"
)

func ctxWith(stepID string, output any) *PipelineContext {
	c := NewContext()
	c.Set(stepID, "output", output)
	return c
}

// --- resolvePath / splitPath ---

func TestSplitPath(t *testing.T) {
	cases := []struct {
		in      string
		want    []any
		wantErr string // substring; "" → no error
	}{
		{"id", []any{"id"}, ""},
		{"containers.id", []any{"containers", "id"}, ""},
		{"containers[0]", []any{"containers", 0}, ""},
		{"containers[0].id", []any{"containers", 0, "id"}, ""},
		{"a.b[2].c", []any{"a", "b", 2, "c"}, ""},
		{"items[0].nested[1].deep", []any{"items", 0, "nested", 1, "deep"}, ""},
		// Errors
		{"", nil, "empty path"},
		{".id", nil, "empty segment"},
		{"id.", nil, "trailing"},
		{"items[0", nil, "unclosed"},
		{"items[]", nil, "invalid array index"},
		{"items[abc]", nil, "invalid array index"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := splitPath(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("err = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("len(got) = %d, want %d (got=%v)", len(got), len(c.want), got)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("seg[%d] = %v (%T), want %v (%T)", i, got[i], got[i], c.want[i], c.want[i])
				}
			}
		})
	}
}

func TestResolvePathTraversal(t *testing.T) {
	root := map[string]any{
		"containers": []any{
			map[string]any{"id": float64(170910), "name": "Berlin Garage"},
			map[string]any{"id": float64(170911), "name": "Munich Workshop"},
		},
		"pagination": map[string]any{"total": float64(2)},
	}
	cases := []struct {
		path string
		want any
	}{
		{"containers[0].id", float64(170910)},
		{"containers[1].name", "Munich Workshop"},
		{"pagination.total", float64(2)},
		{"containers", root["containers"]},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			got, err := resolvePath(root, c.path)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			// Slice equality via length only (deep eq is nil unless we import reflect)
			if s, ok := c.want.([]any); ok {
				gs, _ := got.([]any)
				if len(gs) != len(s) {
					t.Errorf("len = %d, want %d", len(gs), len(s))
				}
				return
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolvePathDiagnostics(t *testing.T) {
	root := map[string]any{
		"containers": []any{map[string]any{"id": float64(1)}},
	}
	cases := []struct {
		path    string
		wantErr string
	}{
		{"items[0].id", "key \"items\" not found (available: containers)"},
		{"containers[5].id", "[5] out of range (length 1)"},
		{"containers[0].id.foo", "expected object, got number"},
		{"pagination.total", "key \"pagination\" not found"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			_, err := resolvePath(root, c.path)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), c.wantErr)
			}
		})
	}
}

// --- resolveArgs / resolveValue ---

func TestResolveArgsSoloPlaceholderPreservesType(t *testing.T) {
	ctx := ctxWith("step1", map[string]any{
		"containers": []any{
			map[string]any{"id": float64(170910)},
		},
	})
	args := map[string]any{
		"container_id": "{{step1.output.containers[0].id}}",
	}
	got, err := resolveArgs(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got["container_id"].(float64); !ok || v != 170910 {
		t.Errorf("container_id = %v (%T), want float64(170910)", got["container_id"], got["container_id"])
	}
}

func TestResolveArgsInterpolation(t *testing.T) {
	ctx := ctxWith("step1", map[string]any{
		"items": []any{map[string]any{"id": float64(42), "name": "Tesla"}},
	})
	args := map[string]any{
		"label": "item-{{step1.output.items[0].id}}",
		"note":  "{{step1.output.items[0].name}} ({{step1.output.items[0].id}})",
	}
	got, err := resolveArgs(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["label"] != "item-42" {
		t.Errorf("label = %v, want 'item-42'", got["label"])
	}
	if got["note"] != "Tesla (42)" {
		t.Errorf("note = %v, want 'Tesla (42)'", got["note"])
	}
}

func TestResolveArgsLargeIntegerNoScientificNotation(t *testing.T) {
	// Regression for the planner-emitted IDs: a Timly item id like 2_037_838
	// becomes float64 after JSON unmarshal and would render as
	// "2.037838e+06" with a default fmt.Sprintf("%v", v). That broke the
	// downstream MCP server before the boundary stringification was fixed.
	ctx := ctxWith("step1", map[string]any{
		"items": []any{map[string]any{"id": float64(2037838)}},
	})
	args := map[string]any{
		"label": "item-{{step1.output.items[0].id}}",
	}
	got, err := resolveArgs(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["label"] != "item-2037838" {
		t.Errorf("label = %v, want 'item-2037838' (no scientific notation)", got["label"])
	}
}

func TestResolveArgsNoPlaceholders(t *testing.T) {
	ctx := NewContext()
	args := map[string]any{"a": "literal", "b": float64(5), "c": true}
	got, err := resolveArgs(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "literal" || got["b"] != float64(5) || got["c"] != true {
		t.Errorf("non-placeholder args mutated: %v", got)
	}
}

func TestResolveArgsNestedValues(t *testing.T) {
	// The planner can emit array / object values containing placeholders.
	// resolveValue must recurse into them.
	ctx := ctxWith("step1", map[string]any{"id": float64(7)})
	args := map[string]any{
		"include": []any{"name", "{{step1.output.id}}"},
		"filter":  map[string]any{"id": "{{step1.output.id}}"},
	}
	got, err := resolveArgs(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	inc := got["include"].([]any)
	if inc[1].(float64) != 7 {
		t.Errorf("include[1] = %v, want float64(7)", inc[1])
	}
	flt := got["filter"].(map[string]any)
	if flt["id"].(float64) != 7 {
		t.Errorf("filter.id = %v, want float64(7)", flt["id"])
	}
}

func TestResolveArgsErrorPaths(t *testing.T) {
	ctx := ctxWith("step1", map[string]any{
		"containers": []any{map[string]any{"id": float64(1)}},
	})
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "unknown step",
			args:    map[string]any{"x": "{{step9.output.id}}"},
			wantErr: "step \"step9\" produced no output",
		},
		{
			name:    "missing key",
			args:    map[string]any{"x": "{{step1.output.items[0].id}}"},
			wantErr: "key \"items\" not found (available: containers)",
		},
		{
			name:    "out-of-range index",
			args:    map[string]any{"x": "{{step1.output.containers[5].id}}"},
			wantErr: "out of range",
		},
		{
			name:    "type mismatch",
			args:    map[string]any{"x": "{{step1.output.containers[0].id.foo}}"},
			wantErr: "expected object, got number",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := resolveArgs(c.args, ctx)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err = %q, want substring %q", err.Error(), c.wantErr)
			}
			// Errors must surface the offending arg key for actionable diagnostics.
			if !strings.Contains(err.Error(), "arg \"x\"") {
				t.Errorf("err missing arg-key context: %q", err.Error())
			}
		})
	}
}

// --- step output: structured vs text ---

func TestParseStepOutputStructured(t *testing.T) {
	r := StepRunResult{
		Content:           "Containers: 1 total",
		StructuredContent: `{"containers":[{"id":42}]}`,
	}
	v := parseStepOutput(r)
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", v)
	}
	if cs, _ := m["containers"].([]any); len(cs) != 1 {
		t.Errorf("containers length = %d, want 1", len(cs))
	}
}

func TestParseStepOutputFallsBackToText(t *testing.T) {
	cases := []struct {
		name string
		in   StepRunResult
		want string
	}{
		{"empty structured", StepRunResult{Content: "hello"}, "hello"},
		{"malformed structured", StepRunResult{Content: "txt", StructuredContent: "not-json"}, "txt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := parseStepOutput(c.in)
			s, ok := v.(string)
			if !ok || s != c.want {
				t.Errorf("got %v (%T), want string %q", v, v, c.want)
			}
		})
	}
}
