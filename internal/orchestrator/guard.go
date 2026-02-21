package orchestrator

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
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
	result.Error = g.sanitizeContent(result.Error)
	return result
}

func (g *Guard) sanitizeContent(s string) string {
	if s == "" {
		return s
	}

	if g.MaxResponseBytes > 0 && len(s) > g.MaxResponseBytes {
		s = s[:g.MaxResponseBytes] + "\n[truncated: response exceeded size limit]"
	}

	for _, pat := range g.ForbiddenPatterns {
		s = pat.ReplaceAllStringFunc(s, func(match string) string {
			return strings.Repeat("*", len(match))
		})
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

func (g *Guard) WrapContent(result ToolResult) string {
	if result.Error != "" {
		return fmt.Sprintf("[plugin_output]\nerror: %s\n[/plugin_output]", result.Error)
	}
	return fmt.Sprintf("[plugin_output]\n%s\n[/plugin_output]", result.Content)
}

func (g *Guard) ExecuteWithTimeout(ctx context.Context, exec PluginExecutor, call ToolCall) ToolResult {
	callCtx, cancel := context.WithTimeout(ctx, g.Timeout)
	defer cancel()

	done := make(chan ToolResult, 1)
	go func() {
		done <- exec.Execute(call)
	}()

	select {
	case result := <-done:
		return result
	case <-callCtx.Done():
		return ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("plugin %q timed out after %s", call.Plugin, g.Timeout),
		}
	}
}
