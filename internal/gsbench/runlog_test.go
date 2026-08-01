package gsbench

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLogWritesBannerBeforeFirstRecord(t *testing.T) {
	var screen bytes.Buffer
	path := filepath.Join(t.TempDir(), "run.log")
	logger, err := NewRunLog(&screen, path, "v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("ready %d", 1)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), Banner("v0.1.0")) {
		t.Fatalf("log does not start with banner: %q", body)
	}
	if !strings.HasPrefix(screen.String(), Banner("v0.1.0")) {
		t.Fatalf("screen does not start with banner: %q", screen.String())
	}
	if !strings.Contains(string(body), "INFO ready 1") {
		t.Fatalf("missing log record: %q", body)
	}
}

func TestRunLogRedactsSecrets(t *testing.T) {
	var screen bytes.Buffer
	path := filepath.Join(t.TempDir(), "run.log")
	logger, err := NewRunLog(&screen, path, "dev")
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("connect %s", "user='bench' password='secret'")
	_ = logger.Close()
	if strings.Contains(screen.String(), "secret") {
		t.Fatalf("screen leaked secret: %q", screen.String())
	}
}

func TestRunLogPathTrimsIdentityAndStaysUnderConfigLogs(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "release", "configs")
	got, err := runLogPath(configDir, "  run-1  ")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDir, "logs", "gsbench_run-1.log")
	if got != want {
		t.Fatalf("run log path=%q want=%q", got, want)
	}
}

func TestRunLogPathRejectsUnsafeIdentityWithoutCreatingDirectories(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "release", "configs")
	for _, identity := range []string{"", "   ", "../escape", "nested/run", `nested\\run`} {
		t.Run(identity, func(t *testing.T) {
			if _, err := runLogPath(configDir, identity); err == nil {
				t.Fatalf("unsafe identity %q accepted", identity)
			}
		})
	}
	if _, err := os.Lstat(configDir); !os.IsNotExist(err) {
		t.Fatalf("path validation created config directory: %v", err)
	}
}

func TestNewRunLogRejectsSymlinkDirectoryWithoutWritingTarget(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "outside")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDirectory, "run.log")
	original := []byte("outside must stay unchanged\n")
	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "logs")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}

	logger, err := NewRunLog(io.Discard, filepath.Join(linkedDirectory, "run.log"), "dev")
	if err == nil {
		logger.Info("must not reach the linked directory")
		_ = logger.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("NewRunLog() error = %v, want symlink rejection", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("symlink directory target changed: %q", got)
	}
}

func TestNewRunLogRejectsSymlinkFileWithoutWritingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "outside.log")
	original := []byte("outside must stay unchanged\n")
	if err := os.WriteFile(target, original, 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	logger, err := NewRunLog(io.Discard, path, "dev")
	if err == nil {
		logger.Info("must not follow the linked file")
		_ = logger.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("NewRunLog() error = %v, want symlink rejection", err)
	}
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("symlink file target changed: %q", got)
	}
}

func TestNewRunLogCreatesRegularFileAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "run.log")
	first, err := NewRunLog(io.Discard, path, "dev")
	if err != nil {
		t.Fatal(err)
	}
	first.Info("first record")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewRunLog(io.Discard, path, "dev")
	if err != nil {
		t.Fatal(err)
	}
	second.Info("second record")
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("run log mode = %v, want regular file", info.Mode())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(body), Banner("dev")) != 1 {
		t.Fatalf("banner count = %d, want 1: %q", strings.Count(string(body), Banner("dev")), body)
	}
	if !strings.Contains(string(body), "first record") ||
		!strings.Contains(string(body), "second record") {
		t.Fatalf("run log did not append both records: %q", body)
	}
}
