package gsbench

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

type advisoryLockTestState struct {
	tryResults    map[string]bool
	trySequences  map[string][]bool
	tryErrors     map[string]error
	unlockResult  map[string]bool
	unlockErrors  map[string]error
	events        []string
	physicalClose int
}

type advisoryLockTestConnector struct {
	state *advisoryLockTestState
}

func (c advisoryLockTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &advisoryLockTestConn{state: c.state}, nil
}

func (advisoryLockTestConnector) Driver() driver.Driver {
	return advisoryLockTestDriver{}
}

type advisoryLockTestDriver struct{}

func (advisoryLockTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type advisoryLockTestConn struct {
	state *advisoryLockTestState
}

func (*advisoryLockTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *advisoryLockTestConn) Close() error {
	c.state.physicalClose++
	return nil
}

func (*advisoryLockTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *advisoryLockTestConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	key, _ := args[0].Value.(string)
	var result bool
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"):
		c.state.events = append(c.state.events, "try "+key)
		if err := c.state.tryErrors[key]; err != nil {
			return nil, err
		}
		if sequence := c.state.trySequences[key]; len(sequence) != 0 {
			result = sequence[0]
			c.state.trySequences[key] = sequence[1:]
		} else {
			result = c.state.tryResults[key]
		}
	case strings.Contains(query, "pg_advisory_unlock"):
		c.state.events = append(c.state.events, "unlock "+key)
		if err := c.state.unlockErrors[key]; err != nil {
			return nil, err
		}
		result = c.state.unlockResult[key]
	default:
		return nil, errors.New("unexpected advisory-lock query")
	}
	return &advisoryLockBoolRows{value: result}, nil
}

type advisoryLockBoolRows struct {
	value bool
	read  bool
}

type cleanupDatasetTestExecutor struct {
	version    string
	versionErr error
	statements []string
}

type stopRequestTestExecutor struct {
	query string
	args  []any
}

type orderedRestoreTestLock struct {
	name   string
	events *[]string
	next   RestoreLock
}

type protectedCleanupTestLock struct {
	orderedRestoreTestLock
	version    string
	statements []string
}

type initializationReleaseTestLock struct {
	events *[]string
	err    error
}

func (l *initializationReleaseTestLock) Release() error {
	if l.events != nil {
		*l.events = append(*l.events, "release restore")
	}
	return l.err
}

func (l *protectedCleanupTestLock) DatasetVersion(
	context.Context,
	string,
) (string, error) {
	return l.version, nil
}

func (l *protectedCleanupTestLock) Exec(
	_ context.Context,
	query string,
	_ ...any,
) error {
	l.statements = append(l.statements, query)
	return nil
}

func (l *orderedRestoreTestLock) Release() error {
	*l.events = append(*l.events, "release "+l.name)
	if l.next != nil {
		return l.next.Release()
	}
	return nil
}

func (e *stopRequestTestExecutor) Exec(
	_ context.Context,
	query string,
	args ...any,
) error {
	e.query = query
	e.args = append([]any(nil), args...)
	return nil
}

func (e *cleanupDatasetTestExecutor) DatasetVersion(
	context.Context,
	string,
) (string, error) {
	return e.version, e.versionErr
}

func (*cleanupDatasetTestExecutor) RecordDatasetVersion(
	context.Context,
	string,
	string,
) error {
	return errors.New("cleanup must not record a dataset version")
}

func (e *cleanupDatasetTestExecutor) Exec(
	_ context.Context,
	query string,
	_ ...any,
) error {
	e.statements = append(e.statements, query)
	return nil
}

func (*advisoryLockBoolRows) Columns() []string { return []string{"locked"} }
func (*advisoryLockBoolRows) Close() error      { return nil }
func (r *advisoryLockBoolRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.value
	return nil
}

func newAdvisoryLockTestBackend(
	t *testing.T,
	state *advisoryLockTestState,
) *databaseRestoreBackend {
	t.Helper()
	pool := sql.OpenDB(advisoryLockTestConnector{state: state})
	t.Cleanup(func() { _ = pool.Close() })
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
		Safety:   SafetyConfig{QueryTimeout: time.Second},
	}
	db := &Database{cfg: cfg, ctx: context.Background(), pool: pool}
	return &databaseRestoreBackend{
		db: db, cfg: cfg, requirePlanLock: true,
	}
}

