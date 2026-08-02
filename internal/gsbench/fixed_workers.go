package gsbench

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type fixedWorkerWorkload interface {
	SetTarget(int) error
	WaitReady(context.Context, int) error
	SetRunDeadline(time.Time) error
	Snapshot() WorkerSnapshot
	Stop(context.Context) error
}

type fixedWorkerLane struct {
	Name     string
	Workers  int
	Workload fixedWorkerWorkload
}

// fixedWorkerRun implements sysbench's unlimited-rate execution model: fixed
// workers wait behind one initialization barrier, execute operations back to
// back for exactly duration, then stop together without draining queued work.
type fixedWorkerRun struct {
	duration time.Duration
	start    chan struct{}
	lanes    []fixedWorkerLane

	releaseOnce sync.Once
	stopOnce    sync.Once
	stopDone    chan struct{}

	mu        sync.Mutex
	startedAt time.Time
	endedAt   time.Time
	final     map[string]WorkerSnapshot
	stopErr   error
}

func fixedWorkerStopTimeout(rt *Runtime) time.Duration {
	if rt != nil && rt.Config.Safety.QueryTimeout > 0 {
		return rt.Config.Safety.QueryTimeout
	}
	return 30 * time.Second
}

func newFixedWorkerRun(
	duration time.Duration,
	start chan struct{},
	lanes ...fixedWorkerLane,
) *fixedWorkerRun {
	if start == nil {
		start = make(chan struct{})
	}
	return &fixedWorkerRun{
		duration: duration,
		start:    start,
		lanes:    append([]fixedWorkerLane(nil), lanes...),
		stopDone: make(chan struct{}),
		final:    make(map[string]WorkerSnapshot, len(lanes)),
	}
}

func (r *fixedWorkerRun) Ramp(ctx context.Context) error {
	if r == nil {
		return fmt.Errorf("fixed worker run is unavailable")
	}
	if r.duration <= 0 {
		return fmt.Errorf("fixed worker duration must be positive")
	}
	for _, lane := range r.lanes {
		if lane.Name == "" || lane.Workers <= 0 || lane.Workload == nil {
			return fmt.Errorf("invalid fixed worker lane %q", lane.Name)
		}
		if err := lane.Workload.SetTarget(lane.Workers); err != nil {
			return fmt.Errorf("start %s workers: %w", lane.Name, err)
		}
	}
	for _, lane := range r.lanes {
		if err := lane.Workload.WaitReady(ctx, lane.Workers); err != nil {
			return fmt.Errorf("initialize %s workers: %w", lane.Name, err)
		}
	}
	return nil
}

// ReleaseWorkers lets workloads with an expensive one-time initialization
// step build their pressure operator before the configured duration starts.
// Hold calls it as well, so ordinary fixed-worker scenarios retain the same
// single barrier behavior.
func (r *fixedWorkerRun) ReleaseWorkers() {
	if r == nil {
		return
	}
	r.releaseOnce.Do(func() { close(r.start) })
}

func (r *fixedWorkerRun) Hold(ctx context.Context, stopTimeout time.Duration) error {
	if r == nil {
		return fmt.Errorf("fixed worker run is unavailable")
	}
	if stopTimeout <= 0 {
		stopTimeout = 30 * time.Second
	}
	startedAt := time.Now()
	deadline := startedAt.Add(r.duration)
	r.mu.Lock()
	r.startedAt = startedAt
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		r.mu.Lock()
		r.endedAt = startedAt
		r.mu.Unlock()
		stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		return errors.Join(err, r.Stop(stopCtx))
	}
	for _, lane := range r.lanes {
		if err := lane.Workload.SetRunDeadline(deadline); err != nil {
			r.mu.Lock()
			r.endedAt = time.Now()
			r.mu.Unlock()
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			return errors.Join(
				fmt.Errorf("set %s worker deadline: %w", lane.Name, err),
				r.Stop(stopCtx),
			)
		}
	}
	r.ReleaseWorkers()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	var runErr error
	endedAt := deadline
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
		endedAt = time.Now()
	case <-timer.C:
	}
	r.mu.Lock()
	r.endedAt = endedAt
	r.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return errors.Join(runErr, r.Stop(stopCtx))
}

func (r *fixedWorkerRun) Stop(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.stopOnce.Do(func() {
		go r.stopAll()
	})
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-r.stopDone:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *fixedWorkerRun) stopAll() {
	defer close(r.stopDone)
	type laneResult struct {
		name     string
		workers  int
		snapshot WorkerSnapshot
		err      error
	}
	results := make(chan laneResult, len(r.lanes))
	var wg sync.WaitGroup
	for _, lane := range r.lanes {
		lane := lane
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := lane.Workload.Stop(context.Background())
			snapshot := lane.Workload.Snapshot()
			snapshot.Target = lane.Workers
			results <- laneResult{
				name: lane.Name, workers: lane.Workers,
				snapshot: snapshot, err: err,
			}
		}()
	}
	wg.Wait()
	close(results)
	r.mu.Lock()
	defer r.mu.Unlock()
	for result := range results {
		r.final[result.name] = result.snapshot
		if result.err != nil {
			r.stopErr = errors.Join(
				r.stopErr,
				fmt.Errorf("stop %s workers: %w", result.name, result.err),
			)
		}
	}
}

