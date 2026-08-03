// Package app wires the monitors, TUI, and background refresh loop together and
// runs gstop's main event loop. It is the coordinator that neither the monitor
// nor tui packages depend on, so it may import both.
package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"gstop/internal/config"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/monitor"
	"gstop/internal/timing"
)

type refreshSlot struct {
	monitor monitor.Monitor
	busy    atomic.Bool
}

// Refresher runs monitors on a background cadence. Each monitor has an
// independent single-flight slot, so a slow panel neither delays nor queues
// work for any other panel.
type Refresher struct {
	slots  []*refreshSlot
	health *health.Health
	cfg    *config.Config
	logger *logging.Logger

	mu      sync.Mutex
	paused  bool
	stopped bool
	wake    chan struct{}
	root    context.Context
	cancel  context.CancelFunc
	workWG  sync.WaitGroup

	afterRefresh  func()
	emergencyBusy atomic.Bool
	afterWG       sync.WaitGroup
}

// NewRefresher builds a Refresher over the given monitors.
func NewRefresher(monitors []monitor.Monitor, h *health.Health, cfg *config.Config, logger *logging.Logger) *Refresher {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Refresher{
		health: h,
		cfg:    cfg,
		logger: logger,
		wake:   make(chan struct{}, 1),
		root:   ctx,
		cancel: cancel,
	}
	for _, mon := range monitors {
		r.slots = append(r.slots, &refreshSlot{monitor: mon})
	}
	return r
}

// SetAfterRefresh registers an asynchronously invoked refresh-success callback.
func (r *Refresher) SetAfterRefresh(fn func()) { r.afterRefresh = fn }

// Run triggers available monitor slots on a fixed cadence until stopped.
func (r *Refresher) Run() {
	r.triggerIdle()
	interval := time.Duration(r.cfg.GetInt("main.interval", 3)) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.root.Done():
			return
		case <-ticker.C:
			r.triggerIdle()
		case <-r.wake:
			r.triggerIdle()
		}
	}
}

// triggerIdle starts each currently idle slot and returns without waiting. The
// scheduler mutex remains held through WaitGroup.Add so Stop cannot begin Wait
// while an accepted trigger is still registering its work.
func (r *Refresher) triggerIdle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.paused {
		return
	}
	for _, slot := range r.slots {
		if !slot.busy.CompareAndSwap(false, true) {
			continue
		}
		r.workWG.Add(1)
		go r.refreshSlot(slot)
	}
}

func (r *Refresher) refreshSlot(slot *refreshSlot) {
	defer r.workWG.Done()
	defer slot.busy.Store(false)

	started := time.Now()
	previous := monitor.RefreshState{Phase: monitor.RefreshLoading}
	if stateful, ok := slot.monitor.(monitor.RefreshStateMonitor); ok {
		previous = stateful.RefreshState()
		phase := monitor.RefreshRefreshing
		if previous.LastSuccess.IsZero() {
			phase = monitor.RefreshLoading
		}
		stateful.SetRefreshState(monitor.RefreshState{
			Phase:       phase,
			LastAttempt: started,
			LastSuccess: previous.LastSuccess,
		})
	}

	ctx, cancel := context.WithTimeout(r.root, config.CollectTimeout(r.cfg))
	defer cancel()
	var err error
	timing.RefreshAnalyze(
		r.logger,
		slot.monitor.Name(),
		time.Duration(r.cfg.GetFloat("main.refresh_analyze_time_thresh", 3)*float64(time.Second)),
		func() {
			if resultMonitor, ok := slot.monitor.(monitor.ResultContextMonitor); ok {
				err = resultMonitor.RefreshWithContext(ctx)
				return
			}
			if contextMonitor, ok := slot.monitor.(monitor.ContextMonitor); ok {
				contextMonitor.RefreshContext(ctx)
				err = ctx.Err()
				return
			}
			slot.monitor.Refresh()
			err = ctx.Err()
		},
	)

	finished := time.Now()
	state := monitor.RefreshState{
		LastAttempt: started,
		LastSuccess: previous.LastSuccess,
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		state.Phase, state.Error = monitor.RefreshTimeout, "collect_timeout"
	case err != nil:
		state.Phase, state.Error = monitor.RefreshError, err.Error()
	default:
		state.Phase, state.LastSuccess = monitor.RefreshFresh, finished
	}
	if stateful, ok := slot.monitor.(monitor.RefreshStateMonitor); ok {
		stateful.SetRefreshState(state)
	}
	if err == nil {
		if r.health != nil {
			r.health.UpdateRefreshTime()
		}
		r.runAfterRefreshAsync()
	}
}

// runAfterRefreshAsync runs the emergency-analysis hook separately from monitor
// collection. At most one hook is active; further completions skip it.
func (r *Refresher) runAfterRefreshAsync() {
	if r.afterRefresh == nil {
		return
	}
	r.mu.Lock()
	if r.stopped || !r.emergencyBusy.CompareAndSwap(false, true) {
		r.mu.Unlock()
		return
	}
	r.afterWG.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.afterWG.Done()
		defer r.emergencyBusy.Store(false)
		r.afterRefresh()
	}()
}

// RefreshOnce is retained as a compatibility trigger and does not wait.
func (r *Refresher) RefreshOnce() { r.triggerIdle() }

// Pause prevents new slot launches. In-flight monitor work continues.
func (r *Refresher) Pause() {
	r.mu.Lock()
	r.paused = true
	r.mu.Unlock()
}

// Resume allows new slot launches and requests one immediate scheduling pass.
func (r *Refresher) Resume() {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.paused = false
	select {
	case r.wake <- struct{}{}:
	default:
	}
	r.mu.Unlock()
}

// RequestStop prevents new work and cancels all in-flight slot contexts.
func (r *Refresher) RequestStop() {
	r.mu.Lock()
	if !r.stopped {
		r.stopped = true
		r.cancel()
	}
	r.mu.Unlock()
}

// Stop ends the loop and waits for monitor work and after-refresh hooks.
func (r *Refresher) Stop() {
	r.RequestStop()
	r.workWG.Wait()
	r.afterWG.Wait()
}
