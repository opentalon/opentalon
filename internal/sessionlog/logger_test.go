package sessionlog

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSanitizeKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"console:general", "console_general"},
		{"slack:C12345", "slack_C12345"},
		{"simple", "simple"},
		{"a/b\\c?d*e", "abcde"},
		{strings.Repeat("x", 100), strings.Repeat("x", 50)},
		{"with spaces", "withspaces"},
	}
	for _, tt := range tests {
		got := sanitizeKey(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestManagerGetCreatesCachedLogger(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.CloseAll()

	l1 := m.Get("test-session")
	if l1 == nil {
		t.Fatal("expected non-nil logger")
	}

	l2 := m.Get("test-session")
	if l1 != l2 {
		t.Error("expected same logger instance for same session key")
	}

	// Different session key should get a different logger.
	l3 := m.Get("other-session")
	if l3 == nil {
		t.Fatal("expected non-nil logger for other session")
	}
	if l1 == l3 {
		t.Error("expected different logger for different session key")
	}
}

func TestLoggerPrintf_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	l := m.Get("write-test")
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
	l.Printf("hello %s", "world")
	m.CloseAll()

	// Find the log file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var logFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			logFile = filepath.Join(dir, e.Name())
			break
		}
	}
	if logFile == "" {
		t.Fatal("no log file created")
	}

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Errorf("log file content = %q, want to contain 'hello world'", string(data))
	}
}

func TestNilLoggerPrintf_DoesNotPanic(t *testing.T) {
	var l *Logger
	// Should not panic; falls back to global log.
	l.Printf("test %d", 42)
}

func TestManagerClose(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	l := m.Get("close-test")
	if l == nil {
		t.Fatal("expected non-nil logger")
	}

	m.Close("close-test")

	// After close, Get should create a new logger.
	l2 := m.Get("close-test")
	if l2 == nil {
		t.Fatal("expected non-nil logger after re-get")
	}
	if l == l2 {
		t.Error("expected new logger after close")
	}
	m.CloseAll()
}

func TestCleanup(t *testing.T) {
	dir := t.TempDir()

	// Create an "old" log file with a modification time > 7 days ago.
	oldFile := filepath.Join(dir, "20260101_000000_old.log")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create a "recent" log file.
	recentFile := filepath.Join(dir, "20260316_000000_recent.log")
	if err := os.WriteFile(recentFile, []byte("recent"), 0600); err != nil {
		t.Fatal(err)
	}

	// Test cleanup directly (NewManager runs it async).
	m := &Manager{dir: dir, loggers: make(map[string]*Logger)}
	defer m.CloseAll()
	m.cleanup(7 * 24 * time.Hour)

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Error("expected old log file to be cleaned up")
	}
	if _, err := os.Stat(recentFile); err != nil {
		t.Error("expected recent log file to survive cleanup")
	}
}

func TestManagerGet_Concurrent(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.CloseAll()

	var wg sync.WaitGroup
	loggers := make([]*Logger, 20)
	for i := range loggers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			loggers[i] = m.Get("shared-key")
		}(i)
	}
	wg.Wait()

	// All goroutines should get the same cached logger.
	for i := 1; i < len(loggers); i++ {
		if loggers[i] != loggers[0] {
			t.Fatalf("goroutine %d got different logger instance", i)
		}
	}
}
