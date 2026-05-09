package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultMaxResponseBytes = 64 * 1024 // 64KB
	DefaultTimeout          = 30 * time.Second
)

var defaultForbiddenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\[tool_call\]`),
	regexp.MustCompile(`\[tool_use\]`),
	regexp.MustCompile(`<tool_call>`),
	regexp.MustCompile(`<function_call>`),
	regexp.MustCompile(`"type"\s*:\s*"function"`),
	regexp.MustCompile(`"tool_calls"\s*:\s*\[`),
}

type Guard struct {
	MaxResponseBytes  int
	Timeout           time.Duration
	ForbiddenPatterns []*regexp.Regexp
}

func NewGuard() *Guard {
	return &Guard{
		MaxResponseBytes:  DefaultMaxResponseBytes,
		Timeout:           DefaultTimeout,
		ForbiddenPatterns: defaultForbiddenPatterns,
	}
}

func (g *Guard) Sanitize(result ToolResult) ToolResult {
	result.Content = g.sanitizeContent(result.Content)
	// Structured content is JSON: pattern-replacement would silently corrupt
	// it (e.g. asterisks inside object keys produce invalid JSON the model
	// then can't parse). Plugins are a trusted subprocess boundary per the
	// project's threat model, so we cap size only and leave the payload
	// intact.
	result.StructuredContent = g.truncate(result.StructuredContent)
	result.Error = g.sanitizeContent(result.Error)
	// Plugin responses can contain invalid UTF-8 from external sources.
	// gRPC proto marshaling rejects any string field with invalid UTF-8,
	// so replace bad sequences with the Unicode replacement character.
	result.Content = toValidUTF8(result.Content)
	result.StructuredContent = toValidUTF8(result.StructuredContent)
	result.Error = toValidUTF8(result.Error)
	return result
}

// toValidUTF8 replaces invalid UTF-8 sequences with U+FFFD.
func toValidUTF8(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\ufffd")
}

func (g *Guard) sanitizeContent(s string) string {
	if s == "" {
		return s
	}
	s = g.truncate(s)
	for _, pat := range g.ForbiddenPatterns {
		s = pat.ReplaceAllStringFunc(s, func(match string) string {
			return strings.Repeat("*", len(match))
		})
	}
	return s
}

func (g *Guard) truncate(s string) string {
	if g.MaxResponseBytes > 0 && len(s) > g.MaxResponseBytes {
		// Back up to the last valid UTF-8 boundary to avoid splitting
		// a multi-byte character, which produces invalid UTF-8 that
		// gRPC proto marshaling rejects.
		cut := g.MaxResponseBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return s[:cut] + "\n[truncated: response exceeded size limit]"
	}
	return s
}

func (g *Guard) ValidateResult(call ToolCall, result ToolResult) ToolResult {
	if result.CallID != call.ID {
		return ToolResult{
			CallID: call.ID,
			Error:  "plugin returned mismatched call ID",
		}
	}
	return result
}

// WrapContent renders a tool result for inclusion in the LLM transcript.
//
// Layout: a single [plugin_output]…[/plugin_output] envelope per result.
// When the underlying plugin produced a structured payload, it is nested
// inside the envelope under a [structured]…[/structured] block. Keeping
// the wrapping single-block preserves the "this belongs to one tool
// call" signal even with parallel calls, and the existing
// internalBlockTags stripper continues to remove the whole envelope as
// one unit.
func (g *Guard) WrapContent(result ToolResult) string {
	if result.Error != "" {
		return fmt.Sprintf("[plugin_output]\nerror: %s\n[/plugin_output]", result.Error)
	}
	if result.StructuredContent == "" {
		return fmt.Sprintf("[plugin_output]\n%s\n[/plugin_output]", result.Content)
	}
	return fmt.Sprintf(
		"[plugin_output]\n%s\n\n[structured]\n%s\n[/structured]\n[/plugin_output]",
		result.Content, result.StructuredContent,
	)
}

func (g *Guard) ExecuteWithTimeout(ctx context.Context, exec PluginExecutor, call ToolCall) ToolResult {
	return g.ExecuteWithDeadline(ctx, exec, call, g.Timeout)
}

// ExecuteWithDeadline runs a plugin call with a caller-specified timeout.
func (g *Guard) ExecuteWithDeadline(ctx context.Context, exec PluginExecutor, call ToolCall, timeout time.Duration) ToolResult {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan ToolResult, 1)
	go func() {
		done <- exec.Execute(callCtx, call)
	}()

	select {
	case result := <-done:
		return result
	case <-callCtx.Done():
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("plugin %q timed out after %s", call.Plugin, timeout),
		}
	}
}
