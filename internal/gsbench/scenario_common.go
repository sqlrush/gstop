package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

type CPUSampler interface {
	SampleCPU(context.Context) (float64, bool)
}

type DatabaseCPUSampler struct {
	db   *Database
	mu   sync.Mutex
	busy float64
	idle float64
	have bool
}

func NewDatabaseCPUSampler(db *Database) *DatabaseCPUSampler {
	return &DatabaseCPUSampler{db: db}
}

func (s *DatabaseCPUSampler) SampleCPU(ctx context.Context) (float64, bool) {
	value, available, _ := s.SampleCPUResult(ctx)
	return value, available
}

// SampleCPUResult preserves real database/scan failures for continuous
// controllers while SampleCPU keeps the legacy two-result interface usable by
// callers that only distinguish available from unavailable metrics.
func (s *DatabaseCPUSampler) SampleCPUResult(ctx context.Context) (float64, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT name,value FROM dbe_perf.os_runtime WHERE name IN ('BUSY_TIME','IDLE_TIME')`)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var busy, idle float64
	var haveBusy, haveIdle bool
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			return 0, false, err
		}
		switch name {
		case "BUSY_TIME":
			busy = value
			haveBusy = true
		case "IDLE_TIME":
			idle = value
			haveIdle = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if !haveBusy || !haveIdle {
		return 0, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		s.busy, s.idle, s.have = busy, idle, true
		return 0, false, nil
	}
	deltaBusy, deltaIdle := busy-s.busy, idle-s.idle
	s.busy, s.idle = busy, idle
	if deltaBusy < 0 || deltaIdle < 0 || deltaBusy+deltaIdle <= 0 {
		return 0, false, nil
	}
	return deltaBusy / (deltaBusy + deltaIdle) * 100, true, nil
}

type cpuResultSampler interface {
	SampleCPUResult(context.Context) (float64, bool, error)
}

func sampleCPU(ctx context.Context, sampler CPUSampler, snapshot WorkerSnapshot) Sample {
	sample := Sample{Errors: snapshot.Errors}
	if snapshot.Errors > 0 {
		sample.Err = workerSnapshotError(snapshot)
		return sample
	}
	if sampler == nil {
		return sample
	}
	if detailed, ok := sampler.(cpuResultSampler); ok {
		value, available, err := detailed.SampleCPUResult(ctx)
		sample.Value, sample.Available, sample.Err = value, available, err
		return sample
	}
	sample.Value, sample.Available = sampler.SampleCPU(ctx)
	return sample
}

func workerSnapshotError(snapshot WorkerSnapshot) error {
	if snapshot.Errors <= 0 {
		return nil
	}
	if snapshot.FirstError == "" {
		return fmt.Errorf("workload execution errors=%d", snapshot.Errors)
	}
	return fmt.Errorf(
		"workload execution errors=%d first_error=%s",
		snapshot.Errors,
		snapshot.FirstError,
	)
}

// continuousControl owns a single Controller.RunUntil goroutine. Wait and Stop
// always join it, so scenario workers can only be stopped after sampling and
// actuation have ceased.
type continuousControl struct {
	cancel context.CancelFunc
	done   chan struct{}
	final  ControlResult
}

func (c *continuousControl) Start(parent context.Context, controller Controller) {
	controlCtx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.done = make(chan struct{})
	go func() {
		c.final = controller.RunUntil(controlCtx)
		close(c.done)
	}()
}

func (c *continuousControl) Wait(ctx context.Context, duration time.Duration) (ControlResult, error) {
	if c.done == nil {
		return c.final, fmt.Errorf("continuous controller is not running")
	}
	var timer *time.Timer
	var durationDone <-chan time.Time
	if duration > 0 {
		timer = time.NewTimer(duration)
		durationDone = timer.C
		defer timer.Stop()
	}
	select {
	case <-c.done:
		if err := nonContextControlError(c.final.Err); err != nil {
			return c.final, err
		}
		if err := ctx.Err(); err != nil {
			return c.final, err
		}
		return c.final, c.final.Err
	case <-ctx.Done():
		c.cancel()
		<-c.done
		if err := nonContextControlError(c.final.Err); err != nil {
			return c.final, err
		}
		return c.final, ctx.Err()
	case <-durationDone:
		c.cancel()
		<-c.done
		if err := nonContextControlError(c.final.Err); err != nil {
			return c.final, err
		}
		if err := ctx.Err(); err != nil {
			return c.final, err
		}
		return c.final, nil
	}
}

func (c *continuousControl) Stop() ControlResult {
	if c.cancel != nil {
		c.cancel()
	}
	if c.done == nil {
		return c.final
	}
	<-c.done
	return c.final
}

func nonContextControlError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

type SQLWorkerOp func(context.Context, *sql.Conn, int) error

type sqlWorkload struct {
	runtime *Runtime
	name    string
	group   *WorkerGroup
	op      SQLWorkerOp

	disableOperationTimeout bool
	cleanup                 SQLWorkerOp
	mu                      sync.Mutex
	sessions                map[int]*TaggedConn
}

func newSQLWorkload(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op SQLWorkerOp) *sqlWorkload {
	w := &sqlWorkload{runtime: runtime, name: name, op: op, sessions: map[int]*TaggedConn{}}
	w.group = NewWorkerGroup(ctx, maxWorkers, w.run)
	return w
}

func newSQLWorkloadWithoutOperationTimeout(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op SQLWorkerOp) *sqlWorkload {
	workload := newSQLWorkload(ctx, runtime, name, maxWorkers, op)
	workload.disableOperationTimeout = true
	return workload
}

func newSQLWorkloadWithCleanup(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op, cleanup SQLWorkerOp) *sqlWorkload {
	workload := newSQLWorkload(ctx, runtime, name, maxWorkers, op)
	workload.cleanup = cleanup
	return workload
}

func (w *sqlWorkload) Target() int              { return w.group.Target() }
func (w *sqlWorkload) SetTarget(n int) error    { return w.group.SetTarget(n) }
func (w *sqlWorkload) Snapshot() WorkerSnapshot { return w.group.Snapshot() }

func (w *sqlWorkload) run(ctx context.Context, workerID int) error {
	conn, err := w.session(ctx, workerID)
	if err != nil {
		return err
	}
	if w.disableOperationTimeout {
		return w.op(ctx, conn.Conn, workerID)
	}
	timeout := w.runtime.Config.Safety.QueryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return w.op(opCtx, conn.Conn, workerID)
}

func (w *sqlWorkload) session(ctx context.Context, workerID int) (*TaggedConn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if conn := w.sessions[workerID]; conn != nil {
		return conn, nil
	}
	conn, err := w.runtime.Database.OpenTagged(ctx, w.runtime.RunID, w.name, fmt.Sprint(workerID))
	if err != nil {
		return nil, err
	}
	w.sessions[workerID] = conn
	return conn, nil
}

func (w *sqlWorkload) Stop(ctx context.Context) error {
	err := w.group.Stop(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, conn := range w.sessions {
		if w.cleanup != nil {
			if cleanupErr := w.cleanup(ctx, conn.Conn, id); err == nil {
				err = cleanupErr
			}
		}
		if closeErr := conn.Close(); err == nil {
			err = closeErr
		}
		delete(w.sessions, id)
	}
	return err
}

func consumeRows(rows *sql.Rows) error {
	columns, err := rows.Columns()
	if err != nil {
		return err
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
	}
	return rows.Err()
}

func verifyCPUResult(name string, target float64, available bool, control ControlResult, snapshot WorkerSnapshot) Result {
	result := Result{Scenario: name, Outcome: OutcomeFailed}
	reachable := control.ReachableMax
	if control.Actual > reachable {
		reachable = control.Actual
	}
	result.Evidence = []Evidence{{Metric: "db_host_cpu_percent", Target: target, Actual: control.Actual, Available: available}, {
		Metric: "reachable_max_percent", Target: target, Actual: reachable, Available: available,
	}, {
		Metric: "operations", Actual: float64(snapshot.Operations), Available: true,
	}}
	switch {
	case available && control.Reached:
		result.Outcome = OutcomeSuccess
		result.Message = fmt.Sprintf("database host CPU sustained %.1f%% with %d workers", control.Actual, control.Workers)
	case !available && control.Ceiling && snapshot.Operations > 0:
		result.Outcome = OutcomeDegraded
		result.Message = "CPU metric unavailable; workload reached the configured worker ceiling"
	case available && control.Ceiling:
		result.Message = fmt.Sprintf("CPU target %.1f%% is unreachable; measured ceiling %.1f%%", target, reachable)
	default:
		result.Message = fmt.Sprintf("CPU target %.1f%% was not reached", target)
	}
	return result
}

func verifyCapacityResult(name string, target, actual float64, real bool, operations int64) Result {
	return verifyControlledCapacityResult(name, target, real, ControlResult{
		Reached: actual >= target, Actual: actual, ReachableMax: actual,
	}, operations)
}

func verifyControlledCapacityResult(name string, target float64, real bool, control ControlResult, operations int64) Result {
	reachable := control.ReachableMax
	if control.Actual > reachable {
		reachable = control.Actual
	}
	result := Result{Scenario: name, Outcome: OutcomeFailed, Evidence: []Evidence{{
		Metric: name + "_percent", Target: target, Actual: control.Actual, Available: real,
	}, {
		Metric: "reachable_max_percent", Target: target, Actual: reachable, Available: real,
	}}}
	switch {
	case real && control.Reached:
		result.Outcome = OutcomeSuccess
		result.Message = fmt.Sprintf("%s reached %.1f%%", name, control.Actual)
	case !real && operations > 0:
		result.Outcome = OutcomeDegraded
		result.Message = fmt.Sprintf("%s real metric unavailable; fallback workload is active", name)
	case real && control.Ceiling:
		result.Message = fmt.Sprintf("%s target %.1f%% is unreachable; measured capacity ceiling %.1f%%", name, target, reachable)
	default:
		result.Message = fmt.Sprintf("%s target %.1f%% was not reached; actual %.1f%%", name, target, control.Actual)
	}
	return result
}

func runtimeInt(rt *Runtime, key string, def int) int {
	if rt == nil || rt.Config.Raw == nil {
		return def
	}
	return rt.Config.Raw.GetInt(key, def)
}

func runtimeString(rt *Runtime, key, def string) string {
	if rt == nil || rt.Config.Raw == nil {
		return def
	}
	return rt.Config.Raw.GetString(key, def)
}

func runtimeFloat(rt *Runtime, key string, def float64) float64 {
	if rt == nil || rt.Config.Raw == nil {
		return def
	}
	return rt.Config.Raw.GetFloat(key, def)
}

func runtimeBool(rt *Runtime, key string, def bool) bool {
	if rt == nil || rt.Config.Raw == nil {
		return def
	}
	return rt.Config.Raw.GetBool(key, def)
}
