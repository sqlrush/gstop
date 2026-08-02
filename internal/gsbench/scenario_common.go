package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

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
	runtime       *Runtime
	name          string
	group         *WorkerGroup
	op            SQLWorkerOp
	sessionOpener func(context.Context, int) (*TaggedConn, error)

	disableOperationTimeout bool
	cleanup                 SQLWorkerOp
	mu                      sync.Mutex
	sessions                map[int]*TaggedConn
	canceledWorkers         map[int]bool
	retireErrMu             sync.Mutex
	retireErr               error
}

func newSQLWorkload(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op SQLWorkerOp) *sqlWorkload {
	w := &sqlWorkload{
		runtime: runtime, name: name, op: op,
		sessions:        map[int]*TaggedConn{},
		canceledWorkers: map[int]bool{},
	}
	w.group = NewWorkerGroup(ctx, maxWorkers, w.run)
	w.group.SetRetireHook(w.retireSession)
	return w
}

func newSQLWorkloadWithStartGate(
	ctx context.Context,
	runtime *Runtime,
	name string,
	maxWorkers int,
	op SQLWorkerOp,
	start <-chan struct{},
) *sqlWorkload {
	w := &sqlWorkload{
		runtime: runtime, name: name, op: op,
		sessions:        map[int]*TaggedConn{},
		canceledWorkers: map[int]bool{},
	}
	w.group = NewWorkerGroupWithStartGate(ctx, maxWorkers, w.run, start)
	w.group.SetRetireHook(w.retireSession)
	return w
}

func newSQLWorkloadWithoutOperationTimeout(ctx context.Context, runtime *Runtime, name string, maxWorkers int, op SQLWorkerOp) *sqlWorkload {
	workload := newSQLWorkload(ctx, runtime, name, maxWorkers, op)
	workload.disableOperationTimeout = true
	return workload
}

func newSQLWorkloadWithoutOperationTimeoutWithStartGate(
	ctx context.Context,
	runtime *Runtime,
	name string,
	maxWorkers int,
	op SQLWorkerOp,
	start <-chan struct{},
) *sqlWorkload {
	workload := newSQLWorkloadWithStartGate(
		ctx, runtime, name, maxWorkers, op, start,
	)
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
func (w *sqlWorkload) WaitReady(ctx context.Context, n int) error {
	return w.group.WaitReady(ctx, n)
}

func (w *sqlWorkload) SetRunDeadline(deadline time.Time) error {
	return w.group.SetRunDeadline(deadline)
}

func (w *sqlWorkload) PrepareSessions(ctx context.Context, workers int) error {
	if workers <= 0 {
		return fmt.Errorf("fixed worker session count must be positive")
	}
	if w == nil || w.group == nil {
		return fmt.Errorf("SQL workload is unavailable")
	}
	if workers > w.group.max {
		return fmt.Errorf("worker target %d exceeds range 0..%d", workers, w.group.max)
	}
	prepareCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for workerID := range workers {
		workerID := workerID
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.session(prepareCtx, workerID); err != nil {
				errs <- fmt.Errorf("prepare worker %d session: %w", workerID, err)
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

func (w *sqlWorkload) run(ctx context.Context, workerID int) error {
	defer func() {
		if ctx.Err() == nil {
			return
		}
		w.mu.Lock()
		w.canceledWorkers[workerID] = true
		w.mu.Unlock()
	}()
	conn, err := w.session(ctx, workerID)
	if err != nil {
		return err
	}
	if w.disableOperationTimeout {
		err = w.op(ctx, conn.Conn, workerID)
	} else {
		timeout := w.runtime.Config.Safety.QueryTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		err = w.op(opCtx, conn.Conn, workerID)
		cancel()
	}
	return err
}

func (w *sqlWorkload) session(ctx context.Context, workerID int) (*TaggedConn, error) {
	w.mu.Lock()
	if conn := w.sessions[workerID]; conn != nil {
		w.mu.Unlock()
		return conn, nil
	}
	w.mu.Unlock()
	var conn *TaggedConn
	var err error
	if w.sessionOpener != nil {
		conn, err = w.sessionOpener(ctx, workerID)
	} else if w.runtime == nil || w.runtime.Database == nil {
		err = sql.ErrConnDone
	} else {
		conn, err = w.runtime.Database.OpenTagged(ctx, w.runtime.RunID, w.name, fmt.Sprint(workerID))
	}
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if existing := w.sessions[workerID]; existing != nil {
		w.mu.Unlock()
		if closeErr := conn.Close(); closeErr != nil {
			return nil, closeErr
		}
		return existing, nil
	}
	w.sessions[workerID] = conn
	w.mu.Unlock()
	return conn, nil
}

func (w *sqlWorkload) Stop(ctx context.Context) error {
	err := w.group.Stop(ctx)
	w.mu.Lock()
	sessions := w.sessions
	w.sessions = map[int]*TaggedConn{}
	canceledWorkers := w.canceledWorkers
	w.canceledWorkers = map[int]bool{}
	w.mu.Unlock()
	for id, conn := range sessions {
		if w.cleanup != nil {
			cleanupErr := w.cleanup(ctx, conn.Conn, id)
			err = errors.Join(err, normalizeCanceledWorkerConnectionError(
				cleanupErr,
				canceledWorkers[id],
			))
		}
		err = errors.Join(err, normalizeCanceledWorkerConnectionError(
			conn.Close(),
			canceledWorkers[id],
		))
	}
	w.retireErrMu.Lock()
	err = errors.Join(err, w.retireErr)
	w.retireErrMu.Unlock()
	return err
}

func (w *sqlWorkload) retireSession(id int) {
	w.mu.Lock()
	conn := w.sessions[id]
	delete(w.sessions, id)
	canceled := w.canceledWorkers[id]
	delete(w.canceledWorkers, id)
	w.mu.Unlock()
	if conn == nil {
		return
	}
	timeout := 30 * time.Second
	if w.runtime != nil && w.runtime.Config.Safety.QueryTimeout > 0 {
		timeout = w.runtime.Config.Safety.QueryTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var retireErr error
	if w.cleanup != nil {
		retireErr = errors.Join(retireErr, normalizeCanceledWorkerConnectionError(
			w.cleanup(ctx, conn.Conn, id),
			canceled,
		))
	}
	retireErr = errors.Join(retireErr, normalizeCanceledWorkerConnectionError(
		conn.Close(),
		canceled,
	))
	if retireErr == nil {
		return
	}
	w.retireErrMu.Lock()
	w.retireErr = errors.Join(w.retireErr, retireErr)
	w.retireErrMu.Unlock()
}

func normalizeCanceledWorkerConnectionError(err error, canceled bool) error {
	if canceled && isCanceledConnectionErrorTree(err) {
		return nil
	}
	return err
}

func isCanceledConnectionErrorTree(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isCanceledConnectionErrorTree(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isCanceledConnectionErrorTree(wrapped.Unwrap())
	}
	return err == driver.ErrBadConn || err == sql.ErrConnDone
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
