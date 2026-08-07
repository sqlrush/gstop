package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestApplicationNameUsesExactRunOwnershipTag(t *testing.T) {
	got, err := ApplicationName("run-1", "tp_cpu", "7")
	if err != nil {
		t.Fatal(err)
	}
	if got != "gsbench/run-1/tp_cpu/7" {
		t.Fatalf("tag = %q", got)
	}
}

func TestApplicationNameKeepsReadableIdentityWhenItFits(t *testing.T) {
	got, err := ApplicationName(
		"run-1",
		"memory_session_context_growth",
		"prepared-control",
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = "gsbench/run-1/memory_session_context_growth/prepared-control"
	if got != want {
		t.Fatalf("tag = %q, want %q", got, want)
	}
}

func TestApplicationNameKeepsLegacyLongRunWhenIdentityFits(t *testing.T) {
	const runID = "1234567890123456789012"
	got, err := ApplicationName(runID, "tp_cpu", "worker")
	if err != nil {
		t.Fatal(err)
	}
	const want = "gsbench/1234567890123456789012/tp_cpu/worker"
	if got != want {
		t.Fatalf("tag = %q, want legacy-readable %q", got, want)
	}
}

func TestApplicationNameRejectsUnsafeComponents(t *testing.T) {
	if _, err := ApplicationName("run/other", "tp_cpu", "7"); err == nil {
		t.Fatal("expected unsafe run id error")
	}
}

func TestApplicationNameBoundsCatalogScenariosAndPreservesWorkerRoles(t *testing.T) {
	const runID = "20260726T123456-abcde"
	seen := make(map[string]string)
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		for _, workerID := range []string{"blocker", "waiter", "chain-2"} {
			got, err := ApplicationName(runID, definition.Name, workerID)
			if err != nil {
				t.Fatalf("%s/%s: %v", definition.Name, workerID, err)
			}
			if len(got) > 63 {
				t.Errorf("%s/%s application name is %d bytes: %q", definition.Name, workerID, len(got), got)
			}
			if !strings.HasSuffix(got, "/"+workerID) {
				t.Errorf("%s/%s lost worker role: %q", definition.Name, workerID, got)
			}
			if previous, exists := seen[got]; exists {
				t.Errorf("%s/%s collided with %s: %q", definition.Name, workerID, previous, got)
			}
			seen[got] = definition.Name + "/" + workerID

			repeated, err := ApplicationName(runID, definition.Name, workerID)
			if err != nil {
				t.Fatalf("repeat %s/%s: %v", definition.Name, workerID, err)
			}
			if repeated != got {
				t.Errorf("%s/%s application name is unstable: %q then %q", definition.Name, workerID, got, repeated)
			}
		}
	}
}

func TestApplicationNameBoundsAndDistinguishesLongValidComponents(t *testing.T) {
	inputs := [][3]string{
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "b", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "b", strings.Repeat("w", 80) + "a"},
		{strings.Repeat("r", 80) + "a", strings.Repeat("s", 80) + "a", strings.Repeat("w", 80) + "b"},
	}
	seen := make(map[string]bool)
	for _, input := range inputs {
		got, err := ApplicationName(input[0], input[1], input[2])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 63 {
			t.Errorf("application name is %d bytes: %q", len(got), got)
		}
		if seen[got] {
			t.Errorf("long valid components collided at %q", got)
		}
		seen[got] = true
	}
}

func TestCompressedRunTokenCannotAliasValidRawRunInput(t *testing.T) {
	longRunID := strings.Repeat("r", 80) + "a"
	applicationName, err := ApplicationName(longRunID, "tp_cpu", "worker")
	if err != nil {
		t.Fatal(err)
	}
	emittedRunToken := strings.Split(applicationName, "/")[1]
	alias, aliasErr := ApplicationName(emittedRunToken, "tp_cpu", "worker")
	if aliasErr == nil && alias == applicationName {
		t.Fatalf("long run %q aliases raw run input %q", longRunID, emittedRunToken)
	}
	if _, _, err := TaggedSessionPredicate(emittedRunToken); err == nil {
		t.Fatalf("compressed run token %q is accepted as a stored run ID", emittedRunToken)
	}
}

