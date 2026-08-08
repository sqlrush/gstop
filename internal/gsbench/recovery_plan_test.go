package gsbench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type recoveryVerifierTestDatabase struct {
	actual        string
	readOnlyError error
	readOnlySQL   []string
	unsafeScans   int
}

func (*recoveryVerifierTestDatabase) Exec(
	context.Context, string, ...any,
) (sql.Result, error) {
	return nil, errors.New("unexpected recovery verifier mutation")
}

func (d *recoveryVerifierTestDatabase) Scan(
	context.Context, string, []any, ...any,
) error {
	d.unsafeScans++
	return errors.New("unsafe non-transactional verifier scan")
}

func (d *recoveryVerifierTestDatabase) ScanReadOnly(
	_ context.Context,
	query string,
	_ []any,
	dest ...any,
) error {
	d.readOnlySQL = append(d.readOnlySQL, query)
	if d.readOnlyError != nil {
		return d.readOnlyError
	}
	*(dest[0].(*string)) = d.actual
	return nil
}

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

func TestReconcile601RecoveryUsesOnlyLiveIndexState(t *testing.T) {
	source := RecoveryPlan{Items: []RecoveryPlanItem{
		{
			RunID: "old-601", ScenarioCode: 601, Kind: ActionSQLMutation,
			Target: `"gsbench".plan_data_lookup_idx`, State: RecoveryPending,
			Statements: []string{"recorded inverse;"},
		},
		{
			RunID: "other-603", ScenarioCode: 603, Kind: ActionSQLMutation,
			Target: "unrelated", State: RecoveryPending,
			Statements: []string{"ANALYZE unrelated;"},
		},
	}}

	restored, err := ReconcilePlanRecoveryWithLiveState(
		source,
		PlanFaultInspection{Code: 601, State: PlanFaultRestored},
		"gsbench",
	)
	if err != nil {
		t.Fatal(err)
	}
	item := recoveryItemForScenario(t, restored, 601)
	if item.RunID != "old-601" || item.State != RecoveryAlreadyRestored ||
		len(item.Statements) != 0 {
		t.Fatalf("restored item=%+v", item)
	}
	if recoveryItemForScenario(t, restored, 603).RunID != "other-603" {
		t.Fatalf("unrelated recovery was replaced: %+v", restored.Items)
	}

	present, err := ReconcilePlanRecoveryWithLiveState(
		source,
		PlanFaultInspection{Code: 601, State: PlanFaultPresent},
		"gsbench",
	)
	if err != nil {
		t.Fatal(err)
	}
	item = recoveryItemForScenario(t, present, 601)
	wantCreate := `CREATE UNIQUE INDEX plan_data_lookup_idx ON "gsbench".plan_data (lookup_key,dist_key);`
	if item.State != RecoveryPending ||
		!reflect.DeepEqual(item.Statements, []string{wantCreate}) {
		t.Fatalf("fault-present item=%+v", item)
	}

	drifted, err := ReconcilePlanRecoveryWithLiveState(
		source,
		PlanFaultInspection{Code: 601, State: PlanFaultDrifted},
		"gsbench",
	)
	if err != nil {
		t.Fatal(err)
	}
	item = recoveryItemForScenario(t, drifted, 601)
	want := []string{
		`DROP INDEX IF EXISTS "gsbench".plan_data_lookup_idx;`,
		wantCreate,
	}
	if item.State != RecoveryPending || !reflect.DeepEqual(item.Statements, want) {
		t.Fatalf("drifted item=%+v want=%q", item, want)
	}
}

