package gsbench

import (
	"context"
	"database/sql"
	"fmt"
)

const defaultAPScanRows = 1_000_000

func APStatements(schema string, scanRows int) ([]string, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	if scanRows <= 0 {
		return nil, fmt.Errorf("scan_rows must be positive")
	}
	fact := fmt.Sprintf(
		"(SELECT id,sale_date,customer_id,product_id,store_id,amount,quantity FROM %s.fact_sales LIMIT %d)",
		schema, scanRows,
	)
	return []string{
		`SELECT /*+ set(query_dop 1) */ p.category_id, count(*), sum(f.amount), avg(f.quantity) FROM ` + fact + ` f JOIN ` + schema + `.dim_product p ON p.id=f.product_id GROUP BY p.category_id ORDER BY sum(f.amount) DESC LIMIT 100`,
		`SELECT /*+ set(query_dop 1) */ c.region_id, f.store_id, sum(f.amount), count(*) FROM ` + fact + ` f JOIN ` + schema + `.customers c ON c.id=f.customer_id GROUP BY c.region_id,f.store_id ORDER BY count(*) DESC LIMIT 200`,
		`SELECT /*+ set(query_dop 1) */ f.sale_date, sum(f.amount), avg(f.amount), max(f.amount) FROM ` + fact + ` f GROUP BY f.sale_date ORDER BY sum(f.amount) DESC`,
	}, nil
}

func buildAPWorkload(
	ctx context.Context,
	rt *Runtime,
	name string,
	workers int,
	scanRows int,
	start <-chan struct{},
) (*sqlWorkload, error) {
	statements, err := APStatements(rt.Config.Data.Schema, scanRows)
	if err != nil {
		return nil, err
	}
	workload := newSQLWorkloadWithoutOperationTimeoutWithStartGate(
		ctx,
		rt,
		name,
		workers,
		func(ctx context.Context, conn *sql.Conn, workerID int) error {
			rows, err := conn.QueryContext(
				ctx,
				statements[workerID%len(statements)],
			)
			if err != nil {
				return err
			}
			defer rows.Close()
			return consumeRows(rows)
		},
		start,
	)
	return workload, nil
}

type APScenario struct {
	workload *sqlWorkload
	run      *fixedWorkerRun
	scanRows int
}

func NewAPScenario() *APScenario                 { return &APScenario{} }
func (s *APScenario) Code() ScenarioCode         { return 102 }
func (s *APScenario) Name() string               { return "ap_cpu" }
func (s *APScenario) Strategy() string           { return "ap_sql_fixed_workers" }
func (s *APScenario) OwnsWorkloadDuration() bool { return true }

func (s *APScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	workers := rt.Config.FixedWorkers.APWorkers
	s.scanRows = runtimeInt(
		rt, "scenario.ap_cpu.scan_rows", defaultAPScanRows,
	)
	start := make(chan struct{})
	workload, err := buildAPWorkload(
		ctx, rt, s.Name(), workers, s.scanRows, start,
	)
	if err != nil {
		return err
	}
	s.workload = workload
	s.run = newFixedWorkerRun(
		rt.Config.Run.Duration,
		start,
		fixedWorkerLane{Name: "ap", Workers: workers, Workload: workload},
	)
	if err := workload.PrepareSessions(ctx, workers); err != nil {
		return err
	}
	if rt.Log != nil {
		rt.Log.Info(
			"scenario=%s workers=%d duration=%s scan_rows=%d rate=unlimited query_timeout=disabled",
			s.Name(), workers, rt.Config.Run.Duration, s.scanRows,
		)
	}
	return nil
}

func (s *APScenario) Ramp(ctx context.Context, _ *Runtime) error {
	return s.run.Ramp(ctx)
}

func (s *APScenario) Hold(ctx context.Context, rt *Runtime) error {
	return s.run.Hold(ctx, fixedWorkerStopTimeout(rt))
}

func (s *APScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyFixedWorkerResult(
		s.Name(), s.run, map[string]string{"ap": "workers"},
	), nil
}

func (s *APScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.run.Snapshot()
}

func (s *APScenario) RuntimeEvidence() []Evidence {
	return fixedWorkerEvidence(s.run, map[string]string{"ap": "workers"})
}

func (s *APScenario) Stop(ctx context.Context, _ *Runtime) error {
	return s.run.Stop(ctx)
}

func (s *APScenario) Restore(context.Context, *Runtime) error { return nil }