func TestCompressedScenarioTokenCannotAliasValidRawScenarioInput(t *testing.T) {
	const runID = "20260726T123456-abcde"
	longScenario := strings.Repeat("scenario", 10) + "a"
	applicationName, err := ApplicationName(runID, longScenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	emittedScenarioToken := strings.Split(applicationName, "/")[2]
	alias, aliasErr := ApplicationName(runID, emittedScenarioToken, "blocker")
	if aliasErr == nil && alias == applicationName {
		t.Fatalf("long scenario %q aliases raw scenario input %q", longScenario, emittedScenarioToken)
	}
	if _, err := taggedScenarioPattern(runID, emittedScenarioToken); err == nil {
		t.Fatalf("compressed scenario token %q is accepted as a raw scenario", emittedScenarioToken)
	}
}

func TestTaggedSessionPredicateDoesNotMatchRunPrefixCollision(t *testing.T) {
	query, args, err := TaggedSessionPredicate("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "application_name LIKE $1") {
		t.Fatalf("query = %q", query)
	}
	arg := onlyStringArgument(t, args)
	if arg != "gsbench/run-1/%" {
		t.Fatalf("arg = %q", arg)
	}
	if strings.HasPrefix("gsbench/run-10/tp_cpu/1", strings.TrimSuffix(arg, "%")) {
		t.Fatal("run-1 ownership prefix also matched run-10")
	}
}

func TestTaggedSessionStateSQLIgnoresThreadPoolWorkerRows(t *testing.T) {
	query, args, err := taggedSessionStateSQL("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "COALESCE(a.sessionid,0)<>0") ||
		!strings.Contains(query, "a.backend_start IS NOT NULL") {
		t.Fatalf("state query includes thread-pool worker rows: %q", query)
	}
	if got := onlyStringArgument(t, args); got != "gsbench/run-1/%" {
		t.Fatalf("state query argument=%q", got)
	}
}

func TestTaggedSessionPredicateIncludesLegacyLongRunPrefix(t *testing.T) {
	const runID = "1234567890123456789012"
	query, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "application_name LIKE $1") ||
		!strings.Contains(query, "application_name LIKE $2") {
		t.Fatalf("query = %q", query)
	}
	if strings.Contains(query, runID) {
		t.Fatalf("query interpolates stored run ID: %q", query)
	}
	const legacyPattern = "gsbench/1234567890123456789012/%"
	var foundLegacy bool
	for _, arg := range args {
		if arg == legacyPattern {
			foundLegacy = true
		}
	}
	if !foundLegacy {
		t.Fatalf("predicate args = %q, want legacy pattern %q included", args, legacyPattern)
	}
	legacyApplicationName := "gsbench/1234567890123456789012/tp_cpu/worker"
	if !strings.HasPrefix(legacyApplicationName, strings.TrimSuffix(legacyPattern, "%")) {
		t.Fatalf("predicate %q does not discover legacy application %q", legacyPattern, legacyApplicationName)
	}
}

func TestTaggedSessionPredicateAlignsWithApplicationNamesAndEscapesLIKE(t *testing.T) {
	const runID = "20260726T123456_abcd"
	query, args, err := TaggedSessionPredicate(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `ESCAPE E'\\'`) {
		t.Fatalf("query has no explicit LIKE escape: %q", query)
	}
	arg := onlyStringArgument(t, args)
	if arg != `gsbench/20260726T123456\_abcd/%` {
		t.Fatalf("arg = %q", arg)
	}
	prefix := literalLIKEPrefix(t, arg)
	for _, definition := range DefaultScenarioCatalog().Definitions() {
		got, err := ApplicationName(runID, definition.Name, "worker")
		if err != nil {
			t.Fatalf("%s: %v", definition.Name, err)
		}
		if !strings.HasPrefix(serverApplicationName(got), prefix) {
			t.Errorf("run predicate %q does not match %q", arg, got)
		}
	}
}

func TestScenarioPatternAlignsWithBoundedApplicationName(t *testing.T) {
	const (
		runID         = "20260726T123456-abcde"
		scenario      = "lockmode_shareupdateexclusive_accessexclusive"
		otherScenario = "lockmode_shareupdateexclusive_exclusive"
	)
	pattern, err := taggedScenarioPattern(runID, scenario)
	if err != nil {
		t.Fatal(err)
	}
	prefix := literalLIKEPrefix(t, pattern)
	got, err := ApplicationName(runID, scenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(serverApplicationName(got), prefix) {
		t.Fatalf("scenario pattern %q does not match server application name %q", pattern, serverApplicationName(got))
	}
	other, err := ApplicationName(runID, otherScenario, "blocker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(serverApplicationName(other), prefix) {
		t.Fatalf("scenario pattern %q also matches %q", pattern, serverApplicationName(other))
	}
}

func onlyStringArgument(t *testing.T, args []any) string {
	t.Helper()
	if len(args) != 1 {
		t.Fatalf("args = %q, want one string", args)
	}
	arg, ok := args[0].(string)
	if !ok {
		t.Fatalf("arg type = %T, want string", args[0])
	}
	return arg
}

func serverApplicationName(applicationName string) string {
	if len(applicationName) > 63 {
		return applicationName[:63]
	}
	return applicationName
}

func literalLIKEPrefix(t *testing.T, pattern string) string {
	t.Helper()
	if !strings.HasSuffix(pattern, "%") {
		t.Fatalf("LIKE pattern has no trailing wildcard: %q", pattern)
	}
	pattern = strings.TrimSuffix(pattern, "%")
	var prefix strings.Builder
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '\\':
			index++
			if index == len(pattern) {
				t.Fatalf("LIKE pattern ends in an escape: %q", pattern)
			}
			prefix.WriteByte(pattern[index])
		case '%', '_':
			t.Fatalf("LIKE pattern contains an unescaped wildcard: %q", pattern)
		default:
			prefix.WriteByte(pattern[index])
		}
	}
	return prefix.String()
}

