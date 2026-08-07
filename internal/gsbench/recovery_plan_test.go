package gsbench

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func recoverySQLAction(runID string, sequence int64, code ScenarioCode, target, inverse string) Action {
	return Action{
		RunID: runID, Sequence: sequence, ScenarioCode: code,
		Kind: ActionSQLMutation, TargetProduct: ProductOpenGauss,
		Target: target, Inverse: json.RawMessage(`{"sql":` + quotedJSON(inverse) + `}`),
		State: MutationApplied,
	}
}

func quotedJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestRecoveryPlanRendersReadOnlySQLAndSessionOrder(t *testing.T) {
	action := recoverySQLAction(
		"run-1", 2, 601, "gsbench.plan_data_lookup_idx",
		"CREATE INDEX plan_data_lookup_idx ON gsbench.plan_data(lookup_key)",
	)
	session := recoverySQLAction("run-1", 1, 602, "gsbench.plan_data", "SELECT 1")
	session.Inverse = json.RawMessage(`{"session_sql":["SET work_mem='64MB'","ANALYZE gsbench.plan_data"]}`)

	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{action, session}},
		RecoveryPlanFilter{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("items=%+v", plan.Items)
	}
	got := append([]string(nil), plan.Items[0].Statements...)
	got = append(got, plan.Items[1].Statements...)
	want := []string{
		"CREATE INDEX plan_data_lookup_idx ON gsbench.plan_data(lookup_key);",
		"SET work_mem='64MB';",
		"ANALYZE gsbench.plan_data;",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("statements=%q want=%q items=%+v", got, want, plan.Items)
	}
	for _, item := range plan.Items {
		if item.State != RecoveryUnverified {
			t.Fatalf("item=%+v", item)
		}
	}
}

func TestRecoveryPlanVerifierSuppressesSatisfiedInverse(t *testing.T) {
	action := recoverySQLAction("run-1", 1, 601, "index", "DROP INDEX index")
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{action}},
		RecoveryPlanFilter{},
		func(context.Context, Action) (bool, error) { return true, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].State != RecoveryAlreadyRestored ||
		len(plan.Items[0].Statements) != 0 {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestRecoveryPlanFiltersScenarioAndRendersExternalManualAction(t *testing.T) {
	external := Action{
		RunID: "run-2", Sequence: 1, ScenarioCode: 706,
		Kind: ActionNetworkQDisc, TargetProduct: ProductOpenGauss,
		Target: "eth0", Node: "dn-1",
		Inverse: json.RawMessage(`{"operation":"delete"}`),
		State:   MutationApplied,
	}
	other := recoverySQLAction("run-1", 1, 601, "index", "DROP INDEX index")
	code := ScenarioCode(706)
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{other}, LocalActions: []Action{external}},
		RecoveryPlanFilter{ScenarioCode: &code},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].ScenarioCode != 706 ||
		!strings.Contains(plan.Items[0].ManualAction, "NETWORK_QDISC") ||
		len(plan.Items[0].Statements) != 0 {
		t.Fatalf("plan=%+v", plan)
	}
}

func TestRecoveryPlanDeduplicatesAndKeepsDescendingInverseOrder(t *testing.T) {
	older := recoverySQLAction("run-1", 1, 601, "index-a", "CREATE INDEX index_a ON t(a)")
	newer := recoverySQLAction("run-1", 3, 601, "index-b", "DROP INDEX index_b")
	duplicate := older
	duplicate.Sequence = 2
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{older, newer, duplicate}},
		RecoveryPlanFilter{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 || plan.Items[0].Target != "index-b" ||
		plan.Items[1].Target != "index-a" {
		t.Fatalf("items=%+v", plan.Items)
	}
}

func TestRecoveryPlanSafelyScansLegacySQLAndRejectsCredentials(t *testing.T) {
	legacy := recoverySQLAction(
		"run-1", 1, 601, "plan_data",
		"ALTER TABLE gsbench.plan_data ALTER COLUMN x SET STATISTICS -1; ANALYZE gsbench.plan_data(x)",
	)
	legacy.LegacySQL = true
	unsafe := recoverySQLAction(
		"run-2", 1, 601, "role", "ALTER USER gaussdb PASSWORD 'secret-value'",
	)
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{legacy, unsafe}},
		RecoveryPlanFilter{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("plan=%+v", plan)
	}
	items := make(map[string]RecoveryPlanItem, len(plan.Items))
	for _, item := range plan.Items {
		items[item.Target] = item
	}
	if len(items["plan_data"].Statements) != 2 {
		t.Fatalf("legacy item=%+v", items["plan_data"])
	}
	if len(items["role"].Statements) != 0 ||
		!strings.Contains(items["role"].Detail, "credential") {
		t.Fatalf("unsafe item=%+v", items["role"])
	}
}

func TestRecoveryPlanBaselineFilteringAndJournalPrecedence(t *testing.T) {
	findings := []PlanBaselineFinding{
		{
			ScenarioCodes: []ScenarioCode{605},
			Check:         "index_definition", Target: "plan_index_drop_idx",
			Statements: []string{"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data(index_drop_key)"},
		},
		{
			ScenarioCodes: []ScenarioCode{606},
			Check:         "unexpected_index", Target: "plan_index_shape_bad_idx",
			Statements: []string{"DROP INDEX plan_index_shape_bad_idx"},
		},
	}
	code := ScenarioCode(605)
	filtered := MergePlanBaselineFindings(
		RecoveryPlan{}, findings, RecoveryPlanFilter{ScenarioCode: &code},
	)
	if len(filtered.Items) != 1 || filtered.Items[0].ScenarioCode != 605 {
		t.Fatalf("filtered=%+v", filtered)
	}

	journal := RecoveryPlan{Items: []RecoveryPlanItem{{
		RunID: "run-1", ScenarioCode: 605, Kind: ActionSQLMutation,
		Target: "plan_index_drop_idx", State: RecoveryPending,
		Statements: []string{"CREATE INDEX original_shape ON gsbench.plan_data(index_drop_key);"},
	}}}
	merged := MergePlanBaselineFindings(journal, findings, RecoveryPlanFilter{})
	if len(merged.Items) != 2 || merged.Items[0].RunID != "run-1" {
		t.Fatalf("journal action did not outrank baseline: %+v", merged)
	}
}

func TestRecoveryPlanKeepsConflictsVisibleAndOrdersAnalyzeLast(t *testing.T) {
	create := recoverySQLAction(
		"run-1", 1, 605, "plan_index_drop_idx",
		"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data(index_drop_key)",
	)
	analyze := recoverySQLAction(
		"run-1", 9, 605, "plan_data analyze",
		"ANALYZE gsbench.plan_data",
	)
	conflict := create
	conflict.Target = "different-target-for-same-journal-id"
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{analyze, create, conflict}},
		RecoveryPlanFilter{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) < 3 || plan.Items[0].State != RecoveryConflict {
		t.Fatalf("conflict not visible: %+v", plan.Items)
	}
	var createAt, analyzeAt = -1, -1
	for index, item := range plan.Items {
		if item.Target == "plan_index_drop_idx" {
			createAt = index
		}
		if item.Target == "plan_data analyze" {
			analyzeAt = index
		}
	}
	if createAt < 0 || analyzeAt <= createAt {
		t.Fatalf("items=%+v", plan.Items)
	}
	lines := RecoveryPlanLines(plan)
	if !strings.Contains(strings.Join(lines, "\n"), "display_only=true") {
		t.Fatalf("lines=%q", lines)
	}
}
