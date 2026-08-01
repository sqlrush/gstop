package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

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
}

func TestTotalMemoryScenarioUsesBoundedWorkerStrategy(t *testing.T) {
	scenario := newTotalMemoryScenario("memory_total_pressure")
	if got := scenario.Strategy(); got != "memory_pressure_workers" {
		t.Fatalf("strategy=%q want memory_pressure_workers", got)
	}
}

func TestTotalMemoryScenarioUsesConfiguredWorkerBudgetAndRejectsUnsafeTarget(t *testing.T) {
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
	if err := newTotalMemoryScenario("memory_total_pressure").Prepare(context.Background(), &Runtime{Config: unsafe, Database: &Database{}}); err == nil {
		t.Fatal("memory worker target exceeding the safety maximum was accepted")
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
