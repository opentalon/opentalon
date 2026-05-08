package pipeline

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// placeholderRe matches `{{<step-id>.output.<json-path>}}` — the only
// reference form supported in step args. Anchoring on the literal `.output.`
// is deliberate: it forces a deterministic shape ("which step? which
// channel? which path?") and rejects ambiguous forms like `{{step1.id}}`
// that would have multiple plausible interpretations.
//
// Submatches:
//
//	[1] step ID
//	[2] path (dot- and bracket-separated)
//
// The path matcher is deliberately permissive — `[0-9a-zA-Z_.\[\]]+` —
// because it's parsed in a second pass by splitPath, which produces a
// precise diagnostic on malformed expressions.
var placeholderRe = regexp.MustCompile(`\{\{([a-zA-Z0-9_-]+)\.output\.([0-9a-zA-Z_.\[\]]+)\}\}`)

// soloPlaceholderRe matches a string whose ENTIRE content is one
// placeholder. When it matches, substitution preserves the resolved
// value's type (an integer ID stays int) instead of stringifying;
// otherwise the value is interpolated into surrounding text.
var soloPlaceholderRe = regexp.MustCompile(`^\{\{([a-zA-Z0-9_-]+)\.output\.([0-9a-zA-Z_.\[\]]+)\}\}$`)

// resolveArgs returns a copy of args with every `{{<step>.output.<path>}}`
// placeholder replaced by the resolved value from ctx.
//
// On any unresolved or malformed reference, resolveArgs returns an error
// describing the failure and the offending argument key — the executor
// surfaces this directly to the user / agent loop. Partial substitution
// (some args resolved, some left literal) is never returned: a step either
// runs with all references concrete or fails fast with a clear message.
func resolveArgs(args map[string]any, ctx *PipelineContext) (map[string]any, error) {
	if len(args) == 0 {
		return args, nil
	}
	out := make(map[string]any, len(args))
	// Stable iteration so error messages are deterministic across runs.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := resolveValue(args[k], ctx)
		if err != nil {
			return nil, fmt.Errorf("arg %q: %w", k, err)
		}
		out[k] = v
	}
	return out, nil
}

// resolveValue applies substitution to a single value. Strings are
// inspected for placeholders; maps and slices are walked recursively (the
// planner can emit them via JSON, e.g. `include_fields: ["a","b"]`); other
// scalars are passed through.
func resolveValue(v any, ctx *PipelineContext) (any, error) {
	switch x := v.(type) {
	case string:
		return resolveString(x, ctx)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			r, err := resolveValue(e, ctx)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = r
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			r, err := resolveValue(e, ctx)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// resolveString returns the value of s with every placeholder replaced.
// When s consists of a single placeholder, the resolved value is returned
// untyped (string / number / bool / nested) so the caller can hand it to
// the wire layer with type intact. Otherwise placeholders are stringified
// and embedded into the surrounding text.
func resolveString(s string, ctx *PipelineContext) (any, error) {
	if m := soloPlaceholderRe.FindStringSubmatch(s); m != nil {
		return lookup(m[1], m[2], ctx)
	}
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	var firstErr error
	out := placeholderRe.ReplaceAllStringFunc(s, func(match string) string {
		m := placeholderRe.FindStringSubmatch(match)
		if m == nil {
			return match // leave literal — regex never disagrees with itself
		}
		v, err := lookup(m[1], m[2], ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return match
		}
		return stringifyForInterpolation(v)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// lookup resolves `<step>.output.<path>` against the pipeline context. The
// step's stored "output" is the parsed structured payload; resolvePath
// walks the user-supplied path through it.
func lookup(stepID, path string, ctx *PipelineContext) (any, error) {
	root, ok := ctx.Get(stepID, "output")
	if !ok {
		return nil, fmt.Errorf("unresolved reference {{%s.output.%s}}: step %q produced no output (or has not run yet)", stepID, path, stepID)
	}
	v, err := resolvePath(root, path)
	if err != nil {
		return nil, fmt.Errorf("unresolved reference {{%s.output.%s}}: %w", stepID, path, err)
	}
	return v, nil
}

// resolvePath walks dot- and bracket-separated segments through a parsed
// JSON value. Errors include the segment that failed and the keys / index
// range available at that point — actionable diagnostics for the
// downstream surfaced-to-LLM error.
func resolvePath(root any, path string) (any, error) {
	segments, err := splitPath(path)
	if err != nil {
		return nil, err
	}
	cur := root
	traversed := ""
	for _, seg := range segments {
		switch s := seg.(type) {
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("at %q: expected object, got %s", traversed, kindOf(cur))
			}
			next, ok := obj[s]
			if !ok {
				keys := make([]string, 0, len(obj))
				for k := range obj {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				return nil, fmt.Errorf("at %q: key %q not found (available: %s)", traversed, s, strings.Join(keys, ", "))
			}
			cur = next
			traversed = appendSegment(traversed, s)
		case int:
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("at %q: expected array, got %s", traversed, kindOf(cur))
			}
			if s < 0 || s >= len(arr) {
				return nil, fmt.Errorf("at %q: index [%d] out of range (length %d)", traversed, s, len(arr))
			}
			cur = arr[s]
			traversed = appendIndex(traversed, s)
		}
	}
	return cur, nil
}

// splitPath turns a path string ("containers[0].id") into a sequence of
// segments — strings for keys, ints for array indices. It rejects empty
// segments, trailing dots, and unmatched brackets up front so resolvePath
// can iterate without re-validating.
func splitPath(path string) ([]any, error) {
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	var segs []any
	i := 0
	for i < len(path) {
		switch path[i] {
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed '[' in path %q", path)
			}
			idxStr := path[i+1 : i+end]
			idx, err := strconv.Atoi(idxStr)
			if err != nil || idxStr == "" {
				return nil, fmt.Errorf("invalid array index %q in path %q", idxStr, path)
			}
			segs = append(segs, idx)
			i += end + 1
			if i < len(path) && path[i] == '.' {
				i++ // accept the join dot between [N] and the next key
			}
		case '.':
			return nil, fmt.Errorf("empty segment in path %q", path)
		default:
			j := i
			for j < len(path) && path[j] != '.' && path[j] != '[' {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("empty segment in path %q", path)
			}
			segs = append(segs, path[i:j])
			i = j
			if i < len(path) && path[i] == '.' {
				i++
				if i == len(path) {
					return nil, fmt.Errorf("trailing '.' in path %q", path)
				}
			}
		}
	}
	return segs, nil
}

// stringifyForInterpolation renders a value when it's embedded into
// surrounding text. Mirrors argToWireString in the orchestrator (no
// scientific notation on floats, JSON for compounds), but lives here so
// the pipeline package stays self-contained.
func stringifyForInterpolation(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		if x == float64(int64(x)) && x > -1e18 && x < 1e18 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// kindOf names a value's runtime shape for error messages — "object" /
// "array" / "string" / "number" / "boolean" / "null" — independent of Go
// types. Matches the JSON vocabulary the planner LLM emits, which is what
// the LLM reads in the surfaced error.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, int, int64, float32, int32:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func appendSegment(s, key string) string {
	if s == "" {
		return key
	}
	return s + "." + key
}

func appendIndex(s string, idx int) string {
	return s + "[" + strconv.Itoa(idx) + "]"
}
