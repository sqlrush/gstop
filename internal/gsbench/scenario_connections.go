package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

func connectionTarget(instanceMax, targetPercent int) int {
	budget, err := calculateConnectionBudget(instanceMax, 0, 0, targetPercent)
	if err != nil {
		return 0
	}
	return budget.WorkloadTarget
}

type ConnectionBudget struct {
	InstanceMax     int
	Reserved        int
	Existing        int
	UsableCapacity  int
	DesiredTotal    int
	WorkloadTarget  int
	ReachableTotal  int
	BaselinePercent float64
	CeilingPercent  float64
	Limited         bool
}

func calculateConnectionBudget(instanceMax, reserved, existing, targetPercent int) (ConnectionBudget, error) {
	if instanceMax <= 0 {
		return ConnectionBudget{}, fmt.Errorf("max_connections must be positive")
	}
	if reserved < 0 || reserved >= instanceMax {
		return ConnectionBudget{}, fmt.Errorf("reserved connections %d exceed max_connections %d", reserved, instanceMax)
	}
	if existing < 0 {
		return ConnectionBudget{}, fmt.Errorf("existing connections must not be negative")
	}
	if targetPercent < 1 {
		return ConnectionBudget{}, fmt.Errorf("connection target percent must be positive")
	}
	usable := instanceMax - reserved
	baselinePercent := float64(existing) / float64(usable) * 100
	desired := int(math.Ceil(float64(usable) * float64(targetPercent) / 100))
	needed := max(0, desired-existing)
	headroom := max(0, usable-existing)
	workloadTarget := min(needed, headroom)
	reachable := min(usable, existing+workloadTarget)
	ceilingPercent := float64(reachable) / float64(usable) * 100
	return ConnectionBudget{
		InstanceMax: instanceMax, Reserved: reserved, Existing: existing,
		UsableCapacity: usable, DesiredTotal: desired,
		WorkloadTarget: workloadTarget, ReachableTotal: reachable,
		BaselinePercent: baselinePercent,
		CeilingPercent:  ceilingPercent, Limited: reachable < desired,
	}, nil
}

type connectionCapacityFacts struct {
	InstanceMax int
	Reserved    int
	Existing    int
}

func probeConnectionCapacity(ctx context.Context, rt *Runtime) (connectionCapacityFacts, error) {
	if rt == nil || rt.Database == nil {
		return connectionCapacityFacts{}, sql.ErrConnDone
	}
	probeInt := func(name, query string) (int, error) {
		value, err := rt.Database.Probe(ctx, name, query)
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
		}
		return parsed, nil
	}
	instanceMax, err := probeInt("max_connections", "SHOW max_connections")
	if err != nil {
		return connectionCapacityFacts{}, err
	}
	reserved, err := probeInt("sysadmin_reserved_connections", "SHOW sysadmin_reserved_connections")
	if err != nil {
		return connectionCapacityFacts{}, err
	}
	existing, err := probeInt("current_connections", "SELECT count(*) FROM pg_stat_activity")
	if err != nil {
		return connectionCapacityFacts{}, err
	}
	return connectionCapacityFacts{InstanceMax: instanceMax, Reserved: reserved, Existing: existing}, nil
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
	connections    []*TaggedConn
	transactions   []*sql.Tx
	activeCancel   context.CancelFunc
	activeWG       sync.WaitGroup
	activeErrors   chan error
	budget         ConnectionBudget
	targetPercent  int
	idlePercent    int
	idleTxnPercent int
	actualPercent  float64
	reachableMax   float64
	liveTagged     int
	targetReached  bool
	nextID         int
	operations     atomic.Int64
	errors         atomic.Int64
	errorMu        sync.Mutex
	firstError     string
}

