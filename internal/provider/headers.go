package provider

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// HeaderMap holds extra HTTP request headers configured for a provider
// (models.providers.<id>.headers). It is a plain map[string]string underneath;
// the named type only exists so that printing a ProviderConfig with fmt shows
// the header names but never their values, which usually carry credentials.
type HeaderMap map[string]string

// Format prints the header names with every value replaced by [redacted].
func (h HeaderMap) Format(f fmt.State, _ rune) {
	if h == nil {
		_, _ = io.WriteString(f, "map[]")
		return
	}
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("map[")
	for i, k := range names {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteString(":[redacted]")
	}
	b.WriteByte(']')
	_, _ = io.WriteString(f, b.String())
}

// copyExtraHeaders returns a private copy of h without the entries whose value
// is empty, so an unset credential sends no header instead of an empty one.
// It returns nil when nothing is left, which keeps requests unchanged.
func copyExtraHeaders(h map[string]string) map[string]string {
	var out map[string]string
	for k, v := range h {
		if v == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(h))
		}
		out[k] = v
	}
	return out
}

// applyExtraHeaders sets the configured headers on req. Callers run it after
// setting their default headers, so a configured header with the same name
// replaces the default one.
func applyExtraHeaders(req *http.Request, h map[string]string) {
	for k, v := range h {
		req.Header.Set(k, v)
	}
}
