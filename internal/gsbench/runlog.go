package gsbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type RunLog struct {
	mu     sync.Mutex
	screen io.Writer
	file   *os.File
	now    func() time.Time
}

func runLogPath(configDir, identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	if err := validateTagComponent("run ID", identity); err != nil {
		return "", err
	}
	configDir = strings.TrimSpace(configDir)
	if configDir == "" {
		return "", fmt.Errorf("config directory is required for run log")
	}
	absoluteDir, err := filepath.Abs(filepath.Clean(configDir))
	if err != nil {
		return "", fmt.Errorf("resolve config directory for run log: %w", err)
	}
	logDir := filepath.Join(absoluteDir, "logs")
	path := filepath.Join(logDir, "gsbench_"+identity+".log")
	relative, err := filepath.Rel(logDir, path)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe run ID %q for log path", identity)
	}
	return filepath.Clean(path), nil
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
	file, err := openRunLogFile(path)
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

func openRunLogFile(path string) (*os.File, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	absolute = canonicalDarwinSystemPath(absolute)

	// Reuse the recovery ledger's descriptor-pinned directory walk so no
	// ancestor can redirect log creation through a symbolic link.
	directory := &fileRecoveryLedger{}
	parent, err := directory.openPinnedParent(absolute, true)
	if err != nil {
		return nil, fmt.Errorf("open trusted directory: %w", err)
	}
	defer unix.Close(parent.descriptor)

	descriptor, err := unix.Openat(
		parent.descriptor,
		parent.targetName,
		unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|
			unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o640,
	)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf(
				"run log %q must not be a symlink",
				parent.targetName,
			)
		}
		return nil, fmt.Errorf("open %q: %w", parent.targetName, err)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		_ = unix.Close(descriptor)
		return nil, fmt.Errorf("inspect %q: %w", parent.targetName, err)
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(descriptor)
		return nil, fmt.Errorf(
			"run log %q must be a regular file",
			parent.targetName,
		)
	}
	return os.NewFile(uintptr(descriptor), absolute), nil
}

func (l *RunLog) Info(format string, args ...any) {
	l.write("INFO", format, args...)
}

func (l *RunLog) Error(format string, args ...any) {
	l.write("ERROR", format, args...)
}

func (l *RunLog) Evidence(envelope EvidenceEnvelope) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal evidence envelope: %w", err)
	}
	l.write("EVIDENCE", "%s", body)
	return nil
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
