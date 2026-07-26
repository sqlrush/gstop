package gsbench

import (
	"context"
	"database/sql"
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
	rows, err := s.db.Query(ctx, `SELECT name,value FROM dbe_perf.os_runtime WHERE name IN ('BUSY_TIME','IDLE_TIME')`)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	var busy, idle float64
	var found int
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			return 0, false
		}
		switch name {
		case "BUSY_TIME":
			busy = value
			found++
		case "IDLE_TIME":
			idle = value
			found++
		}
	}
	if found != 2 {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.have {
		s.busy, s.idle, s.have = busy, idle, true
		return 0, false
	}
	deltaBusy, deltaIdle := busy-s.busy, idle-s.idle
	s.busy, s.idle = busy, idle
	if deltaBusy < 0 || deltaIdle < 0 || deltaBusy+deltaIdle <= 0 {
		return 0, false
	}
	return deltaBusy / (deltaBusy + deltaIdle) * 100, true
}

type SQLWorkerOp func(context.Context, *sql.Conn, int) error

type sqlWorkload struct {
	runtime *Runtime
	name    string
	group   *WorkerGroup
	op      SQLWorkerOp

	mu       sync.Mutex
	sessions map[int]*TaggedConn
}

func newSQLWorkload(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op SQLWorkerOp) *sqlWorkload {
	w := &sqlWorkload{runtime: runtime, name: name, op: op, sessions: map[int]*TaggedConn{}}
	w.group = NewWorkerGroup(ctx, maxWorkers, w.run)
	return w
}

func (w *sqlWorkload) Target() int              { return w.group.Target() }
func (w *sqlWorkload) SetTarget(n int) error    { return w.group.SetTarget(n) }
func (w *sqlWorkload) Snapshot() WorkerSnapshot { return w.group.Snapshot() }

func (w *sqlWorkload) run(ctx context.Context, workerID int) error {
	conn, err := w.session(ctx, workerID)
	if err != nil {
		return err
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
	result.Evidence = []Evidence{{Metric: "db_host_cpu_percent", Target: target, Actual: control.Actual, Available: available}, {
		Metric: "operations", Actual: float64(snapshot.Operations), Available: true,
	}}
	switch {
	case available && control.Reached:
		result.Outcome = OutcomeSuccess
		result.Message = fmt.Sprintf("database host CPU sustained %.1f%% with %d workers", control.Actual, control.Workers)
	case !available && control.Ceiling && snapshot.Operations > 0:
		result.Outcome = OutcomeDegraded
		result.Message = "CPU metric unavailable; workload reached the configured worker ceiling"
	default:
		result.Message = fmt.Sprintf("CPU target %.1f%% was not reached", target)
	}
	return result
}

func verifyCapacityResult(name string, target, actual float64, real bool, operations int64) Result {
	result := Result{Scenario: name, Outcome: OutcomeFailed, Evidence: []Evidence{{
		Metric: name + "_percent", Target: target, Actual: actual, Available: real,
	}}}
	switch {
	case real && actual >= target:
		result.Outcome = OutcomeSuccess
		result.Message = fmt.Sprintf("%s reached %.1f%%", name, actual)
	case !real && operations > 0:
		result.Outcome = OutcomeDegraded
		result.Message = fmt.Sprintf("%s real metric unavailable; fallback workload is active", name)
	default:
		result.Message = fmt.Sprintf("%s target %.1f%% was not reached; actual %.1f%%", name, target, actual)
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
