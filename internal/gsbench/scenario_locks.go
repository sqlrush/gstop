package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"
)

type LockRole struct {
	Kind      string
	Row       int
	ParentRow int
}
type LockTopology struct {
	Blockers []LockRole
	Waiters  []LockRole
	Deadlock bool
}

func BuildLockTopology(depth, waitersPerBlocker int, deadlock bool) (LockTopology, error) {
	if depth < 1 {
		return LockTopology{}, fmt.Errorf("chain depth must be positive")
	}
	if waitersPerBlocker < 1 {
		return LockTopology{}, fmt.Errorf("waiters per blocker must be positive")
	}
	plan := LockTopology{Deadlock: deadlock}
	for i := 1; i <= depth; i++ {
		plan.Blockers = append(plan.Blockers, LockRole{Kind: "row", Row: i, ParentRow: i - 1})
	}
	plan.Blockers = append(plan.Blockers, LockRole{Kind: "table"}, LockRole{Kind: "ddl", Row: 1})
	for i := 1; i <= depth; i++ {
		for range waitersPerBlocker {
			plan.Waiters = append(plan.Waiters, LockRole{Kind: "row", ParentRow: i})
		}
	}
	for range waitersPerBlocker {
		plan.Waiters = append(plan.Waiters, LockRole{Kind: "table"})
	}
	for range waitersPerBlocker {
		plan.Waiters = append(plan.Waiters, LockRole{Kind: "ddl"})
	}
	return plan, nil
}

type LockScenario struct {
	plan         LockTopology
	ctx          context.Context
	cancel       context.CancelFunc
	connections  []*TaggedConn
	transactions []*sql.Tx
	deadlockTx   []*sql.Tx
	wg           sync.WaitGroup
	actual       int
}

func NewLockScenario() *LockScenario { return &LockScenario{} }
func (s *LockScenario) Name() string { return "lock_storm" }
func (s *LockScenario) Prepare(ctx context.Context, rt *Runtime) error {
	plan, err := BuildLockTopology(runtimeInt(rt, "scenario.lock_storm.chain_depth", 5), runtimeInt(rt, "scenario.lock_storm.waiters_per_blocker", 10), runtimeBool(rt, "scenario.lock_storm.deadlock", false))
	if err != nil {
		return err
	}
	s.plan = plan
	s.ctx, s.cancel = context.WithCancel(ctx)
	for i, role := range plan.Blockers {
		conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), "blocker-"+strconv.Itoa(i))
		if err != nil {
			return err
		}
		tx, err := conn.Conn.BeginTx(ctx, nil)
		if err != nil {
			conn.Close()
			return err
		}
		var lockSQL string
		var args []any
		switch role.Kind {
		case "row":
			lockSQL = "UPDATE " + rt.Config.Data.Schema + ".lock_targets SET value=value+1 WHERE id=$1"
			args = []any{role.Row}
		case "table":
			lockSQL = "LOCK TABLE " + rt.Config.Data.Schema + ".lock_table_targets IN ACCESS EXCLUSIVE MODE"
		case "ddl":
			lockSQL = "UPDATE " + rt.Config.Data.Schema + ".lock_ddl_targets SET value=value+1 WHERE id=1"
		}
		if _, err = tx.ExecContext(ctx, lockSQL, args...); err != nil {
			tx.Rollback()
			conn.Close()
			return err
		}
		s.connections = append(s.connections, conn)
		s.transactions = append(s.transactions, tx)
	}
	if plan.Deadlock {
		for i, row := range []int{100, 101} {
			conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), "deadlock-"+strconv.Itoa(i))
			if err != nil {
				return err
			}
			tx, err := conn.Conn.BeginTx(ctx, nil)
			if err != nil {
				conn.Close()
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE "+rt.Config.Data.Schema+".lock_targets SET value=value+1 WHERE id=$1", row); err != nil {
				tx.Rollback()
				conn.Close()
				return err
			}
			s.connections = append(s.connections, conn)
			s.transactions = append(s.transactions, tx)
			s.deadlockTx = append(s.deadlockTx, tx)
		}
	}
	return nil
}
func (s *LockScenario) Ramp(ctx context.Context, rt *Runtime) error {
	query := "UPDATE " + rt.Config.Data.Schema + ".lock_targets SET value=value+1 WHERE id=$1"
	rowBlockers := len(s.plan.Blockers) - 2
	for i := 1; i < rowBlockers; i++ {
		tx := s.transactions[i]
		row := s.plan.Blockers[i].ParentRow
		s.wg.Add(1)
		go func() { defer s.wg.Done(); _, _ = tx.ExecContext(s.ctx, query, row) }()
	}
	for i, role := range s.plan.Waiters {
		conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), "waiter-"+strconv.Itoa(i))
		if err != nil {
			return err
		}
		s.connections = append(s.connections, conn)
		s.wg.Add(1)
		switch role.Kind {
		case "row":
			tx, err := conn.Conn.BeginTx(ctx, nil)
			if err != nil {
				conn.Close()
				return err
			}
			s.transactions = append(s.transactions, tx)
			go func() { defer s.wg.Done(); _, _ = tx.ExecContext(s.ctx, query, role.ParentRow) }()
		case "table":
			go func() {
				defer s.wg.Done()
				rows, err := conn.Conn.QueryContext(s.ctx, "SELECT count(*) FROM "+rt.Config.Data.Schema+".lock_table_targets")
				if err == nil {
					_ = consumeRows(rows)
					_ = rows.Close()
				}
			}()
		case "ddl":
			go func() {
				defer s.wg.Done()
				_, _ = conn.Conn.ExecContext(s.ctx, "ALTER TABLE "+rt.Config.Data.Schema+".lock_ddl_targets SET (fillfactor=90)")
			}()
		}
	}
	if s.plan.Deadlock {
		first := s.deadlockTx[0]
		second := s.deadlockTx[1]
		s.wg.Add(2)
		go func() { defer s.wg.Done(); _, _ = first.ExecContext(s.ctx, query, 101) }()
		go func() { defer s.wg.Done(); _, _ = second.ExecContext(s.ctx, query, 100) }()
	}
	return waitContext(ctx, rt.Config.Run.RampInterval)
}
func (s *LockScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *LockScenario) Verify(ctx context.Context, rt *Runtime) (Result, error) {
	predicate, arg, err := TaggedSessionPredicate(rt.RunID)
	if err != nil {
		return Result{}, err
	}
	query := "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE NOT l.granted AND " + predicate
	if err := rt.Database.Scan(ctx, query, []any{arg}, &s.actual); err != nil {
		return Result{}, err
	}
	target := len(s.plan.Waiters)
	out := OutcomeFailed
	if s.actual >= target {
		out = OutcomeSuccess
	}
	return Result{Scenario: s.Name(), Outcome: out, Message: fmt.Sprintf("lock waiters %d/%d", s.actual, target), Evidence: []Evidence{{Metric: "lock_waiters", Target: float64(target), Actual: float64(s.actual), Available: true}}}, nil
}
func (s *LockScenario) Stop(context.Context, *Runtime) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	var first error
	for _, tx := range s.transactions {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone && first == nil {
			first = err
		}
	}
	for _, conn := range s.connections {
		if err := conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
func (s *LockScenario) Restore(ctx context.Context, rt *Runtime) error {
	_, err := rt.Database.Exec(ctx, "ALTER TABLE "+rt.Config.Data.Schema+".lock_ddl_targets RESET (fillfactor)")
	return err
}
