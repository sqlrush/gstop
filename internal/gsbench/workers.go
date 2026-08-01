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
	Operations   int64
	Errors       int64
	FirstError   string
	TotalLatency time.Duration
}

type WorkerGroup struct {
	ctx          context.Context
	cancel       context.CancelFunc
	max          int
	work         WorkFunc
	errorBackoff time.Duration

	mu      sync.Mutex
	slots   map[int]context.CancelFunc
	nextID  int
	target  int
	stopped bool
	wg      sync.WaitGroup

	active     atomic.Int64
	operations atomic.Int64
	errors     atomic.Int64
	latencyNS  atomic.Int64
	errMu      sync.Mutex
	firstError string
}

const workerErrorMaxRunes = 256

func NewWorkerGroup(parent context.Context, maximum int, work WorkFunc) *WorkerGroup {
	ctx, cancel := context.WithCancel(parent)
	return &WorkerGroup{
		ctx: ctx, cancel: cancel, max: maximum, work: work,
		errorBackoff: 10 * time.Millisecond,
		slots:        map[int]context.CancelFunc{},
	}
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
			g.slots[id]()
			delete(g.slots, id)
		}
	}
	g.target = target
	return nil
}

func (g *WorkerGroup) runWorker(ctx context.Context, id int) {
	defer g.wg.Done()
	g.active.Add(1)
	defer g.active.Add(-1)
	for ctx.Err() == nil {
		started := time.Now()
		err := g.work(ctx, id)
		g.latencyNS.Add(time.Since(started).Nanoseconds())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			g.errors.Add(1)
			g.recordFirstError(err)
			if g.errorBackoff > 0 {
				if err := waitContext(ctx, g.errorBackoff); err != nil {
					return
				}
			}
		} else {
			g.operations.Add(1)
		}
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
		Target: target, Active: int(g.active.Load()), Operations: g.operations.Load(),
		Errors: g.errors.Load(), FirstError: firstError,
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
		g.cancel()
		for id, cancel := range g.slots {
			cancel()
			delete(g.slots, id)
		}
	}
	g.mu.Unlock()
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
