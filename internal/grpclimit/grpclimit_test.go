package grpclimit

import "testing"

func TestMaxMsgBytesUnset(t *testing.T) {
	t.Setenv(EnvMaxMsgBytes, "")
	if got := MaxMsgBytes(); got != DefaultMaxMsgBytes {
		t.Fatalf("MaxMsgBytes() = %d, want %d", got, DefaultMaxMsgBytes)
	}
}

func TestMaxMsgBytesOverride(t *testing.T) {
	t.Setenv(EnvMaxMsgBytes, "1048576")
	if got := MaxMsgBytes(); got != 1048576 {
		t.Fatalf("MaxMsgBytes() = %d, want 1048576", got)
	}
}

func TestMaxMsgBytesBadValues(t *testing.T) {
	for _, raw := range []string{"0", "-1", "banana", "32MiB", "1_000", " 1024", "9999999999999999999999"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvMaxMsgBytes, raw)
			if got := MaxMsgBytes(); got != DefaultMaxMsgBytes {
				t.Fatalf("MaxMsgBytes() with %q = %d, want default %d", raw, got, DefaultMaxMsgBytes)
			}
		})
	}
}

func TestOptionsBuild(t *testing.T) {
	if len(ServerOptions()) != 1 {
		t.Fatal("ServerOptions() must return exactly the recv-size option")
	}
	if len(DialOptions()) != 1 {
		t.Fatal("DialOptions() must return exactly the recv-size call option")
	}
}