func (r *fixedWorkerRun) LaneSnapshot(name string) WorkerSnapshot {
	if r == nil {
		return WorkerSnapshot{}
	}
	r.mu.Lock()
	if snapshot, ok := r.final[name]; ok {
		r.mu.Unlock()
		return snapshot
	}
	r.mu.Unlock()
	for _, lane := range r.lanes {
		if lane.Name == name && lane.Workload != nil {
			snapshot := lane.Workload.Snapshot()
			snapshot.Target = lane.Workers
			return snapshot
		}
	}
	return WorkerSnapshot{}
}

func (r *fixedWorkerRun) Snapshot() WorkerSnapshot {
	if r == nil {
		return WorkerSnapshot{}
	}
	var combined WorkerSnapshot
	for _, lane := range r.lanes {
		snapshot := r.LaneSnapshot(lane.Name)
		combined.Target += snapshot.Target
		combined.Active += snapshot.Active
		combined.Started += snapshot.Started
		combined.PeakActive += snapshot.PeakActive
		combined.Operations += snapshot.Operations
		combined.Errors += snapshot.Errors
		combined.TotalLatency += snapshot.TotalLatency
		if combined.FirstError == "" {
			combined.FirstError = snapshot.FirstError
		}
	}
	return combined
}

func (r *fixedWorkerRun) Elapsed() time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.startedAt.IsZero() {
		return 0
	}
	if r.endedAt.IsZero() {
		return time.Since(r.startedAt)
	}
	return r.endedAt.Sub(r.startedAt)
}

func fixedWorkerEvidence(
	run *fixedWorkerRun,
	laneMetrics map[string]string,
) []Evidence {
	if run == nil {
		return nil
	}
	evidence := make([]Evidence, 0, len(run.lanes)+3)
	for _, lane := range run.lanes {
		snapshot := run.LaneSnapshot(lane.Name)
		metric := laneMetrics[lane.Name]
		if metric == "" {
			metric = lane.Name + "_workers"
		}
		evidence = append(evidence, Evidence{
			Metric: metric, Target: float64(lane.Workers),
			Actual: float64(snapshot.PeakActive), Available: true,
			Details: map[string]any{
				"started":     snapshot.Started,
				"active":      snapshot.Active,
				"operations":  snapshot.Operations,
				"errors":      snapshot.Errors,
				"first_error": snapshot.FirstError,
			},
		})
	}
	combined := run.Snapshot()
	if len(run.lanes) > 1 {
		evidence = append(evidence, Evidence{
			Metric: "workers", Target: float64(combined.Target),
			Actual: float64(combined.PeakActive), Available: true,
		})
	}
	elapsed := run.Elapsed()
	evidence = append(evidence, Evidence{
		Metric: "duration_seconds", Target: run.duration.Seconds(),
		Actual: elapsed.Seconds(), Available: true,
	})
	throughput := 0.0
	if elapsed > 0 {
		throughput = float64(combined.Operations) / elapsed.Seconds()
	}
	evidence = append(evidence, Evidence{
		Metric: "operations_per_second", Actual: throughput, Available: true,
	})
	return evidence
}

func verifyFixedWorkerResult(
	name string,
	run *fixedWorkerRun,
	laneMetrics map[string]string,
) Result {
	result := Result{
		Scenario: name, Outcome: OutcomeFailed,
		Evidence: fixedWorkerEvidence(run, laneMetrics),
	}
	if run == nil {
		result.Message = "fixed worker run is unavailable"
		return result
	}
	for _, lane := range run.lanes {
		snapshot := run.LaneSnapshot(lane.Name)
		if snapshot.Errors > 0 {
			result.Message = fmt.Sprintf(
				"%s workload errors=%d first_error=%s",
				lane.Name,
				snapshot.Errors,
				snapshot.FirstError,
			)
			return result
		}
		if snapshot.Started != lane.Workers || snapshot.PeakActive != lane.Workers {
			result.Message = fmt.Sprintf(
				"%s workers requested=%d started=%d peak_active=%d",
				lane.Name,
				lane.Workers,
				snapshot.Started,
				snapshot.PeakActive,
			)
			return result
		}
	}
	result.Outcome = OutcomeSuccess
	result.Message = fmt.Sprintf(
		"fixed workers completed for %s with %d workers over %.3fs",
		name,
		run.Snapshot().Target,
		run.Elapsed().Seconds(),
	)
	return result
}
