package emergency

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// rawLog is a size-rotated file writer that emits the message verbatim (no
// timestamp/level prefix), matching the Python emergency loggers created with
// fmt="%(message)s". Snapshot dumps and session CSVs are written through it.
type rawLog struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	backupCount int
	file        *os.File
	size        int64
}

// newRawLog opens (creating parents) a raw log at path with a size cap.
func newRawLog(path string, maxBytes int64) *rawLog {
	l := &rawLog{path: path, maxBytes: maxBytes, backupCount: 5}
	l.open()
	return l
}

func (l *rawLog) open() {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	l.file = f
	if info, err := f.Stat(); err == nil {
		l.size = info.Size()
	}
}

// Info writes one line (message plus newline), rotating first if it would exceed
// the size cap.
func (l *rawLog) Info(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	line := msg + "\n"
	if l.maxBytes > 0 && l.size+int64(len(line)) > l.maxBytes {
		l.rollover()
	}
	if n, err := l.file.WriteString(line); err == nil {
		l.size += int64(n)
	}
}

func (l *rawLog) rollover() {
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
	oldest := fmt.Sprintf("%s.%d", l.path, l.backupCount)
	os.Remove(oldest)
	for i := l.backupCount - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	os.Rename(l.path, l.path+".1")
	l.size = 0
	l.open()
}

// Close closes the underlying file.
func (l *rawLog) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}
