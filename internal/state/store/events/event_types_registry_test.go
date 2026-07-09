package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// constEventTypesFromSource parses event_types.go and returns the string
// value of every Type* constant. This is the ground truth AllEventTypes
// must mirror — parsing the AST rather than hand-listing means the test
// can't drift alongside the thing it guards.
func constEventTypesFromSource(t *testing.T) map[string]string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	src := filepath.Join(filepath.Dir(thisFile), "event_types.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	out := make(map[string]string)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Type") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s value %q: %v", name.Name, lit.Value, err)
				}
				out[name.Name] = val
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no Type* constants in event_types.go — parser or filter is wrong")
	}
	return out
}

// TestAllEventTypesRegistered fails if a Type* constant is missing from
// AllEventTypes (a new event type silently unvalidatable) or if
// AllEventTypes carries a value that no constant defines (a stale entry).
func TestAllEventTypesRegistered(t *testing.T) {
	constByName := constEventTypesFromSource(t)

	// Every constant's value must appear in the registry.
	registry := make(map[string]struct{}, len(AllEventTypes))
	for _, v := range AllEventTypes {
		registry[v] = struct{}{}
	}
	for name, val := range constByName {
		if _, ok := registry[val]; !ok {
			t.Errorf("event type %s = %q is not in AllEventTypes — add it there", name, val)
		}
	}

	// Every registry value must be backed by a constant.
	constValues := make(map[string]struct{}, len(constByName))
	for _, v := range constByName {
		constValues[v] = struct{}{}
	}
	for _, v := range AllEventTypes {
		if _, ok := constValues[v]; !ok {
			t.Errorf("AllEventTypes has %q with no matching Type* constant — stale entry", v)
		}
	}

	// No duplicates in the registry.
	if len(registry) != len(AllEventTypes) {
		t.Errorf("AllEventTypes has duplicates: %d entries, %d unique", len(AllEventTypes), len(registry))
	}
}

func TestIsKnownEventType(t *testing.T) {
	if !IsKnownEventType(TypeTurnFinished) {
		t.Errorf("IsKnownEventType(%q) = false, want true", TypeTurnFinished)
	}
	if !IsKnownEventType(TypeUserMessage) {
		t.Errorf("IsKnownEventType(%q) = false, want true", TypeUserMessage)
	}
	if IsKnownEventType("turn_finsihed") {
		t.Error("IsKnownEventType(typo) = true, want false")
	}
	if IsKnownEventType("") {
		t.Error("IsKnownEventType(\"\") = true, want false")
	}
}
