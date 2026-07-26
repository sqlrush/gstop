package gsbench

import (
	"regexp"
	"testing"
	"time"
)

func TestPlanRegressionRequiresChangedPlanSameResultAndSlowdown(t *testing.T) {
	base := PlanObservation{StructureSignature: "Index Scan", ResultFingerprint: "42:900", Median: 10 * time.Millisecond}
	worse := PlanObservation{StructureSignature: "Seq Scan", ResultFingerprint: "42:900", Median: 25 * time.Millisecond}
	result := EvaluatePlanChange("plan_index_unusable", base, worse, 2.0)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	worse.StructureSignature = "Index Scan"
	if got := EvaluatePlanChange("plan_index_unusable", base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("unchanged plan outcome=%s", got)
	}
	worse.StructureSignature = "Seq Scan"
	worse.ResultFingerprint = "different"
	if got := EvaluatePlanChange("plan_index_unusable", base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("wrong-result outcome=%s", got)
	}
}

func TestLiteralPlanWorkerUsesExactSQLWithoutArguments(t *testing.T) {
	query := "SELECT count(*),sum(id) FROM gsbench.plan_data WHERE index_drop_key BETWEEN 100000 AND 110000"
	if regexp.MustCompile(`\$[0-9]+|\?`).MatchString(query) {
		t.Fatalf("query contains bind placeholder: %s", query)
	}
	if op := literalPlanOp(query); op == nil {
		t.Fatal("literal plan operation is nil")
	}
}

func TestPlanScenarioUsesItsCanonicalName(t *testing.T) {
	def := PlanScenarioDefinition{Name: "plan_index_drop", Candidates: []string{"SELECT 1"}}
	scenario := NewPlanChangeScenario(def, &PlanCoordinator{})
	if scenario.Name() != "plan_index_drop" {
		t.Fatalf("name=%s", scenario.Name())
	}
}
