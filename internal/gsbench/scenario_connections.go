package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"
)

func connectionTarget(instanceMax, targetPercent, safetyMax int) int {
	target := instanceMax * targetPercent / 100
	if target > safetyMax {
		target = safetyMax
	}
	if target < 1 {
		target = 1
	}
	return target
}

func connectionStateCounts(total, idlePercent, idleTxnPercent int) (idle, idleTxn, active int) {
	idle = total * idlePercent / 100
	idleTxn = total * idleTxnPercent / 100
	if idle+idleTxn > total {
		idleTxn = total - idle
	}
	active = total - idle - idleTxn
	return
}

type ConnectionScenario struct {
	connections   []*TaggedConn
	transactions  []*sql.Tx
	activeCancel  context.CancelFunc
	activeWG      sync.WaitGroup
	instanceMax   int
	baseline      int
	targetPercent int
	actualPercent float64
}

func NewConnectionScenario() *ConnectionScenario { return &ConnectionScenario{} }
func (s *ConnectionScenario) Name() string       { return "connection_pool" }
func (s *ConnectionScenario) Prepare(ctx context.Context, rt *Runtime) error {
	maxValue, err := rt.Database.Probe(ctx, "max_connections", "SHOW max_connections")
	if err != nil {
		return err
	}
	s.instanceMax, err = strconv.Atoi(maxValue)
	if err != nil {
		return fmt.Errorf("parse max_connections %q: %w", maxValue, err)
	}
	baseline, err := rt.Database.Probe(ctx, "current_connections", "SELECT count(*) FROM pg_stat_activity")
	if err != nil {
		return err
	}
	s.baseline, err = strconv.Atoi(baseline)
	if err != nil {
		return fmt.Errorf("parse connection count %q: %w", baseline, err)
	}
	s.targetPercent = runtimeInt(rt, "scenario.connection_pool.target_percent", 95)
	return nil
}
func (s *ConnectionScenario) Ramp(ctx context.Context, rt *Runtime) error {
	absoluteTarget := connectionTarget(s.instanceMax, s.targetPercent, rt.Config.Safety.MaxConnections)
	toOpen := max(0, absoluteTarget-s.baseline)
	idleN, idleTxnN, _ := connectionStateCounts(toOpen,
		runtimeInt(rt, "scenario.connection_pool.idle_percent", 60),
		runtimeInt(rt, "scenario.connection_pool.idle_in_transaction_percent", 20))
	activeCtx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	for i := 0; i < toOpen; i++ {
		conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), strconv.Itoa(i))
		if err != nil {
			break
		}
		s.connections = append(s.connections, conn)
		switch {
		case i < idleN:
			// An unused established connection is intentionally idle.
		case i < idleN+idleTxnN:
			tx, err := conn.Conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
				_ = tx.Rollback()
				return err
			}
			s.transactions = append(s.transactions, tx)
		default:
			s.activeWG.Add(1)
			go func(c *sql.Conn) {
				defer s.activeWG.Done()
				for activeCtx.Err() == nil {
					_, _ = c.ExecContext(activeCtx, "SELECT pg_sleep(1)")
				}
			}(conn.Conn)
		}
	}
	s.actualPercent = float64(s.baseline+len(s.connections)) / float64(s.instanceMax) * 100
	return nil
}
func (s *ConnectionScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *ConnectionScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCapacityResult(s.Name(), float64(s.targetPercent), s.actualPercent, true, int64(len(s.connections))), nil
}
func (s *ConnectionScenario) Stop(context.Context, *Runtime) error {
	if s.activeCancel != nil {
		s.activeCancel()
	}
	s.activeWG.Wait()
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
	s.transactions = nil
	s.connections = nil
	return first
}
func (s *ConnectionScenario) Restore(context.Context, *Runtime) error { return nil }
