package gsbench

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

type WorkFunc func(ctx context.Context, workerID int) error

type WorkerSnapshot struct {
	Target       int
	Active       int
	Started      int
	PeakActive   int
	Operations   int64
	Errors       int64
	FirstError   string
	TotalLatency time.Duration
}

type WorkerGroup struct {
	ctx      context.Context
	cancel   context.CancelFunc
	max      int
	work     WorkFunc
	start    <-chan struct{}
	state    chan struct{}
	mu       sync.Mutex
	slots    map[int]context.CancelFunc
	retiring map[int]bool
	onRetire func(int)
	nextID   int
	target   int
	stopped  bool
	wg       sync.WaitGroup

	active        atomic.Int64
	started       atomic.Int64
	peakActive    atomic.Int64
	operations    atomic.Int64
	errors        atomic.Int64
	latencyNS     atomic.Int64
	deadlineNS    atomic.Int64
	closed        atomic.Bool
	errMu         sync.Mutex
	firstError    string
	deadlineMu    sync.Mutex
	deadlineTimer *time.Timer
}

const workerErrorMaxRunes = 256

func NewWorkerGroup(parent context.Context, maximum int, work WorkFunc) *WorkerGroup {
	start := make(chan struct{})
	close(start)
	return NewWorkerGroupWithStartGate(parent, maximum, work, start)
}

// NewWorkerGroupWithStartGate creates workers that initialize and then wait
// for start to close before executing their first operation. A shared gate can
// release multiple worker groups at the same instant, matching sysbench's
// worker initialization barrier.
func NewWorkerGroupWithStartGate(
	parent context.Context,
	maximum int,
	work WorkFunc,
	start <-chan struct{},
) *WorkerGroup {
	if start == nil {
		ready := make(chan struct{})
		close(ready)
		start = ready
	}
	ctx, cancel := context.WithCancel(parent)
	return &WorkerGroup{
		ctx: ctx, cancel: cancel, max: maximum, work: work,
		start: start, state: make(chan struct{}, 1),
		slots:    map[int]context.CancelFunc{},
		retiring: map[int]bool{},
	}
}

func (g *WorkerGroup) SetRetireHook(hook func(int)) {
	g.mu.Lock()
	g.onRetire = hook
	g.mu.Unlock()
}

func (g *WorkerGroup) Target() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.target
}

func (g *WorkerGroup) SetTarget(target int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return fmt.Errorf("worker group is stopped")
	}
	if target < 0 || target > g.max {
		return fmt.Errorf("worker target %d exceeds range 0..%d", target, g.max)
	}
	current := len(g.slots)
	if target > current {
		for range target - current {
			id := g.nextID
			g.nextID++
			workerCtx, cancel := context.WithCancel(g.ctx)
			g.slots[id] = cancel
			g.wg.Add(1)
			go g.runWorker(workerCtx, id)
		}
	} else if target < current {
		ids := make([]int, 0, len(g.slots))
		for id := range g.slots {
			ids = append(ids, id)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ids)))
		for _, id := range ids[:current-target] {
			g.retiring[id] = true
			g.slots[id]()
			delete(g.slots, id)
		}
	}
	g.target = target
	return nil
}

func (g *WorkerGroup) runWorker(ctx context.Context, id int) {
	defer func() {
		g.retireWorker(id)
		g.wg.Done()
	}()
	active := g.active.Add(1)
	g.started.Add(1)
	g.updatePeakActive(active)
	g.notifyState()
	defer func() {
		g.active.Add(-1)
		g.notifyState()
	}()
	select {
	case <-ctx.Done():
		return
	case <-g.start:
	}
	for ctx.Err() == nil && g.canStartOperation() {
		started := time.Now()
		err := g.work(ctx, id)
		g.latencyNS.Add(time.Since(started).Nanoseconds())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			g.errors.Add(1)
			g.recordFirstError(err)
			return
		} else {
			g.operations.Add(1)
		}
	}
}

func (g *WorkerGroup) updatePeakActive(active int64) {
	for {
		peak := g.peakActive.Load()
		if active <= peak || g.peakActive.CompareAndSwap(peak, active) {
			return
		}
	}
}