func TestReconcile602RecoveryKeepsResetAndAnalyzeTogether(t *testing.T) {
	plan := RecoveryPlan{Items: []RecoveryPlanItem{
		{
			RunID: "old-602", ScenarioCode: 602, Kind: ActionSQLMutation,
			Target: `"gsbench".plan_data.lookup_key`, State: RecoveryPending,
			Statements: []string{
				`ALTER TABLE "gsbench".plan_data ALTER COLUMN lookup_key RESET (n_distinct);`,
			},
		},
		{
			RunID: "old-602", ScenarioCode: 602, Kind: ActionSQLMutation,
			Target: `"gsbench".plan_data.lookup_key analyze`,
			State:  RecoveryAlreadyRestored,
		},
	}}
	for _, state := range []PlanFaultLiveState{PlanFaultPresent, PlanFaultDrifted} {
		t.Run(string(state), func(t *testing.T) {
			got, err := ReconcilePlanRecoveryWithLiveState(
				plan,
				PlanFaultInspection{Code: 602, State: state},
				"gsbench",
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Items) != 1 {
				t.Fatalf("items=%+v", got.Items)
			}
			want := []string{
				`ALTER TABLE "gsbench".plan_data ALTER COLUMN lookup_key RESET (n_distinct);`,
				`ANALYZE "gsbench".plan_data(lookup_key);`,
			}
			if got.Items[0].RunID != "old-602" ||
				got.Items[0].State != RecoveryPending ||
				!reflect.DeepEqual(got.Items[0].Statements, want) {
				t.Fatalf("item=%+v want=%q", got.Items[0], want)
			}
		})
	}
}

func TestReconcilePlanRecoveryPreservesAuditPlanWhenLiveStateUnavailable(t *testing.T) {
	want := RecoveryPlan{Items: []RecoveryPlanItem{{
		RunID: "old-601", ScenarioCode: 601, Kind: ActionSQLMutation,
		Target: "recorded", State: RecoveryUnverified,
		Statements: []string{"recorded inverse;"},
	}}}
	got, err := ReconcilePlanRecoveryWithLiveState(
		want,
		PlanFaultInspection{Code: 601, State: PlanFaultUnavailable},
		"gsbench",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("plan=%+v want=%+v", got, want)
	}
}

func TestScenarioRecoverySurvivesUnavailablePlanAuditDiscovery(t *testing.T) {
	for _, code := range []ScenarioCode{601, 602} {
		inspection := PlanFaultInspection{Code: code, State: PlanFaultPresent}
		if !planRecoveryDiscoveryCanUseLiveState(code, inspection) {
			t.Fatalf("scenario %03d could not fall back to live state", code)
		}
	}
	if planRecoveryDiscoveryCanUseLiveState(
		603,
		PlanFaultInspection{Code: 603, State: PlanFaultUnavailable},
	) {
		t.Fatal("scenario 603 incorrectly ignored audit discovery failure")
	}
	if planRecoveryDiscoveryCanUseLiveState(
		601,
		PlanFaultInspection{Code: 601, State: PlanFaultUnavailable},
	) {
		t.Fatal("unavailable 601 live state incorrectly ignored audit discovery failure")
	}
}

func TestPlanRecoveryVerifierDefers601And602AuthorityToLiveCatalog(t *testing.T) {
	actions := []Action{
		recoverySQLAction(
			"old-601", 1, 601, `"gsbench".plan_data_lookup_idx`,
			`CREATE UNIQUE INDEX plan_data_lookup_idx ON "gsbench".plan_data (lookup_key,dist_key)`,
		),
		recoverySQLAction(
			"old-602", 1, 602, `"gsbench".plan_data.lookup_key`,
			`ALTER TABLE "gsbench".plan_data ALTER COLUMN lookup_key RESET (n_distinct)`,
		),
		recoverySQLAction(
			"old-602", 2, 602, `"gsbench".plan_data.lookup_key analyze`,
			`ANALYZE "gsbench".plan_data(lookup_key)`,
		),
	}
	baseCalls := 0
	verify := recoveryVerifierWithPlanLiveAuthority(
		func(context.Context, Action) (bool, error) {
			baseCalls++
			return true, nil
		},
	)
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: actions},
		RecoveryPlanFilter{},
		verify,
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseCalls != 0 {
		t.Fatalf("legacy verifier was called %d times", baseCalls)
	}
	if len(plan.Items) != 3 {
		t.Fatalf("items=%+v", plan.Items)
	}
	for _, item := range plan.Items {
		if item.State != RecoveryUnverified || len(item.Statements) != 1 {
			t.Fatalf("plan fault audit item hid recovery SQL: %+v", item)
		}
	}
}