func NewConnectionScenario() *ConnectionScenario { return &ConnectionScenario{} }
func (s *ConnectionScenario) Code() ScenarioCode {
	return 401
}
func (s *ConnectionScenario) Name() string { return "connection_pool" }
func (s *ConnectionScenario) Strategy() string {
	return "capacity_aware_tagged_connection_state_mix"
}
func (s *ConnectionScenario) Prepare(ctx context.Context, rt *Runtime) error {
	facts, err := probeConnectionCapacity(ctx, rt)
	if err != nil {
		return err
	}
	s.targetPercent = rt.Config.PoolTargets.ConnectionPercent
	s.budget, err = calculateConnectionBudget(
		facts.InstanceMax, facts.Reserved, facts.Existing,
		s.targetPercent,
	)
	if err != nil {
		return err
	}
	if float64(s.targetPercent) <= s.budget.BaselinePercent {
		runtimeWarn(rt, PrecheckWarning{
			ScenarioCode: s.Code(), Scenario: s.Name(),
			Check: "capacity", Object: "connection_pool_target",
			Actual:   fmt.Sprintf("baseline=%.1f%% target=%d%%", s.budget.BaselinePercent, s.targetPercent),
			Expected: "target_above_baseline",
			Impact:   "no_additional_sessions_requested",
		})
	}
	if s.budget.Limited {
		runtimeWarn(rt, PrecheckWarning{
			ScenarioCode: s.Code(), Scenario: s.Name(),
			Check: "capacity", Object: "connection_pool_target",
			Actual:   fmt.Sprintf("reachable=%.1f%% target=%d%%", s.budget.CeilingPercent, s.targetPercent),
			Expected: fmt.Sprintf("%d%%", s.targetPercent),
			Impact:   "target_may_not_be_reached",
		})
	}
	s.idlePercent = runtimeInt(rt, "scenario.connection_pool.idle_percent", 60)
	s.idleTxnPercent = runtimeInt(rt, "scenario.connection_pool.idle_in_transaction_percent", 20)
	s.activeErrors = make(chan error, 1)
	return nil
}
func (s *ConnectionScenario) Ramp(ctx context.Context, rt *Runtime) error {
	activeCtx, cancel := context.WithCancel(ctx)
	s.activeCancel = cancel
	for range s.budget.WorkloadTarget {
		if err := s.openConnection(activeCtx, rt); err != nil {
			return connectionTargetRampError(
				err,
				len(s.connections),
				s.budget.WorkloadTarget,
			)
		}
	}
	total, tagged, err := s.sampleConnections(activeCtx, rt)
	if err != nil {
		return connectionTargetRampError(
			err,
			len(s.connections),
			s.budget.WorkloadTarget,
		)
	}
	return s.acceptRampSample(total, tagged)
}

func (s *ConnectionScenario) sampleConnections(ctx context.Context, rt *Runtime) (total, tagged int, err error) {
	value, err := rt.Database.Probe(ctx, "current_connections", "SELECT count(*) FROM pg_stat_activity")
	if err != nil {
		return 0, 0, err
	}
	total, err = strconv.Atoi(value)
	if err != nil {
		return 0, 0, fmt.Errorf("parse current_connections %q: %w", value, err)
	}
	pattern, err := taggedScenarioPattern(rt.RunID, s.Name())
	if err != nil {
		return 0, 0, err
	}
	err = rt.Database.Scan(ctx,
		"SELECT count(*) FROM pg_stat_activity WHERE application_name LIKE $1 ESCAPE E'\\\\'",
		[]any{pattern}, &tagged,
	)
	return total, tagged, err
}

func (s *ConnectionScenario) updateConnectionSample(total, tagged int) {
	s.liveTagged = tagged
	s.actualPercent = float64(total) / float64(s.budget.UsableCapacity) * 100
	if s.actualPercent > s.reachableMax {
		s.reachableMax = s.actualPercent
	}
}

func (s *ConnectionScenario) acceptFrozenSample(total, tagged int) error {
	s.updateConnectionSample(total, tagged)
	if tagged != s.budget.WorkloadTarget {
		return fmt.Errorf(
			"connection_pool injected sessions changed: actual=%d target=%d",
			tagged,
			s.budget.WorkloadTarget,
		)
	}
	return nil
}

func (s *ConnectionScenario) acceptRampSample(total, tagged int) error {
	if err := s.acceptFrozenSample(total, tagged); err != nil {
		return err
	}
	s.targetReached = total >= s.budget.DesiredTotal
	return nil
}

func connectionTargetRampError(err error, opened, target int) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"connection_pool target was not reached before --duration: opened=%d target=%d",
			opened,
			target,
		)
	}
	return err
}

