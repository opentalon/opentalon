package sessionlog

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Logger wraps a *log.Logger writing to a per-session file.
type Logger struct {
	logger *log.Logger
	file   *os.File
}

// Printf logs to the per-session file. If l is nil, falls back to the global log package.
func (l *Logger) Printf(format string, args ...interface{}) {
	if l == nil || l.logger == nil {
		log.Printf(format, args...)
		return
	}
	l.logger.Printf(format, args...)
}

// Manager creates and caches per-session loggers.
type Manager struct {
	mu      sync.RWMutex
	dir     string
	loggers map[string]*Logger
}

// NewManager creates a new session log manager. It ensures the log directory
// exists and cleans up log files older than 7 days.
func NewManager(logsDir string) *Manager {
	if err := os.MkdirAll(logsDir, 0750); err != nil {
		log.Printf("Warning: session log dir: %v", err)
	}
	m := &Manager{
		dir:     logsDir,
		loggers: make(map[string]*Logger),
	}
	go m.cleanup(7 * 24 * time.Hour)
	return m
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_\-]`)

// sanitizeKey replaces : with _, strips other unsafe chars, and truncates to 50 chars.
func sanitizeKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = unsafeChars.ReplaceAllString(s, "")
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// Get returns a per-session logger, creating the log file on first call for a
// given session key. Subsequent calls with the same key return the cached logger.
func (m *Manager) Get(sessionKey string) *Logger {
	// Fast path: read lock for cache hit (common case).
	m.mu.RLock()
	if l, ok := m.loggers[sessionKey]; ok {
		m.mu.RUnlock()
		return l
	}
	m.mu.RUnlock()

	// Slow path: write lock for cache miss (first call per session).
	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check after acquiring write lock.
	if l, ok := m.loggers[sessionKey]; ok {
		return l
	}

	sanitized := sanitizeKey(sessionKey)
	ts := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("%s_%s.log", ts, sanitized)
	path := filepath.Join(m.dir, filename)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Printf("Warning: session log file %s: %v", path, err)
		return nil
	}

	l := &Logger{
		logger: log.New(f, "", log.LstdFlags),
		file:   f,
	}
	m.loggers[sessionKey] = l
	return l
}

// Close closes the log file for a specific session and removes it from the cache.
func (m *Manager) Close(sessionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if l, ok := m.loggers[sessionKey]; ok {
		if l.file != nil {
			_ = l.file.Close()
		}
		delete(m.loggers, sessionKey)
	}
}

// CloseAll closes all open session log files.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, l := range m.loggers {
		if l.file != nil {
			_ = l.file.Close()
		}
		delete(m.loggers, key)
	}
}

// cleanup removes log files older than maxAge from the log directory.
func (m *Manager) cleanup(maxAge time.Duration) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(m.dir, e.Name()))
		}
	}
}