func TestRecoveryDiscoveryContextErrorsNeverAllowFallback(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("wrapped cancellation: %w", context.Canceled),
	} {
		if recoveryDiscoveryErrorAllowsFallback(err) {
			t.Fatalf("context error allowed fallback: %v", err)
		}
	}
	if !recoveryDiscoveryErrorAllowsFallback(errors.New("audit table unavailable")) {
		t.Fatal("ordinary audit discovery failure could not use live fallback")
	}
}

func TestRecoveryBaselineContextErrorsNeverBecomeAdvisoryFindings(t *testing.T) {
	for _, err := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("wrapped deadline: %w", context.DeadlineExceeded),
	} {
		if recoveryBaselineErrorAllowsFinding(err) {
			t.Fatalf("context error became a baseline finding: %v", err)
		}
	}
	if !recoveryBaselineErrorAllowsFinding(errors.New("catalog probe denied")) {
		t.Fatal("ordinary baseline probe failure could not become an advisory finding")
	}
}

func recoveryItemForScenario(
	t *testing.T,
	plan RecoveryPlan,
	code ScenarioCode,
) RecoveryPlanItem {
	t.Helper()
	for _, item := range plan.Items {
		if item.ScenarioCode == code {
			return item
		}
	}
	t.Fatalf("scenario %03d item not found in %+v", code, plan.Items)
	return RecoveryPlanItem{}
}

