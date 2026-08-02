package gsbench

import (
	"context"
	"database/sql"
	"math/rand"
	"strconv"
	"sync/atomic"
	"time"
)

func TPStatements(schema string, id, orderID int64, balance float64) []string {
	idText := strconv.FormatInt(id, 10)
	distKeyText := strconv.FormatInt(id+1, 10)
	orderText := strconv.FormatInt(orderID, 10)
	balanceText := strconv.FormatFloat(balance, 'f', 2, 64)
	return []string{
		"SELECT balance FROM " + schema + ".accounts WHERE dist_key=" + distKeyText + " AND id=" + idText,
		"UPDATE " + schema + ".accounts SET balance=balance+1,updated_at=current_timestamp WHERE dist_key=" + distKeyText + " AND id=" + idText,
		"INSERT INTO " + schema + ".orders(dist_key,id,customer_id,status,amount,created_at) VALUES(" +
			distKeyText + "," + orderText + "," + idText + ",0," + balanceText + ",current_timestamp)",
	}
}

type TPScenario struct {
	workload *sqlWorkload
	run      *fixedWorkerRun
}

func NewTPScenario() *TPScenario                 { return &TPScenario{} }
func (s *TPScenario) Code() ScenarioCode         { return 101 }
func (s *TPScenario) Name() string               { return "tp_cpu" }
func (s *TPScenario) Strategy() string           { return "tp_sql_fixed_workers" }
func (s *TPScenario) OwnsWorkloadDuration() bool { return true }

func (s *TPScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt == nil || rt.Database == nil {
		return sql.ErrConnDone
	}
	workers := rt.Config.FixedWorkers.TPWorkers
	start := make(chan struct{})
	s.workload = buildTPWorkload(ctx, rt, s.Name(), workers, start)
	s.run = newFixedWorkerRun(
		rt.Config.Run.Duration,
		start,
		fixedWorkerLane{Name: "tp", Workers: workers, Workload: s.workload},
	)
	if err := s.workload.PrepareSessions(ctx, workers); err != nil {
		return err
	}
	if rt.Log != nil {
		rt.Log.Info(
			"scenario=%s workers=%d duration=%s rate=unlimited",
			s.Name(), workers, rt.Config.Run.Duration,
		)
	}
	return nil
}

func (s *TPScenario) Ramp(ctx context.Context, _ *Runtime) error {
	return s.run.Ramp(ctx)
}

func (s *TPScenario) Hold(ctx context.Context, rt *Runtime) error {
	return s.run.Hold(ctx, fixedWorkerStopTimeout(rt))
}

func (s *TPScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyFixedWorkerResult(
		s.Name(), s.run, map[string]string{"tp": "workers"},
	), nil
}

func (s *TPScenario) ExecutionSnapshot() WorkerSnapshot {
	return s.run.Snapshot()
}

func (s *TPScenario) RuntimeEvidence() []Evidence {
	return fixedWorkerEvidence(s.run, map[string]string{"tp": "workers"})
}

func (s *TPScenario) Stop(ctx context.Context, _ *Runtime) error {
	return s.run.Stop(ctx)
}

func (s *TPScenario) Restore(context.Context, *Runtime) error { return nil }

var orderSequence atomic.Int64

func buildTPWorkload(
	ctx context.Context,
	rt *Runtime,
	name string,
	workers int,
	start <-chan struct{},
) *sqlWorkload {
	orderSequence.CompareAndSwap(0, time.Now().UnixNano())
	return newSQLWorkloadWithStartGate(
		ctx,
		rt,
		name,
		workers,
		func(ctx context.Context, conn *sql.Conn, workerID int) error {
			// Every supported dataset has at least 10,000 customers. Keeping the
			// runtime account ID below that boundary makes the generator's
			// mod(id, customers)+1 distribution key exactly id+1.
			id := rand.Int63n(9_999) + 1
			statements := TPStatements(rt.Config.Data.Schema, id, 0, 0)
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var balance float64
			if err := tx.QueryRowContext(ctx, statements[0]).Scan(&balance); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, statements[1]); err != nil {
				return err
			}
			if workerID%10 == 0 {
				orderID := orderSequence.Add(1)
				insertSQL := TPStatements(rt.Config.Data.Schema, id, orderID, balance)[2]
				if _, err := tx.ExecContext(ctx, insertSQL); err != nil {
					return err
				}
			}
			return tx.Commit()
		},
		start,
	)
}
