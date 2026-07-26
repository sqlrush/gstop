package gsbench

import (
	"bytes"
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
