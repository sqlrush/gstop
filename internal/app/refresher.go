// Package app wires the monitors, TUI, and background refresh loop together and
// runs gstop's main event loop. It is the coordinator that neither the monitor
// nor tui packages depend on, so it may import both.
package app

import (
	"sync"
	"sync/atomic"
	"time"

	"gstop/internal/config"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/monitor"
	"gstop/internal/timing"
)

// Refresher runs every monitor's Refresh on a background cadence and can be
// paused while a sub-view owns the screen, reproducing GstopRefresher. Refreshes
// run concurrently (one goroutine per monitor) so a slow panel does not delay
// the others, then the liveness heartbeat is stamped.
type Refresher struct {
	monitors []monitor.Monitor
	health   *health.Health
	cfg      *config.Config
	logger   *logging.Logger

	mu      sync.Mutex
	cond    *sync.Cond
	paused  bool
	stopped bool

	// afterRefresh runs after each refresh cycle (emergency analysis hook); it is
	// dispatched asynchronously so a slow scenario cannot stall monitor refreshes.
	afterRefresh  func()
	emergencyBusy atomic.Bool
}

// NewRefresher builds a Refresher over the given monitors.
func NewRefresher(monitors []monitor.Monitor, h *health.Health, cfg *config.Config, logger *logging.Logger) *Refresher {
	r := &Refresher{monitors: monitors, health: h, cfg: cfg, logger: logger}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// SetAfterRefresh registers a callback invoked at the end of each cycle.
func (r *Refresher) SetAfterRefresh(fn func()) { r.afterRefresh = fn }

// Run drives the refresh loop until Stop is called. Intended to run in its own
// goroutine.
func (r *Refresher) Run() {
	for {
		r.mu.Lock()
		for r.paused && !r.stopped {
			r.cond.Wait()
		}
		if r.stopped {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		start := time.Now()
		r.refreshAll()
		r.health.UpdateRefreshTime()
		r.runAfterRefreshAsync()

		interval := time.Duration(r.cfg.GetInt("main.interval", 3)) * time.Second
		if elapsed := time.Since(start); elapsed < interval {
			time.Sleep(interval - elapsed)
		}
	}
}

// runAfterRefreshAsync runs the emergency-analysis hook on its own goroutine so a
// slow scenario (e.g. an inspection query over a large statement history) never
// stalls the monitor refresh cadence. At most one runs at a time; if the previous
// is still in flight this round is skipped.
func (r *Refresher) runAfterRefreshAsync() {
	if r.afterRefresh == nil {
		return
	}
	if !r.emergencyBusy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.emergencyBusy.Store(false)
		r.afterRefresh()
	}()
}

// RefreshOnce runs a single synchronous refresh cycle, used to populate data
// before the first frame is drawn.
func (r *Refresher) RefreshOnce() { r.refreshAll() }

// refreshAll refreshes every monitor concurrently and waits for them all.
func (r *Refresher) refreshAll() {
	var wg sync.WaitGroup
	for _, m := range r.monitors {
		wg.Add(1)
		go func(mon monitor.Monitor) {
			defer wg.Done()
			thresh := time.Duration(r.cfg.GetFloat("main.refresh_analyze_time_thresh", 3) * float64(time.Second))
			timing.RefreshAnalyze(r.logger, mon.Name(), thresh, mon.Refresh)
		}(m)
	}
	wg.Wait()
}

// Pause halts refreshing after the current cycle, used when a sub-view takes over.
func (r *Refresher) Pause() {
	r.mu.Lock()
	r.paused = true
	r.mu.Unlock()
}

// Resume restarts refreshing.
func (r *Refresher) Resume() {
	r.mu.Lock()
	r.paused = false
	r.cond.Signal()
	r.mu.Unlock()
}

// Stop ends the loop and wakes it if paused.
func (r *Refresher) Stop() {
	r.mu.Lock()
	r.stopped = true
	r.cond.Signal()
	r.mu.Unlock()
}
