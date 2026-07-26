package gsbench

import (
	"context"
	"strings"
	"testing"
)

type fixedPlanExplainer struct{ plan string }

func (e fixedPlanExplainer) Explain(context.Context, string) (string, error) { return e.plan, nil }

func TestPlanBaselinePlanVerificationRequiresExpectedToken(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPlanBaselinePlans(context.Background(), fixedPlanExplainer{plan: "Seq Scan (cost=0.00..1.00)"}, definitions[:1]); err != nil {
		t.Fatal(err)
	}
	if err := verifyPlanBaselinePlans(context.Background(), fixedPlanExplainer{plan: "Seq Scan (cost=0.00..1.00)"}, definitions[1:2]); err == nil || !strings.Contains(err.Error(), "plan_index_unusable_idx") {
		t.Fatalf("expected missing baseline index token, got %v", err)
	}
}

func TestPlanBaselineRepairSQLIsScopedAndComplete(t *testing.T) {
	steps, err := PlanBaselineRepairSteps("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(steps, "\n")
	for _, token := range []string{
		"gsbench.plan_data", "plan_index_unusable_idx", "plan_index_drop_idx",
		"plan_index_shape_good_idx", "plan_index_shape_bad_idx",
		"SET STATISTICS -1", "RESET (n_distinct)", "ADD STATISTICS",
		"ANALYZE gsbench.plan_data",
	} {
		if !strings.Contains(joined, token) {
			t.Errorf("repair SQL missing %s", token)
		}
	}
	if strings.Contains(joined, "pg_catalog.pg_statistic SET") ||
		strings.Contains(joined, "pg_index SET") {
		t.Fatalf("repair directly updates catalogs: %s", joined)
	}
}

func TestPlanBaselineRepairRejectsUnsafeSchema(t *testing.T) {
	if _, err := PlanBaselineRepairSteps("gsbench;drop schema public"); err == nil {
		t.Fatal("expected unsafe schema error")
	}
}
