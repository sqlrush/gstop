package health

import (
	"testing"
	"time"

	"gstop/internal/config"
)

func TestMemoryRefreshEligibilityDoesNotConsumeThrottleUntilCommitted(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_cpu_thresh": int64(50),
		"dynamic_mem_interval":   int64(60),
	}})
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.Local)
	h := New(cfg)
	h.now = func() time.Time { return now }

	if !h.CanRefreshMemory("pcache") || !h.CanRefreshMemory("pcache") {
		t.Fatal("eligibility check consumed the pcache throttle")
	}
	h.CommitMemoryRefresh("pcache")
	if h.CanRefreshMemory("pcache") {
		t.Fatal("committed pcache refresh remained immediately eligible")
	}
	now = now.Add(61 * time.Second)
	if !h.CanRefreshMemory("pcache") {
		t.Fatal("pcache refresh did not become eligible after interval")
	}
}

func TestShouldRefreshMemoryStillChecksAndCommitsAtomically(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_cpu_thresh": int64(50),
		"dynamic_mem_interval":   int64(60),
	}})
	h := New(cfg)
	h.now = func() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.Local) }

	if !h.ShouldRefreshMemory("memory") {
		t.Fatal("first atomic refresh check was rejected")
	}
	if h.ShouldRefreshMemory("memory") {
		t.Fatal("second atomic refresh check ignored the committed throttle")
	}
}