func TestRecoveryActionVerifierUsesOnlyCanonicalReadOnlyProbe(t *testing.T) {
	mutations, err := PlanMutationSet(
		"run-1", "gsbench", "planchange_stats_target",
	)
	if err != nil {
		t.Fatal(err)
	}
	action := SQLAction(mutations[0])
	action.TargetProduct = ProductOpenGauss
	db := &recoveryVerifierTestDatabase{actual: "wrong index definition"}
	verify := newRecoveryActionVerifier(db, BenchConfig{
		Data: DataConfig{Schema: "gsbench"},
	})

	restored, err := verify(context.Background(), action)
	if err != nil || restored {
		t.Fatalf("canonical mismatch restored=%t err=%v", restored, err)
	}
	if len(db.readOnlySQL) != 1 || db.unsafeScans != 0 {
		t.Fatalf("read-only SQL=%v unsafe scans=%d", db.readOnlySQL, db.unsafeScans)
	}

	db.readOnlyError = errors.New("catalog probe denied")
	restored, err = verify(context.Background(), action)
	if err == nil || restored || !strings.Contains(err.Error(), "catalog probe denied") {
		t.Fatalf("probe failure restored=%t err=%v", restored, err)
	}

	db.readOnlyError = nil
	db.readOnlySQL = nil
	action.Verify = json.RawMessage(
		`{"sql":"DELETE FROM gsbench.plan_data RETURNING id","expected":"0"}`,
	)
	restored, err = verify(context.Background(), action)
	if err == nil || restored {
		t.Fatalf("untrusted verifier restored=%t err=%v", restored, err)
	}
	if len(db.readOnlySQL) != 0 || db.unsafeScans != 0 {
		t.Fatalf("untrusted verifier executed SQL=%v unsafe=%d", db.readOnlySQL, db.unsafeScans)
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
		!strings.Contains(plan.Items[0].ManualAction, `{"operation":"delete"}`) ||
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
	if len(plan.Items) != 3 || plan.Items[0].State != RecoveryConflict ||
		plan.Items[1].State != RecoveryConflict {
		t.Fatalf("conflict not visible: %+v", plan.Items)
	}
	var analyzeAt = -1
	for index, item := range plan.Items {
		if item.Target == "plan_data analyze" {
			analyzeAt = index
		}
		if item.State == RecoveryConflict && len(item.Statements) != 0 {
			t.Fatalf("conflicting SQL was rendered: %+v", item)
		}
	}
	if analyzeAt != 2 {
		t.Fatalf("items=%+v", plan.Items)
	}
	lines := RecoveryPlanLines(plan)
	if !strings.Contains(strings.Join(lines, "\n"), "display_only=true") {
		t.Fatalf("lines=%q", lines)
	}
}

func TestRecoveryPlanFiltersScenarioBeforeConflictDetection(t *testing.T) {
	selected := recoverySQLAction("run-1", 1, 601, "index-601", "DROP INDEX index_601")
	conflictA := recoverySQLAction("run-2", 1, 602, "stats-a", "ANALYZE stats_a")
	conflictB := conflictA
	conflictB.Target = "stats-b"
	code := ScenarioCode(601)
	plan, err := BuildRecoveryPlan(
		context.Background(),
		RestoreDiscovery{DatabaseActions: []Action{selected, conflictA, conflictB}},
		RecoveryPlanFilter{ScenarioCode: &code},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].ScenarioCode != 601 ||
		plan.Items[0].State == RecoveryConflict {
		t.Fatalf("scenario filter leaked unrelated conflict: %+v", plan.Items)
	}
}

func TestRecoveryPlanRunFilterMergesOnlyMatchingSharedDependency(t *testing.T) {
	plan := MergePlanBaselineFindings(
		RecoveryPlan{Items: []RecoveryPlanItem{{
			RunID: "run-1", ScenarioCode: 605, Target: "journal-target",
		}}},
		[]PlanBaselineFinding{
			{
				ScenarioCodes: []ScenarioCode{605}, Target: "plan_index_drop_idx",
				Statements: []string{"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data(id)"},
			},
			{
				ScenarioCodes: []ScenarioCode{606}, Target: "unrelated-index",
				Statements: []string{"CREATE INDEX unrelated_index ON gsbench.plan_data(id)"},
			},
		},
		RecoveryPlanFilter{RunID: "run-1"},
	)
	if len(plan.Items) != 2 || plan.Items[1].Target != "plan_index_drop_idx" {
		t.Fatalf("run-scoped dependencies=%+v", plan.Items)
	}
}

func TestRecoveryPlanMergeKeepsStructuralDDLBeforeAnalyze(t *testing.T) {
	plan := RecoveryPlan{Items: []RecoveryPlanItem{{
		RunID: "run-1", ScenarioCode: 603, Kind: ActionSQLMutation,
		Target: "plan_data analyze", State: RecoveryPending,
		Statements: []string{"ANALYZE gsbench.plan_data;"},
	}}}
	merged := MergePlanBaselineFindings(
		plan,
		[]PlanBaselineFinding{{
			ScenarioCodes: []ScenarioCode{605}, Target: "plan_index_drop_idx",
			Statements: []string{"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data(id)"},
		}},
		RecoveryPlanFilter{},
	)
	if len(merged.Items) != 2 || merged.Items[0].Target != "plan_index_drop_idx" ||
		merged.Items[1].Target != "plan_data analyze" {
		t.Fatalf("merged order=%+v", merged.Items)
	}
}

func TestRecoveryPlanMergeKeepsStructuralDDLBeforeSessionAnalyzeGroup(t *testing.T) {
	plan := RecoveryPlan{Items: []RecoveryPlanItem{{
		RunID: "run-1", ScenarioCode: 604, Kind: ActionSQLMutation,
		Target: "session analyze", State: RecoveryPending,
		Statements: []string{
			"SET default_statistics_target=-2;",
			"ANALYZE gsbench.plan_data ((stats_corr_a,stats_corr_b));",
			"RESET default_statistics_target;",
		},
	}}}
	merged := MergePlanBaselineFindings(
		plan,
		[]PlanBaselineFinding{{
			ScenarioCodes: []ScenarioCode{605}, Target: "plan_index_drop_idx",
			Statements: []string{"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data(id)"},
		}},
		RecoveryPlanFilter{},
	)
	if len(merged.Items) != 2 || merged.Items[0].Target != "plan_index_drop_idx" ||
		merged.Items[1].Target != "session analyze" {
		t.Fatalf("merged order=%+v", merged.Items)
	}
}

func TestRecoveryPlanEmptyReportsAlreadyRestored(t *testing.T) {
	lines := RecoveryPlanLines(RecoveryPlan{})
	if len(lines) != 1 || !strings.Contains(lines[0], "ALREADY_RESTORED") {
		t.Fatalf("lines=%q", lines)
	}
}
