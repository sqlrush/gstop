package monitor

import (
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
)

func TestMemoryMonitorDefaultsToFiveSecondInterval(t *testing.T) {
	m := NewMemoryMonitor(Deps{Cfg: config.FromMap(map[string]any{})})
	if got := m.memInterval(); got != 5*time.Second {
		t.Fatalf("memInterval() = %s, want 5s", got)
	}
}

func TestMemoryMonitorRefreshIgnoresDynamicMemoryHealthGate(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_cpu_thresh": int64(50),
		"dynamic_mem_interval":   int64(60),
	}})
	logger := logging.New("memory-refresh-test", "")
	db := dbconn.New(cfg, logger)
	db.Cancel()
	defer db.Close()

	memHealth := health.New(cfg)
	memHealth.UpdateCPUUsage(100)
	m := NewMemoryMonitor(Deps{Cfg: cfg, DB: db, Logger: logger, Health: memHealth})
	m.panels[2] = memPanel{title: "stale session panel"}
	m.panels[3] = memPanel{title: "stale thread panel"}

	m.Refresh()

	if got := m.panels[2].title; got != memPanel2Title {
		t.Fatalf("session panel title = %q, want %q", got, memPanel2Title)
	}
	if got := m.panels[3].title; got != memPanel3Title {
		t.Fatalf("thread panel title = %q, want %q", got, memPanel3Title)
	}
}