func TestNormalizeConnectionCloseErrorIgnoresAlreadyClosedResources(t *testing.T) {
	for _, err := range []error{
		sql.ErrConnDone,
		net.ErrClosed,
		&net.OpError{Op: "write", Net: "tcp", Err: net.ErrClosed},
	} {
		if got := normalizeConnectionCloseError(err); got != nil {
			t.Errorf("normalizeConnectionCloseError(%v)=%v, want nil", err, got)
		}
	}

	unexpected := errors.New("close failed")
	if got := normalizeConnectionCloseError(unexpected); !errors.Is(got, unexpected) {
		t.Fatalf("normalizeConnectionCloseError(%v)=%v", unexpected, got)
	}
}

type sessionCleanupTestConnector struct {
	state *sessionCleanupTestState
}

type maintenanceExecTestConnector struct {
	state *maintenanceExecTestState
}

func (c *maintenanceExecTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &maintenanceExecTestConn{state: c.state}, nil
}

func (c *maintenanceExecTestConnector) Driver() driver.Driver {
	return maintenanceExecTestDriver{state: c.state}
}

type maintenanceExecTestDriver struct {
	state *maintenanceExecTestState
}

func (d maintenanceExecTestDriver) Open(string) (driver.Conn, error) {
	return (&maintenanceExecTestConnector{state: d.state}).Connect(
		context.Background(),
	)
}

type maintenanceExecTestState struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type maintenanceExecTestConn struct {
	state *maintenanceExecTestState
}

func (*maintenanceExecTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*maintenanceExecTestConn) Close() error { return nil }

func (*maintenanceExecTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *maintenanceExecTestConn) ExecContext(
	ctx context.Context,
	_ string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	c.state.once.Do(func() { close(c.state.started) })
	select {
	case <-c.state.release:
		return driver.RowsAffected(1), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestDatasetExecutorDoesNotApplyWorkloadQueryTimeoutToMaintenanceDDL(
	t *testing.T,
) {
	const workloadQueryTimeout = 20 * time.Millisecond
	state := &maintenanceExecTestState{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	pool := sql.OpenDB(&maintenanceExecTestConnector{state: state})
	databaseContext, cancelDatabase := context.WithCancel(context.Background())
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{
			QueryTimeout: workloadQueryTimeout,
		}},
		ctx: databaseContext, cancel: cancelDatabase, pool: pool,
		tagged: map[*TaggedConn]struct{}{},
	}
	t.Cleanup(func() {
		cancelDatabase()
		_ = pool.Close()
	})

	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	result := make(chan error, 1)
	go func() {
		result <- (initializationDatasetExecutor{
			dbDatasetExecutor: dbDatasetExecutor{db: database},
		}).Exec(
			parent,
			`CREATE INDEX items_value_idx ON "gsbench".items (value)`,
		)
	}()

	select {
	case <-state.started:
	case <-parent.Done():
		t.Fatalf("maintenance DDL did not start: %v", parent.Err())
	}
	select {
	case err := <-result:
		t.Fatalf(
			"maintenance DDL ended at the workload query timeout: %v",
			err,
		)
	case <-time.After(3 * workloadQueryTimeout):
		close(state.release)
	}
	if err := <-result; err != nil {
		t.Fatalf("maintenance DDL failed after release: %v", err)
	}
}

func TestMaintenanceSessionDoesNotApplyWorkloadQueryTimeout(t *testing.T) {
	const workloadQueryTimeout = 20 * time.Millisecond
	state := &maintenanceExecTestState{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	pool := sql.OpenDB(&maintenanceExecTestConnector{state: state})
	databaseContext, cancelDatabase := context.WithCancel(context.Background())
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{
			QueryTimeout: workloadQueryTimeout,
		}},
		ctx: databaseContext, cancel: cancelDatabase, pool: pool,
		tagged: map[*TaggedConn]struct{}{},
	}
	t.Cleanup(func() {
		cancelDatabase()
		_ = pool.Close()
	})

	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	result := make(chan error, 1)
	go func() {
		result <- database.ExecMaintenanceSession(parent, "ANALYZE plan_data")
	}()

	select {
	case <-state.started:
	case <-parent.Done():
		t.Fatalf("maintenance session did not start: %v", parent.Err())
	}
	select {
	case err := <-result:
		t.Fatalf("maintenance session ended at workload timeout: %v", err)
	case <-time.After(3 * workloadQueryTimeout):
		close(state.release)
	}
	if err := <-result; err != nil {
		t.Fatalf("maintenance session failed after release: %v", err)
	}
}

