// Package grpclimit centralises the gRPC message size limits used on every
// host<->plugin and host<->channel path.
//
// grpc-go's default receive limit is 4 MiB, which is sized for untrusted
// remote peers. Our transport is a local Unix socket (or loopback TCP) between
// a host and a plugin it launched, so that rationale does not apply. Tool call
// arguments are carried inline in one unary message (ToolCallRequest.args is a
// map<string,string>), so a single large argument — a diff, a document, a
// serialised fact set — has to fit in the budget.
//
// The limit must be raised on both the server (MaxRecvMsgSize) and the client
// (MaxCallRecvMsgSize): raising one side alone only moves the failure to the
// other direction.
package grpclimit

import (
	"log/slog"
	"os"
	"strconv"

	"google.golang.org/grpc"
)

// EnvMaxMsgBytes overrides the receive limit, in bytes. It is read by both the
// host and the plugin SDK; the host launches plugins with its own environment,
// so setting it on the host applies to both ends of the connection.
const EnvMaxMsgBytes = "OPENTALON_GRPC_MAX_MSG_BYTES"

// DefaultMaxMsgBytes is the receive limit when EnvMaxMsgBytes is unset.
const DefaultMaxMsgBytes = 32 << 20 // 32 MiB

// MaxMsgBytes returns the configured receive limit in bytes. A missing,
// unparseable, or non-positive EnvMaxMsgBytes falls back to DefaultMaxMsgBytes.
func MaxMsgBytes() int {
	raw := os.Getenv(EnvMaxMsgBytes)
	if raw == "" {
		return DefaultMaxMsgBytes
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		slog.Warn("ignoring invalid "+EnvMaxMsgBytes+", using default",
			"value", raw, "default_bytes", DefaultMaxMsgBytes)
		return DefaultMaxMsgBytes
	}
	return n
}

// ServerOptions returns the grpc.ServerOptions that apply the limit.
func ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{grpc.MaxRecvMsgSize(MaxMsgBytes())}
}

// DialOptions returns the grpc.DialOptions that apply the limit.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMsgBytes())),
	}
}
