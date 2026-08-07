package plugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// echoSizeHandler echoes the "payload" arg back as content, so one Execute
// exercises both receive paths: the plugin server receives the request, the
// host client receives the response.
type echoSizeHandler struct{}

func (echoSizeHandler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{
		Name:        "sizer",
		Description: "echoes its payload",
		Actions:     []pkg.ActionMsg{{Name: "echo", Description: "echo payload"}},
	}
}

func (echoSizeHandler) Execute(req pkg.Request) pkg.Response {
	return pkg.Response{CallID: req.ID, Content: req.Args["payload"]}
}

// startSizedPlugin serves a real pkg/plugin gRPC server on a Unix socket and
// dials it with the real host client, so both ends pick up the configured
// limit the same way the process pair does in production.
func startSizedPlugin(t *testing.T) *Client {
	t.Helper()

	// Not t.TempDir(): it embeds the test name, and the long names here blow
	// past the ~104-byte sun_path limit on macOS.
	dir, err := os.MkdirTemp("", "ot-msgsize-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, pkg.SocketFileName)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = pkg.ServeListener(ln, echoSizeHandler{}) }()
	t.Cleanup(func() { _ = ln.Close() })

	c, err := Dial("unix", sock, 5*time.Second, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func execPayload(t *testing.T, c *Client, size int) orchestrator.ToolResult {
	t.Helper()
	return c.Execute(context.Background(), orchestrator.ToolCall{
		ID:     "call-1",
		Plugin: "sizer",
		Action: "echo",
		Args:   map[string]string{"payload": strings.Repeat("x", size)},
	})
}

// A payload above grpc-go's 4 MiB default must survive both directions.
func TestExecuteAboveGRPCDefault(t *testing.T) {
	c := startSizedPlugin(t)

	const size = 8 << 20
	res := execPayload(t, c, size)
	if res.Error != "" {
		t.Fatalf("Execute with %d-byte arg failed: %s", size, res.Error)
	}
	if len(res.Content) != size {
		t.Fatalf("content = %d bytes, want %d", len(res.Content), size)
	}
}

// Right at the configured ceiling the call still fails — the limit applies to
// the whole message, not to one field — but it must fail at the configured
// value, not at grpc-go's default.
func TestExecuteAtDefaultCeilingFails(t *testing.T) {
	c := startSizedPlugin(t)

	res := execPayload(t, c, 32<<20)
	if res.Error == "" {
		t.Fatal("Execute with a 32 MiB arg unexpectedly succeeded")
	}
	if !strings.Contains(res.Error, "ResourceExhausted") {
		t.Fatalf("error = %q, want ResourceExhausted", res.Error)
	}
	if strings.Contains(res.Error, "4194304") {
		t.Fatalf("limit is still grpc-go's 4 MiB default: %s", res.Error)
	}
}

// The env override must lower the limit on both ends.
func TestEnvOverrideLowersLimit(t *testing.T) {
	t.Setenv("OPENTALON_GRPC_MAX_MSG_BYTES", "65536")
	c := startSizedPlugin(t)

	if res := execPayload(t, c, 1024); res.Error != "" {
		t.Fatalf("Execute below the override failed: %s", res.Error)
	}

	res := execPayload(t, c, 128<<10)
	if res.Error == "" {
		t.Fatal("Execute above the 64 KiB override unexpectedly succeeded")
	}
	if !strings.Contains(res.Error, "65536") {
		t.Fatalf("error = %q, want the 65536 override to be the reported max", res.Error)
	}
}

// An unparseable override falls back to the default instead of failing the
// dial or clamping to something surprising.
func TestBadEnvOverrideFallsBackToDefault(t *testing.T) {
	t.Setenv("OPENTALON_GRPC_MAX_MSG_BYTES", "not-a-number")
	c := startSizedPlugin(t)

	const size = 8 << 20
	res := execPayload(t, c, size)
	if res.Error != "" {
		t.Fatalf("Execute with %d-byte arg failed under a bad override: %s", size, res.Error)
	}
	if len(res.Content) != size {
		t.Fatalf("content = %d bytes, want %d", len(res.Content), size)
	}
}

func TestMain(m *testing.M) {
	// Keep an operator's ambient value out of the size tests.
	_ = os.Unsetenv("OPENTALON_GRPC_MAX_MSG_BYTES")
	os.Exit(m.Run())
}