func TestDatasetExecutorDefaultsToWorkloadQueryTimeout(t *testing.T) {
	const workloadQueryTimeout = 20 * time.Millisecond
	state := &maintenanceExecTestState{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	pool := sql.OpenDB(&maintenanceExecTestConnector{state: state})
	databaseContext, cancelDatabase := context.WithCancel(context.Background())
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{
			QueryTimeout: workloadQueryTimeout,
		}},
		ctx: databaseContext, cancel: cancelDatabase, pool: pool,
		tagged: map[*TaggedConn]struct{}{},
	}
	t.Cleanup(func() {
		cancelDatabase()
		_ = pool.Close()
	})

	result := make(chan error, 1)
	go func() {
		result <- (dbDatasetExecutor{db: database}).Exec(
			context.Background(),
			`UPDATE "gsbench".meta_runs SET stop_requested=true`,
		)
	}()

	select {
	case <-state.started:
	case <-time.After(time.Second):
		t.Fatal("bounded dataset operation did not start")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded dataset operation error=%v", err)
		}
	case <-time.After(3 * workloadQueryTimeout):
		close(state.release)
		err := <-result
		t.Fatalf(
			"default dataset operation exceeded workload query timeout: %v",
			err,
		)
	}
}

func TestOperationContextAllowsBoundedFinalizationAfterDatabaseRootCancellation(
	t *testing.T,
) {
	state := &maintenanceExecTestState{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	pool := sql.OpenDB(&maintenanceExecTestConnector{state: state})
	defer pool.Close()
	databaseContext, cancelDatabase := context.WithCancel(context.Background())
	cancelDatabase()
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: databaseContext, pool: pool,
	}
	result := make(chan error, 1)
	go func() {
		_, err := database.Exec(
			context.Background(),
			`UPDATE "gsbench".meta_runs SET status='complete'`,
		)
		result <- err
	}()
	select {
	case <-state.started:
	case <-time.After(time.Second):
		t.Fatal("finalization query did not start after database root cancellation")
	}
	close(state.release)
	if err := <-result; err != nil {
		t.Fatalf("finalization query error=%v", err)
	}
}

func (c *sessionCleanupTestConnector) Connect(context.Context) (driver.Conn, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.connections++
	return &sessionCleanupTestConn{
		id:    c.state.connections,
		state: c.state,
	}, nil
}

func (c *sessionCleanupTestConnector) Driver() driver.Driver {
	return sessionCleanupTestDriver{state: c.state}
}

type sessionCleanupTestDriver struct {
	state *sessionCleanupTestState
}

func (d sessionCleanupTestDriver) Open(string) (driver.Conn, error) {
	return (&sessionCleanupTestConnector{state: d.state}).Connect(
		context.Background(),
	)
}

type sessionCleanupTestState struct {
	mu                sync.Mutex
	connections       int
	dirty             map[int]bool
	closed            map[int]bool
	statements        map[int][]string
	failRollback      bool
	failResetAll      bool
	failClose         bool
	cancelOnWorkError context.CancelFunc
}

type sessionCleanupTestConn struct {
	id    int
	state *sessionCleanupTestState
}

func (*sessionCleanupTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *sessionCleanupTestConn) Close() error {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.closed[c.id] = true
	if c.state.failClose {
		return errors.New("close failed")
	}
	return nil
}

