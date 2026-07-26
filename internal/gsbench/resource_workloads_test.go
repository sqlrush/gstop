package gsbench

import (
	"strings"
	"testing"
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
