package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestContinuousControlStopCancelsAndJoinsController(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	a := &fakeActuator{}
	loop := &continuousControl{}
	loop.Start(context.Background(), Controller{
		Config: ControllerConfig{
			Target: 50, MinWorkers: 1, MaxWorkers: 2,
			RequiredSamples: 1, Interval: time.Nanosecond,
		},
		Actuator: a,
		Sample: func(ctx context.Context) Sample {
			close(entered)
			<-ctx.Done()
			close(exited)
			return Sample{}
		},
	})
	<-entered
	result := loop.Stop()
	select {
	case <-exited:
	default:
		t.Fatal("Stop returned before the controller sampler exited")
	}
	if result.Err != nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestContinuousControlWaitDoesNotHideCallerCancellation(t *testing.T) {
	for range 50 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		loop := &continuousControl{}
		loop.Start(ctx, Controller{
			Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 1, Interval: time.Nanosecond},
			Actuator: &fakeActuator{},
			Sample:   func(context.Context) Sample { return Sample{} },
		})
		time.Sleep(time.Millisecond)
		_, err := loop.Wait(ctx, time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation was hidden: %v", err)
		}
	}
}

func TestContinuousControlWaitAndStopCanJoinConcurrently(t *testing.T) {
	entered := make(chan struct{})
	loop := &continuousControl{}
	loop.Start(context.Background(), Controller{
		Config:   ControllerConfig{Target: 50, MinWorkers: 1, MaxWorkers: 1, Interval: time.Nanosecond},
		Actuator: &fakeActuator{},
		Sample: func(ctx context.Context) Sample {
			close(entered)
			<-ctx.Done()
			return Sample{}
		},
	})
	<-entered
	waitDone := make(chan struct{})
	go func() {
		_, _ = loop.Wait(context.Background(), time.Second)
		close(waitDone)
	}()
	stopDone := make(chan struct{})
	go func() {
		_ = loop.Stop()
		close(stopDone)
	}()
	deadline := time.After(500 * time.Millisecond)
	for waitDone != nil || stopDone != nil {
		select {
		case <-waitDone:
			waitDone = nil
		case <-stopDone:
			stopDone = nil
		case <-deadline:
			t.Fatal("concurrent Wait/Stop did not both join")
		}
	}
}

