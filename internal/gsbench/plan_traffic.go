package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

type planTraffic struct {
	workload *sqlWorkload
	workers  int
	start    chan struct{}
	startOne sync.Once
}

func planCandidateIndex(workerID, operation, candidateCount int) int {
	if candidateCount <= 0 {
		return -1
	}
	return (workerID + operation) % candidateCount
}

func newPlanTraffic(
	ctx context.Context,
	runtime *Runtime,
	definition PlanScenarioDefinition,
	workers int,
) (*planTraffic, error) {
	if runtime == nil {
		return nil, fmt.Errorf("plan traffic runtime is required")
	}
	if workers <= 0 {
		return nil, fmt.Errorf("plan traffic workers must be positive")
	}
	if len(definition.Candidates) == 0 {
		return nil, fmt.Errorf(
			"plan scenario %03d has no workload candidates",
			definition.Code,
		)
	}
	start := make(chan struct{})
	counters := make([]int, workers)
	op := func(
		opCtx context.Context,
		conn sqlQueryConn,
		workerID int,
	) error {
		index := planCandidateIndex(
			workerID,
			counters[workerID],
			len(definition.Candidates),
		)
		counters[workerID]++
		rows, err := conn.QueryContext(opCtx, definition.Candidates[index])
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	}
	workload := newSQLWorkloadWithoutOperationTimeoutWithStartGate(
		ctx,
		runtime,
		definition.Name,
		workers,
		func(opCtx context.Context, conn *sql.Conn, workerID int) error {
			return op(opCtx, conn, workerID)
		},
		start,
	)
	return &planTraffic{
		workload: workload,
		workers:  workers,
		start:    start,
	}, nil
}

type sqlQueryConn interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (t *planTraffic) Run(
	ctx context.Context,
	duration time.Duration,
) (WorkerSnapshot, error) {
	return t.RunWithReady(ctx, duration, nil)
}

func (t *planTraffic) RunWithReady(
	ctx context.Context,
	duration time.Duration,
	onReady func(context.Context) error,
) (WorkerSnapshot, error) {
	if t == nil || t.workload == nil {
		return WorkerSnapshot{}, fmt.Errorf("plan traffic is unavailable")
	}
	if duration <= 0 {
		return WorkerSnapshot{}, fmt.Errorf("plan traffic duration must be positive")
	}
	if err := t.workload.PrepareSessions(ctx, t.workers); err != nil {
		_ = t.stop()
		return t.workload.Snapshot(), err
	}
	if err := t.workload.SetTarget(t.workers); err != nil {
		_ = t.stop()
		return t.workload.Snapshot(), err
	}
	if err := t.workload.WaitReady(ctx, t.workers); err != nil {
		_ = t.stop()
		return t.workload.Snapshot(), err
	}
	if onReady != nil {
		if err := onReady(ctx); err != nil {
			_ = t.stop()
			return t.workload.Snapshot(), fmt.Errorf(
				"announce plan traffic readiness: %w",
				err,
			)
		}
	}
	deadline := time.Now().Add(duration)
	if err := t.workload.SetRunDeadline(deadline); err != nil {
		_ = t.stop()
		return t.workload.Snapshot(), err
	}
	t.startOne.Do(func() { close(t.start) })
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var runErr error
	select {
	case <-ctx.Done():
		runErr = ctx.Err()
	case <-timer.C:
	}
	stopErr := t.stop()
	snapshot := t.workload.Snapshot()
	return snapshot, errors.Join(runErr, workerSnapshotError(snapshot), stopErr)
}

func (t *planTraffic) stop() error {
	timeout := 30 * time.Second
	if t != nil && t.workload != nil && t.workload.runtime != nil &&
		t.workload.runtime.Config.Safety.QueryTimeout > 0 {
		timeout = t.workload.runtime.Config.Safety.QueryTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return t.workload.Stop(ctx)
}
