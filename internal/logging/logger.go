// Package logging provides gstop's file-only rotating logger, a port of the
// Python common/log.py. Output never goes to the console (the TUI owns the
// screen); it is written to size-rotated files. Loggers that target the same
// file share one handler and its mutex, so writes from the refresh, alarm, and
// persistence goroutines never interleave or race on rotation.
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Level is a syslog-style severity. Higher values are more severe.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarning
	LevelError
	LevelCritical
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarning:
		return "WARNING"
	case LevelError:
		return "ERROR"
	case LevelCritical:
		return "CRITICAL"
	default:
		return "INFO"
	}
}

const (
	defaultMaxBytes    int64 = 10 * 1024 * 1024
	defaultBackupCount       = 5
)

// Logger emits formatted records to a shared file handler. A Logger with no
// handler (empty logFile) discards output, matching the Python behaviour of a
// logger created without a log_file.
type Logger struct {
	name  string
	level Level
	h     *handler
	now   func() time.Time
}

// New returns a logger writing to logFile with the default rotation policy
// (10 MiB per file, 5 backups). An empty logFile yields a no-op logger.
func New(name, logFile string) *Logger {
	return NewWithRotation(name, logFile, LevelInfo, defaultMaxBytes, defaultBackupCount)
}

// NewWithRotation returns a logger with an explicit level and rotation policy.
func NewWithRotation(name, logFile string, level Level, maxBytes int64, backupCount int) *Logger {
	var h *handler
	if logFile != "" {
		h = getHandler(logFile, maxBytes, backupCount)
	}
	return &Logger{name: name, level: level, h: h, now: time.Now}
}

// SetLevel returns a copy of the logger at the given level, leaving the
// receiver unchanged.
func (l *Logger) SetLevel(level Level) *Logger {
	clone := *l
	clone.level = level
	return &clone
}

func (l *Logger) Debug(format string, a ...any)    { l.log(LevelDebug, format, a...) }
func (l *Logger) Info(format string, a ...any)     { l.log(LevelInfo, format, a...) }
func (l *Logger) Warning(format string, a ...any)  { l.log(LevelWarning, format, a...) }
func (l *Logger) Error(format string, a ...any)    { l.log(LevelError, format, a...) }
func (l *Logger) Critical(format string, a ...any) { l.log(LevelCritical, format, a...) }

func (l *Logger) log(level Level, format string, a ...any) {
	if l == nil || l.h == nil || level < l.level {
		return
	}
	msg := format
	if len(a) > 0 {
		msg = fmt.Sprintf(format, a...)
	}
	t := l.now()
	line := fmt.Sprintf("%s.%03d - %s - %s - %s\n",
		t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6, l.name, level, msg)
	l.h.write(line)
}

// handler owns an open file and serialises writes and rotation for one path.
type handler struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	backupCount int
	file        *os.File
	size        int64
}

var (
	handlersMu sync.Mutex
	handlers   = map[string]*handler{}
)

// getHandler returns the shared handler for path, creating it (and the parent
// directory) on first use. Rotation parameters are fixed by the first caller.
func getHandler(path string, maxBytes int64, backupCount int) *handler {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	handlersMu.Lock()
	defer handlersMu.Unlock()
	if h, ok := handlers[abs]; ok {
		return h
	}
	h := &handler{path: abs, maxBytes: maxBytes, backupCount: backupCount}
	h.open()
	handlers[abs] = h
	return h
}

func (h *handler) open() {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	h.file = f
	if info, err := f.Stat(); err == nil {
		h.size = info.Size()
	}
}

func (h *handler) write(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return
	}
	if h.maxBytes > 0 && h.size+int64(len(line)) > h.maxBytes {
		h.rollover()
	}
	n, err := h.file.WriteString(line)
	if err == nil {
		h.size += int64(n)
	}
}

// rollover renames path -> path.1 -> ... -> path.N, discarding the oldest,
// then opens a fresh file, matching RotatingFileHandler.
func (h *handler) rollover() {
	if h.file != nil {
		h.file.Close()
		h.file = nil
	}
	if h.backupCount > 0 {
		oldest := fmt.Sprintf("%s.%d", h.path, h.backupCount)
		os.Remove(oldest)
		for i := h.backupCount - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", h.path, i)
			dst := fmt.Sprintf("%s.%d", h.path, i+1)
			os.Rename(src, dst)
		}
		os.Rename(h.path, h.path+".1")
	}
	h.size = 0
	h.open()
}
