package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

type resourceExecTestConnector struct {
	state *resourceExecTestState
}

func (c *resourceExecTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &resourceExecTestConn{state: c.state}, nil
}

func (*resourceExecTestConnector) Driver() driver.Driver {
	return resourceExecTestDriver{}
}

type resourceExecTestDriver struct{}

func (resourceExecTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type resourceExecTestState struct {
	statements []string
	failPrefix string
	failures   map[string]error
}

type resourceExecTestConn struct {
	state *resourceExecTestState
}

func (*resourceExecTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*resourceExecTestConn) Close() error { return nil }

func (*resourceExecTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

var errResourceExecTest = errors.New("resource SQL failed")

func (c *resourceExecTestConn) ExecContext(
	_ context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	c.state.statements = append(c.state.statements, statement)
	if err := c.state.failures[statement]; err != nil {
		return nil, err
	}
	if c.state.failPrefix != "" && strings.HasPrefix(statement, c.state.failPrefix) {
		return nil, errResourceExecTest
	}
	return driver.RowsAffected(1), nil
}

func openResourceExecTestConn(
	t *testing.T,
	state *resourceExecTestState,
) *sql.Conn {
	t.Helper()
	pool := sql.OpenDB(&resourceExecTestConnector{state: state})
	t.Cleanup(func() { _ = pool.Close() })
	conn, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func statementPrefixCount(statements []string, prefix string) int {
	count := 0
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			count++
		}
	}
	return count
}

func TestResourceWorkloadSQLUsesQuotedAllowlistedSchema(t *testing.T) {
	workload, err := ResourceWorkloadFor(201, "select", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(workload.Statement, `"select".fact_sales`) {
		t.Fatalf("sort workload did not quote its schema: %s", workload.Statement)
	}
	if _, err := ResourceWorkloadFor(201, "unsafe-name", centralizedFixture()); err == nil {
		t.Fatal("unsafe schema was accepted")
	}
}

func TestParameterizedDiscardQueriesCastBinaryIntegerResultsToText(t *testing.T) {
	tests := []struct {
		code ScenarioCode
		want string
	}{
		{
			code: 201,
			want: "SELECT CAST(id AS text) AS id_text,CAST(customer_id AS text) AS customer_id_text,amount,payload " +
				"FROM \"gsbench\".fact_sales WHERE id BETWEEN $1 AND $2 ORDER BY payload,amount DESC,id",
		},
		{
			code: 203,
			want: "SELECT sum(amount),avg(quantity),CAST(count(payload) AS text) AS payload_count_text " +
				"FROM \"gsbench\".fact_sales WHERE id BETWEEN $1 AND $2",
		},
		{
			code: 301,
			want: "SELECT sum(amount),avg(quantity),CAST(count(payload) AS text) AS payload_count_text " +
				"FROM \"gsbench\".fact_sales WHERE id BETWEEN $1 AND $2",
		},
		{
			code: 321,
			want: "SELECT CAST(id AS text) AS id_text,CAST(customer_id AS text) AS customer_id_text," +
				"CAST(product_id AS text) AS product_id_text,CAST(store_id AS text) AS store_id_text,amount,payload " +
				"FROM \"gsbench\".fact_sales WHERE id BETWEEN $1 AND $2",
		},
	}
	for _, test := range tests {
		workload, err := ResourceWorkloadFor(test.code, "gsbench", centralizedFixture())
		if err != nil {
			t.Fatalf("workload %d: %v", test.code, err)
		}
		if workload.Statement != test.want {
			t.Errorf("workload %d statement:\n got: %s\nwant: %s", test.code, workload.Statement, test.want)
		}
	}
}

func TestDistributedStreamScenariosRequireTheirNamedStreamEvidence(t *testing.T) {
	missing := verifyResourceWorkload(332, WorkerSnapshot{Operations: 1}, resourceEvidence{})
	if missing.Outcome == OutcomeSuccess {
		t.Fatalf("shuffle succeeded without a REDISTRIBUTE plan or node evidence: %+v", missing)
	}
	verified := verifyResourceWorkload(332, WorkerSnapshot{Operations: 1}, resourceEvidence{
		plan: "Streaming(type: REDISTRIBUTE)", node: "dn_1", streamBytes: 1024,
	})
	if verified.Outcome != OutcomeSuccess {
		t.Fatalf("shuffle with direct stream evidence failed: %+v", verified)
	}
}

func TestObserveRequiredStreamSupportsMultiColumnExplainRows(t *testing.T) {
	pool := sql.OpenDB(&explainRowsTestConnector{rows: &explainRowsTestRows{
		columns: []string{"id", "operation", "detail"},
		values:  [][]driver.Value{{int64(1), "Streaming", "type: REDISTRIBUTE"}},
	}})
	t.Cleanup(func() { _ = pool.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	scenario := &resourceScenario{workload: ResourceWorkload{Statement: "SELECT 1", RequiredStream: "REDISTRIBUTE"}}
	err := scenario.observeRequiredStream(ctx, &Runtime{
		Database:    &Database{pool: pool, ctx: ctx, cancel: cancel},
		Environment: Environment{Nodes: []Node{{Name: "dn_1", Role: NodeRoleDNPrimary}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scenario.evidence.plan, "Streaming\ttype: REDISTRIBUTE") {
		t.Fatalf("multi-column plan was not retained: %q", scenario.evidence.plan)
	}
}

func TestResourcePrepareKeepsMissingStreamAdvisory(t *testing.T) {
	pool := sql.OpenDB(&explainRowsTestConnector{rows: &explainRowsTestRows{
		columns: []string{"QUERY PLAN"},
		values:  [][]driver.Value{{"Seq Scan on fact_sales"}},
	}})
	t.Cleanup(func() { _ = pool.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	database := &Database{
		pool: pool, ctx: ctx, cancel: cancel,
		tagged: map[*TaggedConn]struct{}{},
	}
	var warnings []PrecheckWarning
	scenario := newResourceScenario(332, "network_distributed_shuffle")
	if err := scenario.Prepare(ctx, &Runtime{
		Config:   BenchConfig{Data: DataConfig{Schema: "gsbench"}},
		Database: database,
		Environment: Environment{
			Product: ProductOpenGauss, Topology: TopologyDistributed,
			Nodes: []Node{{Name: "dn_1", Role: NodeRoleDNPrimary}},
		},
		ReportWarning: func(warning PrecheckWarning) {
			warnings = append(warnings, warning)
		},
	}); err != nil {
		t.Fatalf("missing stream evidence blocked workload: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Check != "required_stream" {
		t.Fatalf("warnings=%+v", warnings)
	}
}

func TestResourceFactoriesBuildApprovedCodesAndKeepDeferredCodesAbsent(t *testing.T) {
	factories := ResourceScenarioFactories()
	for _, code := range []ScenarioCode{
		201, 202, 203, 204, 205, 207, 208,
		301, 302, 303, 304, 321, 322, 331, 332, 333,
		403, 404,
	} {
		factory := factories[code]
		if factory == nil {
			t.Fatalf("missing resource factory %d", code)
		}
		scenario, err := factory(DefaultScenarioCatalog().MustCode(code), distributedFixture())
		if err != nil {
			t.Fatalf("factory %d: %v", code, err)
		}
		if scenario.Code() != code {
			t.Fatalf("factory %d built code %d", code, scenario.Code())
		}
	}
	for _, code := range []ScenarioCode{206, 209, 305, 341, 342, 343, 405} {
		if factories[code] != nil {
			t.Fatalf("deferred resource factory %d was registered", code)
		}
	}
}

func TestComplexPoolerScenarioStaysCataloguedButUnregistered(t *testing.T) {
	definition := DefaultScenarioCatalog().MustCode(405)
	if len(definition.AppliesTo) != 1 || definition.AppliesTo[0] != EnvironmentDistributedGaussDB {
		t.Fatalf("405 applicability=%v", definition.AppliesTo)
	}
	if len(definition.Requires) != 1 || definition.Requires[0] != RequirementPoolerViews {
		t.Fatalf("405 requirements=%v", definition.Requires)
	}
	if ResourceScenarioFactories()[405] != nil {
		t.Fatal("complex pooler scenario is registered in resource factories")
	}
	if DefaultScenarioFactories()[405] != nil {
		t.Fatal("complex pooler scenario is registered in default factories")
	}
}

func TestPlanCachePrepareDeclaresEveryBoundParameter(t *testing.T) {
	statement, err := ResourceWorkloadFor(204, "gsbench", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	prepared := resourcePrepareStatement("run-1", 7, statement.Statement)
	if !strings.Contains(prepared, "(bigint,bigint)") {
		t.Fatalf("prepared statement does not declare its two bind parameters: %s", prepared)
	}
}

func TestPlanCacheGrowthBoundsPreparedPlansAndHoldsOwnedSession(t *testing.T) {
	workload, err := ResourceWorkloadFor(204, "gsbench", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	scenario := &resourceScenario{code: 204, workload: workload}
	op := scenario.operation(&Runtime{RunID: "run-1"})
	for range 66 {
		if err := op(context.Background(), conn, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := statementPrefixCount(state.statements, "PREPARE "); got != 64 {
		t.Fatalf("prepared plans=%d want=64", got)
	}
	if got := statementPrefixCount(state.statements, "EXECUTE "); got != 2 {
		t.Fatalf("post-cap plan executions=%d want=2", got)
	}
	for _, statement := range state.statements[64:] {
		if statement != "EXECUTE gsbench_pc_run1_1(0,0)" {
			t.Fatalf("post-cap plan SQL=%q want stable zero-row execution", statement)
		}
	}
}

func TestPlanCacheGrowthReturnsBoundedPlanExecutionError(t *testing.T) {
	workload, err := ResourceWorkloadFor(204, "gsbench", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	scenario := &resourceScenario{code: 204, workload: workload}
	op := scenario.operation(&Runtime{RunID: "run-1"})
	for range 64 {
		if err := op(context.Background(), conn, 0); err != nil {
			t.Fatal(err)
		}
	}
	state.failPrefix = "EXECUTE "
	if err := op(context.Background(), conn, 0); !errors.Is(err, errResourceExecTest) {
		t.Fatalf("bounded plan execution error=%v want=%v", err, errResourceExecTest)
	}
}

func TestSessionContextGrowthBoundsCursorsAndHoldsOwnedSession(t *testing.T) {
	workload, err := ResourceWorkloadFor(205, "gsbench", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	scenario := &resourceScenario{code: 205, workload: workload}
	op := scenario.operation(&Runtime{RunID: "run-1"})
	for range 258 {
		if err := op(context.Background(), conn, 0); err != nil {
			t.Fatal(err)
		}
	}
	if got := statementPrefixCount(state.statements, "BEGIN"); got != 1 {
		t.Fatalf("transactions begun=%d want=1", got)
	}
	if got := statementPrefixCount(state.statements, "DECLARE "); got != 256 {
		t.Fatalf("declared cursors=%d want=256", got)
	}
	if got := statementPrefixCount(state.statements, "FETCH 1 FROM "); got != 256 {
		t.Fatalf("cursor fetches=%d want=256", got)
	}
	wantTail := []string{"SELECT 1", "SELECT 1"}
	if got := state.statements[len(state.statements)-2:]; !equalStringSlices(got, wantTail) {
		t.Fatalf("post-cap cursor keepalive=%v want=%v", got, wantTail)
	}
}

func TestSessionContextGrowthReturnsPostCapKeepaliveError(t *testing.T) {
	workload, err := ResourceWorkloadFor(205, "gsbench", centralizedFixture())
	if err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{}
	conn := openResourceExecTestConn(t, state)
	scenario := &resourceScenario{code: 205, workload: workload}
	op := scenario.operation(&Runtime{RunID: "run-1"})
	for range 256 {
		if err := op(context.Background(), conn, 0); err != nil {
			t.Fatal(err)
		}
	}
	state.failPrefix = "SELECT 1"
	if err := op(context.Background(), conn, 0); !errors.Is(err, errResourceExecTest) {
		t.Fatalf("post-cap keepalive error=%v want=%v", err, errResourceExecTest)
	}
}

func TestApprovedResourceWorkloadsHaveBoundedSQLAndOwnedSessionCleanup(t *testing.T) {
	for _, code := range []ScenarioCode{201, 202, 203, 204, 205, 207, 208, 301, 302, 303, 304, 321, 322, 331, 332, 333, 403, 404} {
		workload, err := ResourceWorkloadFor(code, "gsbench", distributedFixture())
		if err != nil || workload.Statement == "" {
			t.Fatalf("workload %03d: %+v err=%v", code, workload, err)
		}
	}
	spill, _ := ResourceWorkloadFor(304, "gsbench", centralizedFixture())
	if spill.Setup != "SET work_mem='64kB'" || spill.Cleanup != "RESET work_mem" {
		t.Fatalf("spill session ownership=%+v", spill)
	}
	ingress, _ := ResourceWorkloadFor(322, "gsbench", centralizedFixture())
	if !strings.Contains(ingress.Statement, "network_ingress(run_id,dist_key,seq,payload)") {
		t.Fatalf("ingress is not a client parameterized insert: %s", ingress.Statement)
	}
	planCache, _ := ResourceWorkloadFor(204, "gsbench", centralizedFixture())
	if planCache.Cleanup != "DEALLOCATE ALL" {
		t.Fatalf("plan-cache cleanup=%q want DEALLOCATE ALL", planCache.Cleanup)
	}
}

func TestResourceCleanupAttemptsRollbackAfterEarlierCleanupError(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newResourceScenario(205, "memory_session_context_growth")
	if err := scenario.Prepare(context.Background(), &Runtime{Config: cfg, Database: &Database{}}); err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{failPrefix: "CLOSE ALL"}
	conn := openResourceExecTestConn(t, state)
	cleanupErr := scenario.workers.cleanup(context.Background(), conn, 0)
	if !errors.Is(cleanupErr, errResourceExecTest) {
		t.Fatalf("cleanup error=%v want=%v", cleanupErr, errResourceExecTest)
	}
	if got := statementPrefixCount(state.statements, "ROLLBACK"); got != 1 {
		t.Fatalf("rollback attempts=%d want=1 after CLOSE ALL failure", got)
	}
}

func TestResourcePlanCacheCancellationRecognizesSingleBadCleanupConnection(
	t *testing.T,
) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newResourceScenario(204, "memory_plan_cache_growth")
	if err := scenario.Prepare(context.Background(), &Runtime{
		Config: cfg, Database: &Database{},
	}); err != nil {
		t.Fatal(err)
	}
	state := &resourceExecTestState{failures: map[string]error{
		"DEALLOCATE ALL": driver.ErrBadConn,
	}}
	conn := openResourceExecTestConn(t, state)
	cleanupErr := scenario.workers.cleanup(context.Background(), conn, 0)
	if cleanupErr != driver.ErrBadConn {
		t.Fatalf("single cleanup error identity=%T %v, want driver.ErrBadConn", cleanupErr, cleanupErr)
	}
	if got := normalizeCanceledWorkerConnectionError(cleanupErr, true); got != nil {
		t.Fatalf("cancellation cleanup error=%v, want nil", got)
	}
}

func TestResourceCleanupDoesNotHideBadConnectionJoinedWithRealFailure(
	t *testing.T,
) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newResourceScenario(207, "memory_total_pressure")
	if err := scenario.Prepare(context.Background(), &Runtime{
		Config: cfg, Database: &Database{},
	}); err != nil {
		t.Fatal(err)
	}
	realCleanupErr := errors.New("deallocate protocol failed")
	state := &resourceExecTestState{failures: map[string]error{
		"DEALLOCATE ALL": realCleanupErr,
		"CLOSE ALL":      driver.ErrBadConn,
	}}
	conn := openResourceExecTestConn(t, state)
	cleanupErr := scenario.workers.cleanup(context.Background(), conn, 0)
	if !errors.Is(cleanupErr, driver.ErrBadConn) ||
		!errors.Is(cleanupErr, sql.ErrConnDone) ||
		!errors.Is(cleanupErr, realCleanupErr) {
		t.Fatalf("multiple cleanup errors=%v", cleanupErr)
	}
	if got := normalizeCanceledWorkerConnectionError(cleanupErr, true); got == nil {
		t.Fatal("cancellation hid a real cleanup error joined with driver.ErrBadConn")
	}
}

func TestTotalMemoryScenarioUsesBoundedWorkerStrategy(t *testing.T) {
	scenario := newTotalMemoryScenario("memory_total_pressure")
	if got := scenario.Strategy(); got != "memory_pressure_workers" {
		t.Fatalf("strategy=%q want memory_pressure_workers", got)
	}
}

func TestTotalMemoryScenarioUsesConfiguredWorkerBudgetWithoutLegacyCap(t *testing.T) {
	defaults, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	defaultScenario := newTotalMemoryScenario("memory_total_pressure")
	if err := defaultScenario.Prepare(context.Background(), &Runtime{Config: defaults, Database: &Database{}}); err != nil {
		t.Fatal(err)
	}
	if defaultScenario.target != 4 {
		t.Fatalf("default memory workers=%d want=4", defaultScenario.target)
	}
	if err := defaultScenario.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	configured, err := LoadConfig(writeTestConfig(t, minimalConfig()+`
[scenario.memory_total_pressure]
workers = 3

[safety]
max_workers = 4
`), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newTotalMemoryScenario("memory_total_pressure")
	if err := scenario.Prepare(context.Background(), &Runtime{Config: configured, Database: &Database{}}); err != nil {
		t.Fatal(err)
	}
	if scenario.target != 3 {
		t.Fatalf("configured memory workers=%d want=3", scenario.target)
	}
	if err := scenario.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	unsafe, err := LoadConfig(writeTestConfig(t, minimalConfig()+`
[scenario.memory_total_pressure]
workers = 3

[safety]
max_workers = 2
`), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	uncapped := newTotalMemoryScenario("memory_total_pressure")
	if err := uncapped.Prepare(context.Background(), &Runtime{Config: unsafe, Database: &Database{}}); err != nil {
		t.Fatalf("legacy safety maximum blocked target: %v", err)
	}
	if uncapped.target != 3 {
		t.Fatalf("uncapped memory workers=%d want=3", uncapped.target)
	}
	if err := uncapped.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestTotalMemoryScenarioUsesCompositeWorkMemBudget(t *testing.T) {
	cfg, err := LoadConfig(writeTestConfig(t, minimalConfig()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	scenario := newTotalMemoryScenario("memory_total_pressure")
	if err := scenario.Prepare(context.Background(), &Runtime{Config: cfg, Database: &Database{}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scenario.Stop(context.Background(), nil) })
	for _, child := range scenario.composite.scenarios {
		if child.code != 201 && child.code != 202 {
			continue
		}
		if child.workload.Setup != "SET work_mem='64MB'" {
			t.Fatalf("composite child %d setup=%q want 64MB work_mem", child.code, child.workload.Setup)
		}
	}
}

func TestTotalMemoryScenarioHoldReturnsWorkerErrorsImmediately(t *testing.T) {
	group := &WorkerGroup{}
	group.errors.Store(1)
	group.firstError = "memory worker failed"
	scenario := &totalMemoryScenario{composite: memoryComposite{scenarios: []*resourceScenario{{workers: &sqlWorkload{group: group}}}}}
	started := time.Now()
	err := scenario.Hold(context.Background(), &Runtime{Config: BenchConfig{Run: RunConfig{Duration: 100 * time.Millisecond}}})
	if err == nil || !strings.Contains(err.Error(), "memory worker failed") {
		t.Fatalf("hold error=%v", err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("hold waited %s after a known worker error", elapsed)
	}
}