func TestExitCodeForOutcome(t *testing.T) {
	for _, test := range []struct {
		outcome Outcome
		want    int
	}{
		{OutcomeSuccess, 0},
		{OutcomeUnverified, 0},
		{OutcomeNotApplicable, 0},
		{OutcomeDegraded, 3},
		{OutcomeNotImplemented, 1},
		{OutcomeFailed, 1},
		{OutcomeRestoreFailed, 1},
	} {
		if got := exitCodeForOutcome(test.outcome); got != test.want {
			t.Fatalf("outcome=%s exit code=%d want=%d", test.outcome, got, test.want)
		}
	}
}

func TestStoredScenarioCodesContainPlanChangeRejectsLegacyNames(
	t *testing.T,
) {
	tests := []struct {
		value string
		want  bool
	}{
		{"101,501", false},
		{"101,605", true},
		{"tp_cpu,plan_index_drop", false},
		{"plan_regression", false},
	}
	for _, test := range tests {
		if got := storedScenarioCodesContainPlanChange(
			test.value,
		); got != test.want {
			t.Fatalf("%q got=%v want=%v", test.value, got, test.want)
		}
	}
}

func TestWithPlanScenarioDatabaseLockFailsBusyBeforeWork(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
		Run:      RunConfig{ScenarioCodes: []ScenarioCode{101, 606}},
	}
	wantErr := errors.New("plan lock busy")
	workCalls := 0
	identity := ""

	_, err := withPlanScenarioDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(_ context.Context, _ *Database, gotIdentity string) (func() error, error) {
			identity = gotIdentity
			return nil, wantErr
		},
		func() int {
			workCalls++
			return 0
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	if identity != "gsbench:plan:postgres:Bench" {
		t.Fatalf("identity=%q", identity)
	}
	if workCalls != 0 {
		t.Fatalf("work calls=%d want=0", workCalls)
	}
}

func TestWithPlanScenarioDatabaseLockReleasesAfterWork(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
		Run:      RunConfig{ScenarioCodes: []ScenarioCode{601}},
	}
	var events []string

	code, err := withPlanScenarioDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(context.Context, *Database, string) (func() error, error) {
			events = append(events, "acquire")
			return func() error {
				events = append(events, "release")
				return nil
			}, nil
		},
		func() int {
			events = append(events, "runner restore complete")
			return 3
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("exit code=%d want=3", code)
	}
	want := []string{"acquire", "runner restore complete", "release"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestCommandRunDryRunPlanScenarioSkipsDatabaseLock(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cfg := BenchConfig{
		Data: DataConfig{Schema: "Bench"},
		Run: RunConfig{
			ScenarioCodes: []ScenarioCode{601},
			DryRun:        true,
		},
	}

	if code := commandRun(
		context.Background(),
		nil,
		cfg,
		Environment{},
		Capabilities{},
		RiskA,
		log,
		"dry-run",
	); code != 0 {
		t.Fatalf("exit code=%d output=%s", code, output.String())
	}
	if !strings.Contains(output.String(), "run SUCCESS (dry run") {
		t.Fatalf("dry-run success not reported: %s", output.String())
	}
}

func TestWithPlanDatabaseLockUsesSharedSchemaIdentity(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	var events []string

	code, err := withPlanDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(_ context.Context, _ *Database, identity string) (func() error, error) {
			events = append(events, "acquire "+identity)
			return func() error {
				events = append(events, "release "+identity)
				return nil
			}, nil
		},
		func() int {
			events = append(events, "mutate baseline")
			return 0
		},
	)
	if err != nil || code != 0 {
		t.Fatalf("exit code=%d error=%v", code, err)
	}
	want := []string{
		"acquire gsbench:plan:postgres:Bench",
		"mutate baseline",
		"release gsbench:plan:postgres:Bench",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestFinishInitializationPlanLockForcesFailureOnReleaseError(
	t *testing.T,
) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	exitCode := 0
	wantErr := errors.New("unlock failed")

	finishInitializationPlanLock(
		func() error { return wantErr },
		log,
		&exitCode,
	)
	if exitCode != 1 {
		t.Fatalf("exit code=%d want=1", exitCode)
	}
	if !strings.Contains(output.String(), wantErr.Error()) {
		t.Fatalf("release error not logged: %s", output.String())
	}
}

func TestRestoreDatabaseAdvisoryKeysAcquirePlanBeforeRestore(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	want := []string{
		"gsbench:plan:postgres:Bench",
		"gsbench/restore/postgres/Bench",
	}
	if got := restoreDatabaseAdvisoryKeys(cfg, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys=%v want=%v", got, want)
	}
	if got := restoreDatabaseAdvisoryKeys(cfg, false); !reflect.DeepEqual(
		got,
		want[1:],
	) {
		t.Fatalf("outer-plan-lock keys=%v want=%v", got, want[1:])
	}
}

func TestRequestRestoreRunsStopUsesParameterizedTarget(t *testing.T) {
	for _, test := range []struct {
		name   string
		runID  string
		args   []any
		filter string
	}{
		{
			name: "specific run", runID: "run-1",
			args: []any{"run-1"}, filter: "run_id=$1 AND status='running'",
		},
		{
			name: "all running runs", filter: "WHERE status='running'",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := &stopRequestTestExecutor{}
			if err := requestRestoreRunsStop(
				context.Background(), executor, "Bench", test.runID,
			); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(executor.query, test.filter) {
				t.Fatalf("query=%q missing %q", executor.query, test.filter)
			}
			ownershipGate := `EXISTS (SELECT 1 FROM "Bench".meta_dataset ` +
				`WHERE key='dataset_version' AND value IN ('1','2','3','4'))`
			if !strings.Contains(executor.query, ownershipGate) {
				t.Fatalf("query=%q missing ownership gate %q", executor.query, ownershipGate)
			}
			if strings.Contains(executor.query, "run-1") {
				t.Fatalf("run ID was interpolated into SQL: %q", executor.query)
			}
			if !reflect.DeepEqual(executor.args, test.args) {
				t.Fatalf("args=%v want=%v", executor.args, test.args)
			}
		})
	}
}