func (s *ConnectionScenario) openConnection(ctx context.Context, rt *Runtime) error {
	id := s.nextID
	s.nextID++
	conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), strconv.Itoa(id))
	if err != nil {
		s.recordExecutionError(err)
		return fmt.Errorf("open target connection %d: %w", id, err)
	}
	s.connections = append(s.connections, conn)
	s.operations.Add(1)
	idleN, idleTxnN, _ := connectionStateCounts(s.budget.WorkloadTarget, s.idlePercent, s.idleTxnPercent)
	roleIndex := id
	if s.budget.WorkloadTarget > 0 {
		roleIndex %= s.budget.WorkloadTarget
	}
	switch {
	case roleIndex < idleN:
		// An unused established connection is intentionally idle.
	case roleIndex < idleN+idleTxnN:
		tx, err := conn.Conn.BeginTx(ctx, nil)
		if err != nil {
			s.recordExecutionError(err)
			return err
		}
		if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
			_ = tx.Rollback()
			s.recordExecutionError(err)
			return err
		}
		s.transactions = append(s.transactions, tx)
	default:
		s.activeWG.Add(1)
		go func(c *sql.Conn) {
			defer s.activeWG.Done()
			for ctx.Err() == nil {
				if _, err := c.ExecContext(ctx, "SELECT pg_sleep(1)"); err != nil {
					if ctx.Err() == nil {
						s.recordExecutionError(err)
					}
					return
				}
				s.operations.Add(1)
			}
		}(conn.Conn)
	}
	return nil
}
func (s *ConnectionScenario) Hold(ctx context.Context, rt *Runtime) error {
	interval := rt.Config.Run.RampInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(rt.Config.Run.Duration)
	defer timer.Stop()
	for {
		select {
		case err := <-s.activeErrors:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			total, tagged, err := s.sampleConnections(ctx, rt)
			if err != nil {
				return err
			}
			if err := s.acceptFrozenSample(total, tagged); err != nil {
				return err
			}
		}
	}
}
func (s *ConnectionScenario) Verify(context.Context, *Runtime) (Result, error) {
	control := ControlResult{
		Actual: s.actualPercent, ReachableMax: s.reachableMax,
		Reached: s.targetReached,
		Ceiling: s.budget.Limited,
	}
	result := verifyControlledCapacityResult(s.Name(), float64(s.targetPercent), true, control, int64(s.liveTagged))
	result.Evidence = append(result.Evidence,
		Evidence{Metric: "usable_connection_capacity", Actual: float64(s.budget.UsableCapacity), Available: true},
		Evidence{Metric: "reserved_connections", Actual: float64(s.budget.Reserved), Available: true},
		Evidence{Metric: "existing_connections", Actual: float64(s.budget.Existing), Available: true},
		Evidence{Metric: "workload_connection_target", Target: float64(s.budget.WorkloadTarget), Actual: float64(s.liveTagged), Available: true},
		Evidence{Metric: "connection_capacity_ceiling_percent", Actual: s.budget.CeilingPercent, Available: true},
	)
	return result, nil
}
func (s *ConnectionScenario) ExecutionSnapshot() WorkerSnapshot {
	s.errorMu.Lock()
	firstError := s.firstError
	s.errorMu.Unlock()
	return WorkerSnapshot{
		Target: s.budget.WorkloadTarget, Active: s.liveTagged,
		Operations: s.operations.Load(), Errors: s.errors.Load(),
		FirstError: firstError,
	}
}
func (s *ConnectionScenario) recordExecutionError(err error) {
	if err == nil {
		return
	}
	s.errors.Add(1)
	s.errorMu.Lock()
	defer s.errorMu.Unlock()
	if s.firstError == "" {
		s.firstError = journalSafeErrorText(err.Error())
	}
	if s.activeErrors != nil {
		select {
		case s.activeErrors <- err:
		default:
		}
	}
}
func (s *ConnectionScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.activeCancel != nil {
		s.activeCancel()
		s.activeCancel = nil
	}
	done := make(chan struct{})
	go func() {
		s.activeWG.Wait()
		close(done)
	}()
	var errs []error
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	for _, tx := range s.transactions {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			errs = append(errs, err)
		}
	}
	for _, conn := range s.connections {
		if err := conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	s.transactions = nil
	s.connections = nil
	return errors.Join(errs...)
}
func (s *ConnectionScenario) Restore(context.Context, *Runtime) error { return nil }
