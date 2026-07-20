// Package health tracks the process-level throttles and liveness signals that
// the original tool kept as module globals in common/util.py: the last observed
// CPU usage, per-key dynamic-memory refresh timestamps, and the refresh-loop
// heartbeat used to self-terminate a hung process so an external supervisor can
// relaunch it.
package health

import (
	"sync"
	"time"

	"gstop/internal/config"
)

// exitGracePeriod is how long the refresh loop may stall before the process
// decides to exit and wait to be relaunched (5 minutes, matching should_exit).
const exitGracePeriod = 5 * time.Minute

// Health holds shared throttle and heartbeat state. It is safe for concurrent use.
type Health struct {
	cfg *config.Config
	now func() time.Time

	mu           sync.Mutex
	lastCPUUsage float64
	lastMemRef   map[string]time.Time
	lastRefresh  time.Time
}

// New builds a Health bound to cfg for its thresholds.
func New(cfg *config.Config) *Health {
	return &Health{cfg: cfg, now: time.Now, lastMemRef: map[string]time.Time{}}
}

// UpdateCPUUsage records the latest OS CPU usage percentage.
func (h *Health) UpdateCPUUsage(cpu float64) {
	h.mu.Lock()
	h.lastCPUUsage = cpu
	h.mu.Unlock()
}

// ShouldRefreshMemory reports whether the dynamic-memory metric identified by
// key may be refreshed now: never while CPU is at or above the configured
// threshold, and otherwise no more often than dynamic_mem_interval. A true
// result records the refresh time.
func (h *Health) ShouldRefreshMemory(key string) bool {
	threshold := h.cfg.GetFloat("main.dynamic_mem_cpu_thresh", 50)
	interval := time.Duration(h.cfg.GetInt("main.dynamic_mem_interval", 60)) * time.Second

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastCPUUsage >= threshold {
		return false
	}
	last, ok := h.lastMemRef[key]
	if !ok || h.now().Sub(last) >= interval {
		h.lastMemRef[key] = h.now()
		return true
	}
	return false
}

// UpdateRefreshTime records a completed refresh cycle (the liveness heartbeat).
func (h *Health) UpdateRefreshTime() {
	h.mu.Lock()
	h.lastRefresh = h.now()
	h.mu.Unlock()
}

// ShouldExit reports whether the refresh loop has been silent long enough that
// the process should exit and be relaunched. It only fires after at least one
// refresh has been recorded.
func (h *Health) ShouldExit() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.lastRefresh.IsZero() && h.now().Sub(h.lastRefresh) >= exitGracePeriod
}
