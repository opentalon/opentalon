package channel

import (
	"strings"
	"testing"
)

func TestChunkMessage(t *testing.T) {
	tests := []struct {
		name    string
		msg     string
		maxLen  int
		wantN   int
		wantAll string // concatenated chunks should equal original
	}{
		{
			name:    "fits in one chunk",
			msg:     "hello world",
			maxLen:  100,
			wantN:   1,
			wantAll: "hello world",
		},
		{
			name:    "zero maxLen returns single chunk",
			msg:     "hello world",
			maxLen:  0,
			wantN:   1,
			wantAll: "hello world",
		},
		{
			name:    "negative maxLen returns single chunk",
			msg:     "hello world",
			maxLen:  -1,
			wantN:   1,
			wantAll: "hello world",
		},
		{
			name:    "splits at newline",
			msg:     "line1\nline2\nline3",
			maxLen:  10,
			wantN:   3,
			wantAll: "line1\nline2\nline3",
		},
		{
			name:    "exact fit",
			msg:     "12345",
			maxLen:  5,
			wantN:   1,
			wantAll: "12345",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := ChunkMessage(tt.msg, tt.maxLen)
			if len(chunks) != tt.wantN {
				t.Errorf("got %d chunks, want %d: %v", len(chunks), tt.wantN, chunks)
			}
			got := strings.Join(chunks, "")
			if got != tt.wantAll {
				t.Errorf("reassembled = %q, want %q", got, tt.wantAll)
			}
			// Each chunk should be within maxLen (when maxLen > 0)
			if tt.maxLen > 0 {
				for i, c := range chunks {
					if len(c) > tt.maxLen {
						t.Errorf("chunk[%d] len=%d > maxLen=%d", i, len(c), tt.maxLen)
					}
				}
			}
		})
	}
}