func TestAcquireDatabaseRestoreLockRequestsStopAgainForLateRunRow(
	t *testing.T,
) {
	planKey := "gsbench:plan:postgres:Bench"
	state := &advisoryLockTestState{
		tryResults:   make(map[string]bool),
		trySequences: make(map[string][]bool),
		tryErrors:    make(map[string]error),
		unlockResult: make(map[string]bool),
		unlockErrors: make(map[string]error),
	}
	backend := newAdvisoryLockTestBackend(t, state)
	backend.restorePollInterval = time.Nanosecond
	acquireCalls := 0
	backend.acquirePlanRunLock = func(
		_ context.Context,
		_ *Database,
		identity string,
	) (func() error, error) {
		acquireCalls++
		state.events = append(state.events, "try "+identity)
		if acquireCalls == 1 {
			return nil, errors.New("plan lock busy")
		}
		return func() error {
			state.events = append(state.events, "release "+identity)
			return nil
		}, nil
	}
	requestCalls := 0
	backend.requestPlanLockOwnerStop = func(context.Context) error {
		requestCalls++
		if requestCalls == 1 {
			state.events = append(state.events, "request stop before run row")
		} else {
			state.events = append(state.events, "request stop matched late run row")
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := backend.acquirePlanLockForRestore(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"request stop before run row",
		"try " + planKey,
		"request stop matched late run row",
		"try " + planKey,
		"release " + planKey,
	}
	if !reflect.DeepEqual(state.events, want) {
		t.Fatalf("events=%v want=%v", state.events, want)
	}
}

func TestAcquirePlanFirstRestoreLockUsesPlanLocalRestoreOrder(t *testing.T) {
	var events []string
	backend := &databaseRestoreBackend{
		cfg: BenchConfig{
			Database: DatabaseConfig{Database: "postgres"},
			Data:     DataConfig{Schema: "Bench"},
		},
		requirePlanLock: true,
	}
	backend.requestPlanLockOwnerStop = func(context.Context) error {
		events = append(events, "request stop")
		return nil
	}
	backend.acquirePlanRunLock = func(
		context.Context,
		*Database,
		string,
	) (func() error, error) {
		events = append(events, "acquire plan")
		return func() error {
			events = append(events, "release plan")
			return nil
		}, nil
	}
	backend.acquireLocalRestoreLock = func(
		context.Context,
		string,
	) (RestoreLock, error) {
		events = append(events, "acquire local")
		return &orderedRestoreTestLock{name: "local", events: &events}, nil
	}
	backend.acquireDatabaseRestoreLock = func(
		_ context.Context,
		local RestoreLock,
	) (RestoreLock, error) {
		events = append(events, "acquire restore")
		return &orderedRestoreTestLock{
			name: "restore", events: &events, next: local,
		}, nil
	}

	lock, err := backend.acquirePlanFirstRestoreLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"request stop",
		"acquire plan",
		"acquire local",
		"acquire restore",
		"release restore",
		"release local",
		"release plan",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestInitializationMutationUsesPlanLocalRestoreLockOrder(t *testing.T) {
	var events []string
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	backend := &databaseRestoreBackend{cfg: cfg}
	backend.acquireLocalRestoreLock = func(
		context.Context,
		string,
	) (RestoreLock, error) {
		events = append(events, "acquire local")
		return &orderedRestoreTestLock{
			name: "local", events: &events,
		}, nil
	}
	backend.acquireDatabaseRestoreLock = func(
		_ context.Context,
		local RestoreLock,
	) (RestoreLock, error) {
		events = append(events, "acquire restore")
		return &orderedRestoreTestLock{
			name: "restore", events: &events, next: local,
		}, nil
	}
	log, err := NewRunLog(io.Discard, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	code, err := withPlanDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(context.Context, *Database, string) (func() error, error) {
			events = append(events, "acquire plan")
			return func() error {
				events = append(events, "release plan")
				return nil
			}, nil
		},
		func() int {
			return executeInitializationMutation(
				context.Background(),
				backend,
				log,
				acquireInitializationRestoreLock,
				func() error {
					wantPrefix := []string{
						"acquire plan",
						"acquire local",
						"acquire restore",
					}
					if !reflect.DeepEqual(events, wantPrefix) {
						return fmt.Errorf(
							"mutation lock prefix=%v want=%v",
							events,
							wantPrefix,
						)
					}
					events = append(events, "manager mutation")
					return nil
				},
			)
		},
	)
	if err != nil || code != 0 {
		t.Fatalf("exit code=%d error=%v events=%v", code, err, events)
	}
	want := []string{
		"acquire plan",
		"acquire local",
		"acquire restore",
		"manager mutation",
		"release restore",
		"release local",
		"release plan",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestInitializationRestoreLockFailureReleasesLocal(t *testing.T) {
	var events []string
	wantErr := errors.New("restore lock unavailable")
	backend := &databaseRestoreBackend{}
	backend.acquireLocalRestoreLock = func(
		context.Context,
		string,
	) (RestoreLock, error) {
		events = append(events, "acquire local")
		return &orderedRestoreTestLock{
			name: "local", events: &events,
		}, nil
	}
	backend.acquireDatabaseRestoreLock = func(
		context.Context,
		RestoreLock,
	) (RestoreLock, error) {
		events = append(events, "acquire restore")
		return nil, wantErr
	}

	lock, err := acquireInitializationRestoreLock(
		context.Background(), backend,
	)
	if lock != nil || !errors.Is(err, wantErr) {
		t.Fatalf("lock=%v error=%v", lock, err)
	}
	want := []string{"acquire local", "acquire restore", "release local"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestInitializationRestoreReleaseFailureReturnsExitOne(t *testing.T) {
	wantErr := errors.New("restore unlock failed")
	log, err := NewRunLog(io.Discard, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	mutationCalls := 0

	code := executeInitializationMutation(
		context.Background(),
		&databaseRestoreBackend{},
		log,
		func(
			context.Context,
			*databaseRestoreBackend,
		) (RestoreLock, error) {
			return &initializationReleaseTestLock{err: wantErr}, nil
		},
		func() error {
			mutationCalls++
			return nil
		},
	)
	if code != 1 || mutationCalls != 1 {
		t.Fatalf("exit code=%d mutation calls=%d", code, mutationCalls)
	}
}

func TestAcquireDatabaseRestoreLockReleasesPartialPlanLockOnFailure(
	t *testing.T,
) {
	planKey := "gsbench:plan:postgres:Bench"
	restoreKey := "gsbench/restore/postgres/Bench"
	for _, test := range []struct {
		name              string
		restoreTry        bool
		restoreErr        error
		wantPhysicalClose int
	}{
		{name: "busy", restoreTry: false},
		{
			name:              "query error",
			restoreErr:        errors.New("restore lock query failed"),
			wantPhysicalClose: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &advisoryLockTestState{
				tryResults: map[string]bool{
					planKey:    true,
					restoreKey: test.restoreTry,
				},
				tryErrors:    map[string]error{restoreKey: test.restoreErr},
				unlockResult: map[string]bool{planKey: true},
				unlockErrors: make(map[string]error),
			}
			backend := newAdvisoryLockTestBackend(t, state)

			lock, err := backend.acquireDatabaseLock(
				context.Background(),
				nil,
			)
			if err == nil || lock != nil {
				t.Fatalf("lock=%v error=%v want failed acquisition", lock, err)
			}
			want := []string{
				"try " + planKey,
				"try " + restoreKey,
				"unlock " + planKey,
			}
			if !reflect.DeepEqual(state.events, want) {
				t.Fatalf("events=%v want=%v", state.events, want)
			}
			if state.physicalClose != test.wantPhysicalClose {
				t.Fatalf(
					"physical closes=%d want=%d",
					state.physicalClose,
					test.wantPhysicalClose,
				)
			}
		})
	}
}

func TestDatabaseRestoreLockDiscardsConnectionWhenUnlockFails(
	t *testing.T,
) {
	planKey := "gsbench:plan:postgres:Bench"
	restoreKey := "gsbench/restore/postgres/Bench"
	for _, test := range []struct {
		name         string
		unlockResult bool
		unlockErr    error
	}{
		{name: "unlock query error", unlockErr: errors.New("unlock failed")},
		{name: "lock not held", unlockResult: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := &advisoryLockTestState{
				tryResults: map[string]bool{
					planKey: true, restoreKey: true,
				},
				tryErrors: make(map[string]error),
				unlockResult: map[string]bool{
					restoreKey: test.unlockResult,
					planKey:    true,
				},
				unlockErrors: map[string]error{restoreKey: test.unlockErr},
			}
			backend := newAdvisoryLockTestBackend(t, state)
			lock, err := backend.acquireDatabaseLock(
				context.Background(),
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Release(); err == nil {
				t.Fatal("unlock failure was not returned")
			}
			want := []string{
				"try " + planKey,
				"try " + restoreKey,
				"unlock " + restoreKey,
				"unlock " + planKey,
			}
			if !reflect.DeepEqual(state.events, want) {
				t.Fatalf("events=%v want=%v", state.events, want)
			}
			if state.physicalClose != 1 {
				t.Fatalf(
					"physical closes=%d want=1 discarded connection",
					state.physicalClose,
				)
			}
		})
	}
}

func TestWithPlanDatabaseLockFailsCleanupBusyBeforeDrop(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	dropCalls := 0
	wantErr := errors.New("plan lock busy")

	_, err := withPlanDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(context.Context, *Database, string) (func() error, error) {
			return nil, wantErr
		},
		func() int {
			dropCalls++
			return 0
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	if dropCalls != 0 {
		t.Fatalf("drop calls=%d want=0", dropCalls)
	}
}

func TestCleanupDataDryRunDoesNotTouchDatabase(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cfg := BenchConfig{
		Data: DataConfig{Schema: "Bench"},
		Run:  RunConfig{DryRun: true},
	}

	if code := cleanupData(context.Background(), nil, cfg, log); code != 0 {
		t.Fatalf("exit code=%d output=%s", code, output.String())
	}
	if !strings.Contains(output.String(), "DRY-RUN DROP SCHEMA") {
		t.Fatalf("dry-run cleanup was not reported: %s", output.String())
	}
}

func TestCommandCleanupRejectsDataWithRunIDBeforeDatabaseWork(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	code := commandCleanup(
		context.Background(),
		nil,
		BenchConfig{},
		log,
		"run-1",
		true,
	)
	if code != 1 {
		t.Fatalf("exit code=%d output=%s", code, output.String())
	}
	if !strings.Contains(output.String(), "--data") ||
		!strings.Contains(output.String(), "--run-id") {
		t.Fatalf("rejection not reported: %s", output.String())
	}
}

func TestCleanupDataRejectsMissingOrUnsupportedOwnershipMarker(
	t *testing.T,
) {
	for _, test := range []struct {
		name       string
		version    string
		versionErr error
	}{
		{name: "missing marker"},
		{name: "unsupported version", version: "99"},
		{name: "marker query failed", versionErr: errors.New("marker unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			log, err := NewRunLog(&output, "", Version)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			executor := &cleanupDatasetTestExecutor{
				version: test.version, versionErr: test.versionErr,
			}
			cfg := BenchConfig{Data: DataConfig{Schema: "Bench"}}

			if code := cleanupDataWithExecutor(
				context.Background(), executor, cfg, log,
			); code != 1 {
				t.Fatalf("exit code=%d output=%s", code, output.String())
			}
			if len(executor.statements) != 0 {
				t.Fatalf("unowned schema was dropped: %v", executor.statements)
			}
		})
	}
}

func TestCleanupDataAcceptsSupportedOwnershipVersions(t *testing.T) {
	for _, version := range []string{"1", "2", "3", "4"} {
		t.Run(version, func(t *testing.T) {
			log, err := NewRunLog(io.Discard, "", Version)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			executor := &cleanupDatasetTestExecutor{version: version}
			cfg := BenchConfig{Data: DataConfig{Schema: "Bench"}}

			if code := cleanupDataWithExecutor(
				context.Background(), executor, cfg, log,
			); code != 0 {
				t.Fatalf("version=%s exit code=%d", version, code)
			}
			want := []string{`DROP SCHEMA "Bench" CASCADE`}
			if !reflect.DeepEqual(executor.statements, want) {
				t.Fatalf("statements=%v want=%v", executor.statements, want)
			}
		})
	}
}

func TestCleanupAfterRestoreUsesProtectedLockSession(t *testing.T) {
	log, err := NewRunLog(io.Discard, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	lock := &protectedCleanupTestLock{version: datasetVersion}
	cfg := BenchConfig{Data: DataConfig{Schema: "Bench"}}

	if err := cleanupDataAfterRestore(
		context.Background(), lock, cfg, log,
	); err != nil {
		t.Fatal(err)
	}
	want := []string{`DROP SCHEMA "Bench" CASCADE`}
	if !reflect.DeepEqual(lock.statements, want) {
		t.Fatalf("protected statements=%v want=%v", lock.statements, want)
	}

	unprotected := &orderedRestoreTestLock{}
	if err := cleanupDataAfterRestore(
		context.Background(), unprotected, cfg, log,
	); err == nil {
		t.Fatal("cleanup accepted a restore lock without protected executor")
	}
}

func TestCleanupRejectsLiveThreePhasePlanWorkload(t *testing.T) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
		Safety:   SafetyConfig{QueryTimeout: time.Second},
	}
	identity := planActivityLockIdentity(cfg)
	state := &advisoryLockTestState{
		tryResults:   map[string]bool{identity: false},
		tryErrors:    map[string]error{},
		unlockResult: map[string]bool{},
		unlockErrors: map[string]error{},
	}
	backend := newAdvisoryLockTestBackend(t, state)
	backend.cfg = cfg
	backend.db.cfg = cfg

	err := ensureNoPlanWorkload(
		context.Background(), backend.db, cfg,
	)
	if err == nil || !strings.Contains(err.Error(), "plan workload") {
		t.Fatalf("error=%v", err)
	}
	if !reflect.DeepEqual(state.events, []string{"try " + identity}) {
		t.Fatalf("events=%v", state.events)
	}
}

func TestWithPlanRunPreparationDatabaseLockDoesNotReacquireHeldLock(
	t *testing.T,
) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	acquireCalls := 0
	workCalls := 0

	code, err := withPlanRunPreparationDatabaseLock(
		context.Background(),
		nil,
		cfg,
		true,
		func(context.Context, *Database, string) (func() error, error) {
			acquireCalls++
			return nil, errors.New("must not reacquire an already-held lock")
		},
		func() int {
			workCalls++
			return 0
		},
	)
	if err != nil || code != 0 {
		t.Fatalf("exit code=%d error=%v", code, err)
	}
	if acquireCalls != 0 || workCalls != 1 {
		t.Fatalf("acquire calls=%d work calls=%d", acquireCalls, workCalls)
	}
}

func TestWithPlanRunPreparationDatabaseLockProtectsStaleRestore(
	t *testing.T,
) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	var events []string

	code, err := withPlanRunPreparationDatabaseLock(
		context.Background(),
		nil,
		cfg,
		false,
		func(_ context.Context, _ *Database, identity string) (func() error, error) {
			events = append(events, "acquire "+identity)
			return func() error {
				events = append(events, "release")
				return nil
			}, nil
		},
		func() int {
			events = append(events, "stale restore and start run")
			return 0
		},
	)
	if err != nil || code != 0 {
		t.Fatalf("exit code=%d error=%v", code, err)
	}
	want := []string{
		"acquire gsbench:plan:postgres:Bench",
		"stale restore and start run",
		"release",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestPoolOnlyRecoveryFailureContinuationPolicy(t *testing.T) {
	errStop := errors.New("stop failed")
	tests := []struct {
		name    string
		summary RestoreSummary
		want    bool
	}{
		{
			name: "401 and 402 without actions",
			summary: RestoreSummary{
				Failed:              true,
				Err:                 errStop,
				DiscoveryComplete:   true,
				RestoreLockReleased: true,
				Runs: []RestoreRun{
					{RunID: "a", ScenarioCodes: []ScenarioCode{401}},
					{RunID: "b", ScenarioCodes: []ScenarioCode{402}},
				},
			},
			want: true,
		},
		{
			name: "success is not an exception",
			summary: RestoreSummary{Runs: []RestoreRun{{
				RunID: "a", ScenarioCodes: []ScenarioCode{401},
			}}},
		},
		{
			name:    "no discovered runs",
			summary: RestoreSummary{Failed: true, Err: errStop},
		},
		{
			name: "discovery failure",
			summary: RestoreSummary{
				Failed: true, Err: errStop, RestoreLockReleased: true,
				Runs: []RestoreRun{{
					RunID: "a", ScenarioCodes: []ScenarioCode{401},
				}},
			},
		},
		{
			name: "restore lock release failure",
			summary: RestoreSummary{
				Failed: true, Err: errStop, DiscoveryComplete: true,
				Runs: []RestoreRun{{
					RunID: "a", ScenarioCodes: []ScenarioCode{401},
				}},
			},
		},
		{
			name: "new run startup failure",
			summary: RestoreSummary{
				Failed: true, Err: errStop,
				DiscoveryComplete:     true,
				RestoreLockReleased:   true,
				AfterSuccessAttempted: true,
				Runs: []RestoreRun{{
					RunID: "a", ScenarioCodes: []ScenarioCode{401},
				}},
			},
		},
		{
			name: "unknown run identity",
			summary: RestoreSummary{
				Failed: true, Err: errStop,
				DiscoveryComplete:   true,
				RestoreLockReleased: true,
				Runs:                []RestoreRun{{RunID: "a"}},
			},
		},
		{
			name: "non-pool scenario",
			summary: RestoreSummary{
				Failed: true, Err: errStop,
				DiscoveryComplete:   true,
				RestoreLockReleased: true,
				Runs: []RestoreRun{{
					RunID: "a", ScenarioCodes: []ScenarioCode{401, 301},
				}},
			},
		},
		{
			name: "database or local action",
			summary: RestoreSummary{
				Failed: true, Err: errStop,
				DiscoveryComplete:   true,
				RestoreLockReleased: true,
				Runs: []RestoreRun{{
					RunID: "a", ScenarioCodes: []ScenarioCode{402},
				}},
				PlannedActions: []Action{{RunID: "a", ScenarioCode: 402}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canContinueAfterPoolOnlyRecoveryFailure(
				test.summary,
			); got != test.want {
				t.Fatalf(
					"got=%v want=%v summary=%+v",
					got,
					test.want,
					test.summary,
				)
			}
		})
	}
}

func TestContinueAfterPoolOnlyRecoveryFailureStartsNewRun(t *testing.T) {
	var output bytes.Buffer
	log, err := NewRunLog(&output, "", Version)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	starts := 0
	summary := RestoreSummary{
		Failed:              true,
		Err:                 errors.New("stop stale sessions failed"),
		DiscoveryComplete:   true,
		RestoreLockReleased: true,
		Runs: []RestoreRun{{
			RunID: "old", ScenarioCodes: []ScenarioCode{401},
		}},
	}
	if err := continueAfterPoolOnlyRecoveryFailure(
		summary,
		log,
		func() error {
			starts++
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if starts != 1 || !strings.Contains(output.String(), "WARN") ||
		!strings.Contains(output.String(), "stop stale sessions failed") {
		t.Fatalf("starts=%d output=%s", starts, output.String())
	}
}

func TestWithPlanRunPreparationDatabaseLockFailsBusyBeforeStaleRestore(
	t *testing.T,
) {
	cfg := BenchConfig{
		Database: DatabaseConfig{Database: "postgres"},
		Data:     DataConfig{Schema: "Bench"},
	}
	wantErr := errors.New("plan lock busy")
	staleRestoreCalls := 0

	_, err := withPlanRunPreparationDatabaseLock(
		context.Background(),
		nil,
		cfg,
		false,
		func(context.Context, *Database, string) (func() error, error) {
			return nil, wantErr
		},
		func() int {
			staleRestoreCalls++
			return 0
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
	if staleRestoreCalls != 0 {
		t.Fatalf("stale restore calls=%d want=0", staleRestoreCalls)
	}
}

func TestWithPlanScenarioDatabaseLockSkipsUnrelatedScenarios(t *testing.T) {
	cfg := BenchConfig{Run: RunConfig{ScenarioCodes: []ScenarioCode{101, 625}}}
	acquireCalls := 0
	workCalls := 0

	code, err := withPlanScenarioDatabaseLock(
		context.Background(),
		nil,
		cfg,
		func(context.Context, *Database, string) (func() error, error) {
			acquireCalls++
			return nil, errors.New("must not acquire")
		},
		func() int {
			workCalls++
			return 0
		},
	)
	if err != nil || code != 0 {
		t.Fatalf("exit code=%d error=%v", code, err)
	}
	if acquireCalls != 0 || workCalls != 1 {
		t.Fatalf("acquire calls=%d work calls=%d", acquireCalls, workCalls)
	}
}

func TestDoctorEnvironmentReportIncludesTopologyCapabilitiesAndDecisions(t *testing.T) {
	env := Environment{
		Product: ProductGaussDB, Version: "GaussDB Kernel V500", Topology: TopologyCentralized,
		Supported:    true,
		Nodes:        []Node{{Name: "cn_1", Role: NodeRoleCN, Host: "127.0.0.1", Port: 5432}},
		Capabilities: CapabilitySet{CapabilityStatementHistory: true},
	}
	report := strings.Join(doctorEnvironmentReport(env, []ScenarioDefinition{
		{Code: 101, Name: "cpu", AppliesTo: []EnvironmentClass{EnvironmentCentralizedGaussDB}},
		{Code: 405, Name: "pooler", AppliesTo: []EnvironmentClass{EnvironmentDistributedGaussDB}},
		{Code: 621, Name: "hard_parse", AppliesTo: []EnvironmentClass{EnvironmentCentralizedGaussDB}, Requires: []Requirement{RequirementHardParseCounters}},
	}), "\n")
	for _, want := range []string{
		"product=GaussDB version=GaussDB Kernel V500 topology=centralized_gaussdb",
		"nodes=1",
		"node=cn_1 role=CN shard= host=127.0.0.1 port=5432",
		"capability=statement_history supported=true",
		"scenario=101 name=cpu decision=SUPPORTED",
		"scenario=405 name=pooler decision=NOT_APPLICABLE",
		"scenario=621 name=hard_parse decision=UNSUPPORTED",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestDoctorUsesDeclaredFallbackMetadata(t *testing.T) {
	env := Environment{
		Product: ProductOpenGauss, Topology: TopologyStandalone,
		Capabilities: make(CapabilitySet), Supported: true,
	}
	withFallback := ScenarioDefinition{
		Code:      621,
		Name:      "declared_fallback",
		Category:  CategoryPlan,
		Risk:      RiskA,
		AppliesTo: []EnvironmentClass{EnvironmentOpenGauss},
		Requires:  []Requirement{RequirementHardParseCounters},
		FallbackRequirements: []Requirement{
			RequirementHardParseCounters,
		},
	}
	withoutFallback := withFallback
	withoutFallback.Name = "no_fallback"
	withoutFallback.FallbackRequirements = nil

	decision, _ := doctorScenarioDecision(env, withFallback)
	if decision != "DEGRADED" {
		t.Fatalf("declared fallback decision=%s", decision)
	}
	decision, _ = doctorScenarioDecision(env, withoutFallback)
	if decision != "UNSUPPORTED" {
		t.Fatalf("undeclared fallback decision=%s", decision)
	}
}