func TestSQLWorkloadScalingDownClosesRetiredWorkerSession(t *testing.T) {
	state := &sessionCleanupTestState{}
	database := newSessionCleanupTestDatabase(t, state)
	taggedPool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	taggedPool.SetMaxOpenConns(1)
	taggedPool.SetMaxIdleConns(1)
	conn, err := taggedPool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := &TaggedConn{Conn: conn, pool: taggedPool, db: database}
	database.mu.Lock()
	database.tagged[tagged] = struct{}{}
	database.mu.Unlock()

	started := make(chan struct{})
	workload := newSQLWorkload(
		context.Background(),
		&Runtime{Config: database.cfg},
		"scale-down",
		1,
		func(ctx context.Context, _ *sql.Conn, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	)
	workload.sessions[0] = tagged
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	if err := workload.SetTarget(0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		database.mu.Lock()
		remaining := len(database.tagged)
		database.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	database.mu.Lock()
	remaining := len(database.tagged)
	database.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("scale-down left %d retired tagged sessions open", remaining)
	}
	if err := workload.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSQLWorkloadPreparesFixedWorkerSessionsConcurrently(t *testing.T) {
	start := make(chan struct{})
	workload := newSQLWorkloadWithStartGate(
		context.Background(),
		&Runtime{},
		"fixed-session-prepare",
		3,
		nil,
		start,
	)
	var active, peak atomic.Int64
	allStarted := make(chan struct{})
	release := make(chan struct{})
	var readyOnce sync.Once
	workload.sessionOpener = func(ctx context.Context, workerID int) (*TaggedConn, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		if current == 3 {
			readyOnce.Do(func() { close(allStarted) })
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return &TaggedConn{}, nil
		}
	}
	done := make(chan error, 1)
	go func() { done <- workload.PrepareSessions(context.Background(), 3) }()
	select {
	case <-allStarted:
	case <-time.After(time.Second):
		t.Fatal("session preparation was serialized")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != 3 {
		t.Fatalf("peak concurrent session opens=%d, want 3", got)
	}
	workload.mu.Lock()
	prepared := len(workload.sessions)
	workload.sessions = map[int]*TaggedConn{}
	workload.mu.Unlock()
	if prepared != 3 {
		t.Fatalf("prepared sessions=%d, want 3", prepared)
	}
}

func TestSQLWorkloadStopReportsRetiredSessionCloseFailure(t *testing.T) {
	state := &sessionCleanupTestState{failClose: true}
	database := newSessionCleanupTestDatabase(t, state)
	taggedPool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	conn, err := taggedPool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := &TaggedConn{Conn: conn, pool: taggedPool, db: database}
	database.mu.Lock()
	database.tagged[tagged] = struct{}{}
	database.mu.Unlock()

	started := make(chan struct{})
	workload := newSQLWorkload(
		context.Background(),
		&Runtime{Config: database.cfg},
		"scale-down-close-error",
		1,
		func(ctx context.Context, _ *sql.Conn, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	)
	workload.sessions[0] = tagged
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := workload.SetTarget(0); err != nil {
		t.Fatal(err)
	}
	if err := workload.Stop(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Stop error=%v, want retired close failure", err)
	}
}

func TestSQLWorkloadRetireFailureDoesNotRequeueClosedSession(t *testing.T) {
	state := &sessionCleanupTestState{}
	database := newSessionCleanupTestDatabase(t, state)
	taggedPool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	conn, err := taggedPool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tagged := &TaggedConn{Conn: conn, pool: taggedPool, db: database}
	database.mu.Lock()
	database.tagged[tagged] = struct{}{}
	database.mu.Unlock()

	started := make(chan struct{})
	cleanupFailure := errors.New("retire cleanup failed")
	var cleanupCalls atomic.Int64
	workload := newSQLWorkloadWithCleanup(
		context.Background(),
		&Runtime{Config: database.cfg},
		"scale-down-cleanup-error",
		1,
		func(ctx context.Context, _ *sql.Conn, _ int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		func(context.Context, *sql.Conn, int) error {
			cleanupCalls.Add(1)
			return cleanupFailure
		},
	)
	workload.sessions[0] = tagged
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := workload.SetTarget(0); err != nil {
		t.Fatal(err)
	}
	workload.group.wg.Wait()

	workload.mu.Lock()
	remainingSessions := len(workload.sessions)
	workload.mu.Unlock()
	if remainingSessions != 0 {
		t.Fatalf("retire failure requeued %d already-closed sessions", remainingSessions)
	}
	database.mu.Lock()
	remainingTagged := len(database.tagged)
	database.mu.Unlock()
	if remainingTagged != 0 {
		t.Fatalf("retire failure left %d tagged database sessions", remainingTagged)
	}
	if err := workload.Stop(context.Background()); !errors.Is(err, cleanupFailure) {
		t.Fatalf("Stop error=%v, want original retire cleanup failure", err)
	}
	if calls := cleanupCalls.Load(); calls != 1 {
		t.Fatalf("retire cleanup calls=%d, want exactly one", calls)
	}
}

func TestSQLWorkloadStopIgnoresBadConnectionCausedByWorkerCancellation(t *testing.T) {
	state := &stopCancellationTestState{started: make(chan struct{})}
	workload := newStopCancellationTestWorkload(t, state)
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.started:
	case <-time.After(time.Second):
		t.Fatal("workload operation did not start")
	}

	if err := workload.Stop(context.Background()); err != nil {
		t.Fatalf("Stop reported the connection invalidated by its own cancellation: %v", err)
	}
	if snapshot := workload.Snapshot(); snapshot.Errors != 0 {
		t.Fatalf("canceled workload errors=%d first_error=%q", snapshot.Errors, snapshot.FirstError)
	}
}

func TestSQLWorkloadStopReportsBadConnectionAfterRealWorkerFailure(t *testing.T) {
	state := &stopCancellationTestState{
		started: make(chan struct{}),
		workErr: errors.New("network read failed"),
	}
	workload := newStopCancellationTestWorkload(t, state)
	if err := workload.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-state.started:
	case <-time.After(time.Second):
		t.Fatal("workload operation did not start")
	}
	deadline := time.Now().Add(time.Second)
	for workload.Snapshot().Errors == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if snapshot := workload.Snapshot(); snapshot.Errors != 1 ||
		!strings.Contains(snapshot.FirstError, "network read failed") {
		t.Fatalf("worker failure snapshot=%+v", snapshot)
	}

	if err := workload.Stop(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("Stop error=%v, want the independent bad connection", err)
	}
}

func TestCanceledWorkerConnectionErrorTreeIgnoresOnlyCancellationFailures(t *testing.T) {
	cancellationTree := errors.Join(
		driver.ErrBadConn,
		errors.Join(sql.ErrConnDone),
	)
	if got := normalizeCanceledWorkerConnectionError(cancellationTree, true); got != nil {
		t.Fatalf("canceled connection error tree=%v, want nil", got)
	}

	realFailure := errors.New("rollback protocol failed")
	mixedTree := errors.Join(driver.ErrBadConn, sql.ErrConnDone, realFailure)
	if got := normalizeCanceledWorkerConnectionError(mixedTree, true); got == nil ||
		!errors.Is(got, realFailure) {
		t.Fatalf("mixed cancellation and real error tree=%v, want real failure retained", got)
	}

	if got := normalizeCanceledWorkerConnectionError(cancellationTree, false); got == nil ||
		!errors.Is(got, driver.ErrBadConn) || !errors.Is(got, sql.ErrConnDone) {
		t.Fatalf("uncanceled connection error tree=%v, want all errors retained", got)
	}
}

type stopCancellationTestState struct {
	mu        sync.Mutex
	started   chan struct{}
	startOnce sync.Once
	bad       bool
	workErr   error
}

type stopCancellationTestConnector struct {
	state *stopCancellationTestState
}

func (c *stopCancellationTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &stopCancellationTestConn{state: c.state}, nil
}

func (c *stopCancellationTestConnector) Driver() driver.Driver {
	return stopCancellationTestDriver{state: c.state}
}

type stopCancellationTestDriver struct {
	state *stopCancellationTestState
}

func (d stopCancellationTestDriver) Open(string) (driver.Conn, error) {
	return (&stopCancellationTestConnector{state: d.state}).Connect(context.Background())
}

type stopCancellationTestConn struct {
	state *stopCancellationTestState
}

func (*stopCancellationTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*stopCancellationTestConn) Close() error { return nil }

func (*stopCancellationTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *stopCancellationTestConn) ExecContext(
	ctx context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	switch statement {
	case "BLOCKING WORK":
		c.state.startOnce.Do(func() { close(c.state.started) })
		c.state.mu.Lock()
		workErr := c.state.workErr
		if workErr != nil {
			c.state.bad = true
		}
		c.state.mu.Unlock()
		if workErr != nil {
			return nil, workErr
		}
		<-ctx.Done()
		c.state.mu.Lock()
		c.state.bad = true
		c.state.mu.Unlock()
		return nil, ctx.Err()
	case "RESET WORKLOAD STATE":
		c.state.mu.Lock()
		bad := c.state.bad
		c.state.mu.Unlock()
		if bad {
			return nil, driver.ErrBadConn
		}
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected statement: " + statement)
	}
}

func newStopCancellationTestWorkload(
	t *testing.T,
	state *stopCancellationTestState,
) *sqlWorkload {
	t.Helper()
	pool := sql.OpenDB(&stopCancellationTestConnector{state: state})
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	databaseCtx, cancelDatabase := context.WithCancel(context.Background())
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: databaseCtx, cancel: cancelDatabase, pool: pool,
		tagged: map[*TaggedConn]struct{}{},
	}
	tagged := &TaggedConn{Conn: conn, pool: pool, db: database}
	database.tagged[tagged] = struct{}{}
	runtime := &Runtime{Config: database.cfg, Database: database}
	workload := newSQLWorkloadWithCleanup(
		context.Background(), runtime, "stop-cancellation", 1,
		func(ctx context.Context, conn *sql.Conn, _ int) error {
			_, err := conn.ExecContext(ctx, "BLOCKING WORK")
			return err
		},
		func(ctx context.Context, conn *sql.Conn, _ int) error {
			_, err := conn.ExecContext(ctx, "RESET WORKLOAD STATE")
			return err
		},
	)
	workload.sessions[0] = tagged
	t.Cleanup(func() {
		cancelDatabase()
		_ = workload.Stop(context.Background())
		_ = pool.Close()
	})
	return workload
}