func (g *WorkerGroup) notifyState() {
	select {
	case g.state <- struct{}{}:
	default:
	}
}

// WaitReady waits until target worker goroutines have initialized. Gated
// workers have not executed any workload operation when this returns.
func (g *WorkerGroup) WaitReady(ctx context.Context, target int) error {
	if target < 0 || target > g.max {
		return fmt.Errorf("worker ready target %d exceeds range 0..%d", target, g.max)
	}
	for int(g.started.Load()) < target {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.ctx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("worker group stopped before %d workers initialized", target)
		case <-g.state:
		}
	}
	return nil
}

// SetRunDeadline installs the fixed-run cutoff before the shared start gate is
// released. The worker loop checks the absolute deadline before every new
// operation, while the timer cancels any operation still in flight at cutoff.
func (g *WorkerGroup) SetRunDeadline(deadline time.Time) error {
	if deadline.IsZero() {
		return fmt.Errorf("worker run deadline is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return fmt.Errorf("worker group is stopped")
	}
	g.deadlineNS.Store(deadline.UnixNano())
	g.deadlineMu.Lock()
	if g.deadlineTimer != nil {
		g.deadlineTimer.Stop()
	}
	delay := time.Until(deadline)
	if delay <= 0 {
		g.deadlineTimer = nil
		g.deadlineMu.Unlock()
		g.closeInjection()
		return nil
	}
	g.deadlineTimer = time.AfterFunc(delay, g.closeInjection)
	g.deadlineMu.Unlock()
	return nil
}

func (g *WorkerGroup) closeInjection() {
	g.closed.Store(true)
	g.cancel()
	g.notifyState()
}

func (g *WorkerGroup) canStartOperation() bool {
	if g.closed.Load() {
		return false
	}
	deadlineNS := g.deadlineNS.Load()
	if deadlineNS > 0 && time.Now().UnixNano() >= deadlineNS {
		g.closeInjection()
		return false
	}
	return true
}

func (g *WorkerGroup) retireWorker(id int) {
	g.mu.Lock()
	retiring := g.retiring[id]
	delete(g.retiring, id)
	hook := g.onRetire
	g.mu.Unlock()
	if retiring && hook != nil {
		hook(id)
	}
}

func (g *WorkerGroup) Snapshot() WorkerSnapshot {
	g.mu.Lock()
	target := g.target
	g.mu.Unlock()
	g.errMu.Lock()
	firstError := g.firstError
	g.errMu.Unlock()
	return WorkerSnapshot{
		Target: target, Active: int(g.active.Load()),
		Started: int(g.started.Load()), PeakActive: int(g.peakActive.Load()),
		Operations: g.operations.Load(),
		Errors:     g.errors.Load(), FirstError: firstError,
		TotalLatency: time.Duration(g.latencyNS.Load()),
	}
}

func (g *WorkerGroup) recordFirstError(err error) {
	if err == nil {
		return
	}
	g.errMu.Lock()
	defer g.errMu.Unlock()
	if g.firstError != "" {
		return
	}
	text := journalSafeErrorText(err.Error())
	if len(text) > workerErrorMaxRunes {
		limit := workerErrorMaxRunes - len("…")
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "…"
	}
	g.firstError = text
}

func (g *WorkerGroup) ExecutionError() error {
	snapshot := g.Snapshot()
	if snapshot.Errors == 0 {
		return nil
	}
	return fmt.Errorf(
		"workload execution errors=%d first_error=%s",
		snapshot.Errors,
		snapshot.FirstError,
	)
}

func (g *WorkerGroup) Stop(ctx context.Context) error {
	g.mu.Lock()
	if !g.stopped {
		g.stopped = true
		g.target = 0
		for id, cancel := range g.slots {
			cancel()
			delete(g.slots, id)
		}
	}
	g.mu.Unlock()
	g.deadlineMu.Lock()
	if g.deadlineTimer != nil {
		g.deadlineTimer.Stop()
		g.deadlineTimer = nil
	}
	g.deadlineMu.Unlock()
	g.closeInjection()
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
