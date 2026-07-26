package gsbench

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizePlanStructureIgnoresCostsButKeepsAccessPath(t *testing.T) {
	a := "Aggregate  (cost=10.00..10.01 rows=1 width=8)\n  ->  Index Scan using idx_a on plan_data  (cost=0.00..9.00 rows=5 width=8)"
	b := "Aggregate  (cost=900.00..901.00 rows=1 width=48)\n  ->  Index Scan using idx_a on plan_data  (cost=2.00..800.00 rows=999 width=16)"
	if NormalizePlanStructure(a) != NormalizePlanStructure(b) {
		t.Fatalf("cost-only change altered signature:\n%s\n%s", NormalizePlanStructure(a), NormalizePlanStructure(b))
	}
	seq := strings.Replace(b, "Index Scan using idx_a", "Seq Scan", 1)
	if NormalizePlanStructure(a) == NormalizePlanStructure(seq) {
		t.Fatal("access-path change was ignored")
	}
}

func TestEvaluatePlanChangeRequiresStructureResultAndSlowdown(t *testing.T) {
	base := PlanObservation{
		SQL: "SELECT 1", StructureSignature: "Index Scan idx",
		ResultFingerprint: "42:900", Median: 10 * time.Millisecond,
	}
	changed := PlanObservation{
		SQL: "SELECT 1", StructureSignature: "Seq Scan",
		ResultFingerprint: "42:900", Median: 25 * time.Millisecond,
	}
	result := EvaluatePlanChange("plan_index_unusable", base, changed, 2)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	changed.StructureSignature = base.StructureSignature
	if got := EvaluatePlanChange("plan_index_unusable", base, changed, 2).Outcome; got != OutcomeFailed {
		t.Fatalf("unchanged structure outcome=%s", got)
	}
}

func TestSelectChangedCandidateUsesFirstStructuralChange(t *testing.T) {
	firstPlan := "Index Scan using idx_a on plan_data (cost=1..2 rows=1 width=8)"
	secondPlan := "Index Scan using idx_b on plan_data (cost=1..2 rows=1 width=8)"
	baselines := []PlanObservation{
		{SQL: "SELECT 1", StructureSignature: NormalizePlanStructure(firstPlan)},
		{SQL: "SELECT 2", StructureSignature: NormalizePlanStructure(secondPlan)},
	}
	changedPlans := map[string]string{
		"SELECT 1": firstPlan,
		"SELECT 2": "Seq Scan on plan_data (cost=1..2 rows=1 width=8)",
	}
	baseline, changed, err := SelectChangedCandidate(baselines, changedPlans)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.SQL != "SELECT 2" || !strings.Contains(changed, "Seq Scan") {
		t.Fatalf("baseline=%+v changed=%q", baseline, changed)
	}
}
