package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

type MixedScenario struct {
	tp, ap   *sqlWorkload
	run      *fixedWorkerRun
	scanRows int
}

func NewMixedScenario() *MixedScenario              { return &MixedScenario{} }
func (s *MixedScenario) Code() ScenarioCode         { return 103 }
func (s *MixedScenario) Name() string               { return "mixed_cpu" }
func (s *MixedScenario) Strategy() string           { return "mixed_tp_ap_fixed_workers" }
func (s *MixedScenario) OwnsWorkloadDuration() bool { return true }

func (s *MixedScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	tpWorkers := rt.Config.FixedWorkers.MixedTPWorkers
	apWorkers := rt.Config.FixedWorkers.MixedAPWorkers
	s.scanRows = runtimeInt(
		rt, "scenario.mixed_cpu.scan_rows", defaultAPScanRows,
	)
	start := make(chan struct{})
	s.tp = buildTPWorkload(
		ctx, rt, "mixed_cpu_tp", tpWorkers, start,
	)
	ap, err := buildAPWorkload(
		ctx, rt, "mixed_cpu_ap", apWorkers, s.scanRows, start,
	)
	if err != nil {
		return err
	}
	s.ap = ap
	s.run = newFixedWorkerRun(
		rt.Config.Run.Duration,
		start,
		fixedWorkerLane{Name: "tp", Workers: tpWorkers, Workload: s.tp},
		fixedWorkerLane{Name: "ap", Workers: apWorkers, Workload: s.ap},
	)
	if err := prepareMixedSessions(
		ctx, s.tp, tpWorkers, s.ap, apWorkers,
	); err != nil {
		return err
	}
	if rt.Log != nil {
		rt.Log.Info(
			"scenario=%s tp_workers=%d ap_workers=%d duration=%s scan_rows=%d rate=unlimited ap_query_timeout=disabled",
			s.Name(), tpWorkers, apWorkers, rt.Config.Run.Duration, s.scanRows,
		)
	}
	return nil
}

func prepareMixedSessions(
	ctx context.Context,
	tp *sqlWorkload,
	tpWorkers int,
	ap *sqlWorkload,
	apWorkers int,
) error {
	prepareCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, lane := range []struct {
		name     string
		workload *sqlWorkload
		workers  int
	}{
		{name: "tp", workload: tp, workers: tpWorkers},
		{name: "ap", workload: ap, workers: apWorkers},
	} {
		lane := lane
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lane.workload.PrepareSessions(
				prepareCtx, lane.workers,
			); err != nil {
				errs <- fmt.Errorf("prepare %s sessions: %w", lane.name, err)
				cancel()
			}
		}()
	}
	wg.Wait()
	close(errs)
	var result error
	for err := range errs {
		result = errors.Join(result, err)
	}
	return result
}

func (s *MixedScenario) Ramp(ctx context.Context, _ *Runtime) error {
	return s.run.Ramp(ctx)
}

func (s *MixedScenario) Hold(ctx context.Context, rt *Runtime) error {
	return s.run.Hold(ctx, fixedWorkerStopTimeout(rt))
}

func (s *MixedScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyFixedWorkerResult(
		s.Name(),
		s.run,
		map[string]string{"tp": "tp_workers", "ap": "ap_workers"},
	), nil
}

func (s *MixedScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.run.Snapshot()
}

func (s *MixedScenario) RuntimeEvidence() []Evidence {
	return fixedWorkerEvidence(
		s.run,
		map[string]string{"tp": "tp_workers", "ap": "ap_workers"},
	)
}

func (s *MixedScenario) Stop(ctx context.Context, _ *Runtime) error {
	return s.run.Stop(ctx)
}

func (s *MixedScenario) Restore(context.Context, *Runtime) error { return nil }
