package gsbench

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fixedWorkerGroupAdapter struct{ group *WorkerGroup }

func (w fixedWorkerGroupAdapter) SetTarget(target int) error {
	return w.group.SetTarget(target)
}

func (w fixedWorkerGroupAdapter) WaitReady(ctx context.Context, target int) error {
	return w.group.WaitReady(ctx, target)
}

func (w fixedWorkerGroupAdapter) SetRunDeadline(deadline time.Time) error {
	return w.group.SetRunDeadline(deadline)
}

func (w fixedWorkerGroupAdapter) Snapshot() WorkerSnapshot {
	return w.group.Snapshot()
}

func (w fixedWorkerGroupAdapter) Stop(ctx context.Context) error {
	return w.group.Stop(ctx)
}

func TestFixedWorkerRunStagesReleasesAndStopsTwoLanes(t *testing.T) {
	start := make(chan struct{})
	var tpOperations, apOperations atomic.Int64
	newLane := func(name string, workers int, operations *atomic.Int64) fixedWorkerLane {
		group := NewWorkerGroupWithStartGate(
			context.Background(),
			workers,
			func(ctx context.Context, _ int) error {
				operations.Add(1)
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					return nil
				}
			},
			start,
		)
		return fixedWorkerLane{
			Name: name, Workers: workers,
			Workload: fixedWorkerGroupAdapter{group: group},
		}
	}
	run := newFixedWorkerRun(
		35*time.Millisecond,
		start,
		newLane("tp", 2, &tpOperations),
		newLane("ap", 1, &apOperations),
	)
	readyCtx, cancelReady := context.WithTimeout(context.Background(), time.Second)
	defer cancelReady()
	if err := run.Ramp(readyCtx); err != nil {
		t.Fatal(err)
	}
	if tpOperations.Load() != 0 || apOperations.Load() != 0 {
		t.Fatalf("work started before Hold: tp=%d ap=%d", tpOperations.Load(), apOperations.Load())
	}

	started := time.Now()
	if err := run.Hold(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 30*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("fixed run elapsed=%s, want configured duration", elapsed)
	}
	if tpOperations.Load() == 0 || apOperations.Load() == 0 {
		t.Fatalf("one or more lanes did not execute: tp=%d ap=%d", tpOperations.Load(), apOperations.Load())
	}
	for _, check := range []struct {
		name      string
		requested int
	}{
		{name: "tp", requested: 2},
		{name: "ap", requested: 1},
	} {
		snapshot := run.LaneSnapshot(check.name)
		if snapshot.Target != check.requested || snapshot.Started != check.requested ||
			snapshot.PeakActive != check.requested || snapshot.Active != 0 {
			t.Fatalf("lane %s snapshot=%+v, want requested/started/peak=%d and active=0", check.name, snapshot, check.requested)
		}
	}
	stoppedTP, stoppedAP := tpOperations.Load(), apOperations.Load()
	time.Sleep(10 * time.Millisecond)
	if tpOperations.Load() != stoppedTP || apOperations.Load() != stoppedAP {
		t.Fatalf("traffic continued after Hold: tp=%d->%d ap=%d->%d", stoppedTP, tpOperations.Load(), stoppedAP, apOperations.Load())
	}
	if elapsed := run.Elapsed(); elapsed < 30*time.Millisecond || elapsed > 100*time.Millisecond {
		t.Fatalf("recorded pressure duration=%s", elapsed)
	}
}

func TestFixedWorkerRunStopsOnCallerCancellation(t *testing.T) {
	start := make(chan struct{})
	group := NewWorkerGroupWithStartGate(
		context.Background(),
		1,
		func(ctx context.Context, _ int) error {
			<-ctx.Done()
			return ctx.Err()
		},
		start,
	)
	run := newFixedWorkerRun(
		time.Minute,
		start,
		fixedWorkerLane{
			Name: "tp", Workers: 1,
			Workload: fixedWorkerGroupAdapter{group: group},
		},
	)
	if err := run.Ramp(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run.Hold(ctx, time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("Hold error=%v, want context canceled", err)
	}
	if snapshot := run.LaneSnapshot("tp"); snapshot.Active != 0 {
		t.Fatalf("canceled run left active workers: %+v", snapshot)
	}
}

type blockingFixedWorkerWorkload struct {
	release chan struct{}
	called  chan struct{}
}

func (w *blockingFixedWorkerWorkload) SetTarget(int) error { return nil }

func (w *blockingFixedWorkerWorkload) WaitReady(context.Context, int) error { return nil }

func (w *blockingFixedWorkerWorkload) SetRunDeadline(time.Time) error { return nil }

func (w *blockingFixedWorkerWorkload) Snapshot() WorkerSnapshot {
	return WorkerSnapshot{Started: 1, PeakActive: 1}
}

func (w *blockingFixedWorkerWorkload) Stop(context.Context) error {
	select {
	case w.called <- struct{}{}:
	default:
	}
	<-w.release
	return nil
}

func TestFixedWorkerRunStopHonorsEachCallerContextAndContinuesJoining(t *testing.T) {
	workload := &blockingFixedWorkerWorkload{
		release: make(chan struct{}),
		called:  make(chan struct{}, 1),
	}
	run := newFixedWorkerRun(
		time.Second,
		make(chan struct{}),
		fixedWorkerLane{Name: "tp", Workers: 1, Workload: workload},
	)
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	started := time.Now()
	if err := run.Stop(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Stop error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("first Stop ignored caller deadline: elapsed=%s", elapsed)
	}
	select {
	case <-workload.called:
	case <-time.After(time.Second):
		t.Fatal("background stop was not started")
	}
	close(workload.release)
	longCtx, cancelLong := context.WithTimeout(context.Background(), time.Second)
	defer cancelLong()
	if err := run.Stop(longCtx); err != nil {
		t.Fatalf("second Stop did not finish the original join: %v", err)
	}
	if snapshot := run.LaneSnapshot("tp"); snapshot.Active != 0 || snapshot.Target != 1 {
		t.Fatalf("final snapshot=%+v", snapshot)
	}
}
