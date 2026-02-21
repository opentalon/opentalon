package channel

import (
	"fmt"
	"strings"
)

// DetectMode inspects the plugin reference string and returns the
// appropriate connection mode. The detection relies on URI scheme
// prefixes:
//
//	grpc://      -> ModeGRPC
//	docker://    -> ModeDocker
//	http(s)://   -> ModeWebhook
//	ws(s)://     -> ModeWebSocket
//	anything else -> ModeBinary (local filesystem path)
func DetectMode(plugin string) PluginMode {
	lower := strings.ToLower(plugin)

	switch {
	case strings.HasPrefix(lower, "grpc://"):
		return ModeGRPC
	case strings.HasPrefix(lower, "docker://"):
		return ModeDocker
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return ModeWebhook
	case strings.HasPrefix(lower, "ws://"), strings.HasPrefix(lower, "wss://"):
		return ModeWebSocket
	default:
		return ModeBinary
	}
}

// ParsePluginAddress strips the scheme prefix and returns the
// usable address for each mode.
//
//	ModeBinary:    returns the path as-is
//	ModeGRPC:      strips "grpc://"
//	ModeDocker:    strips "docker://"
//	ModeWebhook:   returns full URL (http/https)
//	ModeWebSocket: returns full URL (ws/wss)
func ParsePluginAddress(plugin string) (PluginMode, string) {
	mode := DetectMode(plugin)

	switch mode {
	case ModeGRPC:
		return mode, strings.TrimPrefix(strings.TrimPrefix(plugin, "grpc://"), "GRPC://")
	case ModeDocker:
		return mode, strings.TrimPrefix(strings.TrimPrefix(plugin, "docker://"), "DOCKER://")
	case ModeWebhook, ModeWebSocket:
		return mode, plugin
	default:
		return mode, plugin
	}
}

// PluginRef holds the parsed components of a plugin reference string.
type PluginRef struct {
	Raw     string
	Mode    PluginMode
	Address string
}

// ParsePluginRef parses a raw plugin string into a PluginRef.
func ParsePluginRef(raw string) (PluginRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return PluginRef{}, fmt.Errorf("empty plugin reference")
	}
	mode, addr := ParsePluginAddress(raw)
	return PluginRef{
		Raw:     raw,
		Mode:    mode,
		Address: addr,
	}, nil
}