func (*sessionCleanupTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *sessionCleanupTestConn) ExecContext(
	_ context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.statements[c.id] = append(c.state.statements[c.id], statement)
	switch statement {
	case "SET default_statistics_target=-2":
		c.state.dirty[c.id] = true
	case "ANALYZE broken":
		if c.state.cancelOnWorkError != nil {
			c.state.cancelOnWorkError()
		}
		return nil, errors.New("analyze failed")
	case "ROLLBACK":
		if c.state.failRollback {
			return nil, errors.New("rollback failed")
		}
	case "RESET ALL":
		if c.state.failResetAll {
			return nil, errors.New("reset failed")
		}
		c.state.dirty[c.id] = false
	case "ASSERT SESSION CLEAN":
		if c.state.dirty[c.id] {
			return nil, errors.New("session is dirty")
		}
	}
	return driver.RowsAffected(1), nil
}

func newSessionCleanupTestDatabase(
	t *testing.T,
	state *sessionCleanupTestState,
) *Database {
	t.Helper()
	state.dirty = make(map[int]bool)
	state.closed = make(map[int]bool)
	state.statements = make(map[int][]string)
	pool := sql.OpenDB(&sessionCleanupTestConnector{state: state})
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	databaseContext, cancel := context.WithCancel(context.Background())
	database := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: databaseContext, cancel: cancel, pool: pool,
		tagged: map[*TaggedConn]struct{}{},
	}
	t.Cleanup(func() {
		cancel()
		if err := pool.Close(); err != nil {
			t.Errorf("close session cleanup test pool: %v", err)
		}
	})
	return database
}

func TestExecSessionCleansFailedSessionBeforeReturningItToPool(t *testing.T) {
	state := &sessionCleanupTestState{}
	database := newSessionCleanupTestDatabase(t, state)
	parent, cancel := context.WithCancel(context.Background())
	state.cancelOnWorkError = cancel

	err := database.ExecSession(
		parent,
		"SET default_statistics_target=-2",
		"ANALYZE broken",
		"RESET default_statistics_target",
	)
	if err == nil || !strings.Contains(err.Error(), "analyze failed") {
		t.Fatalf("ExecSession() error=%v", err)
	}
	if _, err := database.Exec(
		context.Background(), "ASSERT SESSION CLEAN",
	); err != nil {
		t.Fatalf("connection returned to pool with dirty session: %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.connections != 1 {
		t.Fatalf("connections=%d, want cleaned connection reused", state.connections)
	}
	want := []string{
		"SET default_statistics_target=-2",
		"ANALYZE broken",
		"ROLLBACK",
		"RESET ALL",
		"ASSERT SESSION CLEAN",
	}
	if got := state.statements[1]; !equalStringSlices(got, want) {
		t.Fatalf("session statements=%v, want %v", got, want)
	}
}

func TestExecSessionDiscardsConnectionWhenFailureCleanupFails(t *testing.T) {
	state := &sessionCleanupTestState{
		failRollback: true,
		failResetAll: true,
	}
	database := newSessionCleanupTestDatabase(t, state)

	err := database.ExecSession(
		context.Background(),
		"SET default_statistics_target=-2",
		"ANALYZE broken",
		"RESET default_statistics_target",
	)
	if err == nil ||
		!strings.Contains(err.Error(), "rollback failed") ||
		!strings.Contains(err.Error(), "reset failed") {
		t.Fatalf("ExecSession() error=%v", err)
	}
	state.mu.Lock()
	state.failRollback = false
	state.failResetAll = false
	state.mu.Unlock()
	if _, err := database.Exec(
		context.Background(), "ASSERT SESSION CLEAN",
	); err != nil {
		t.Fatalf("replacement connection is not clean: %v", err)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if state.connections != 2 {
		t.Fatalf("connections=%d, want dirty connection replaced", state.connections)
	}
	if !state.closed[1] {
		t.Fatal("dirty physical connection was not closed")
	}
	wantFailedSession := []string{
		"SET default_statistics_target=-2",
		"ANALYZE broken",
		"ROLLBACK",
		"RESET ALL",
	}
	if got := state.statements[1]; !equalStringSlices(got, wantFailedSession) {
		t.Fatalf("failed session statements=%v, want %v", got, wantFailedSession)
	}
	if got := state.statements[2]; !equalStringSlices(
		got, []string{"ASSERT SESSION CLEAN"},
	) {
		t.Fatalf("replacement session statements=%v", got)
	}
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
