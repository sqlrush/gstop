package gsbench

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type RunLog struct {
	mu     sync.Mutex
	screen io.Writer
	file   *os.File
	now    func() time.Time
}

func NewRunLog(screen io.Writer, path, version string) (*RunLog, error) {
	if screen == nil {
		screen = io.Discard
	}
	logger := &RunLog{screen: screen, now: time.Now}
	if _, err := io.WriteString(screen, Banner(version)); err != nil {
		return nil, fmt.Errorf("write screen banner: %w", err)
	}
	if path == "" {
		return logger, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open run log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat run log: %w", err)
	}
	if info.Size() == 0 {
		if _, err := io.WriteString(file, Banner(version)); err != nil {
			file.Close()
			return nil, fmt.Errorf("write log banner: %w", err)
		}
	}
	logger.file = file
	return logger, nil
}

func (l *RunLog) Info(format string, args ...any) {
	l.write("INFO", format, args...)
}

func (l *RunLog) Error(format string, args ...any) {
	l.write("ERROR", format, args...)
}

func (l *RunLog) write(level, format string, args ...any) {
	if l == nil {
		return
	}
	message := RedactDSN(fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	line := fmt.Sprintf("%s %s %s\n", l.now().Format(time.RFC3339), level, message)
	_, _ = io.WriteString(l.screen, line)
	if l.file != nil {
		_, _ = io.WriteString(l.file, line)
	}
}

func (l *RunLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
