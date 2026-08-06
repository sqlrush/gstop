package gsbench

import (
	"context"
	"database/sql"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type planSessionRestoreDatabase struct {
	execCalls    []string
	sessionCalls [][]string
}

func (d *planSessionRestoreDatabase) Exec(
	_ context.Context,
	query string,
	_ ...any,
) (sql.Result, error) {
	d.execCalls = append(d.execCalls, query)
	return nil, nil
}

func (*planSessionRestoreDatabase) Scan(
	context.Context,
	string,
	[]any,
	...any,
) error {
	return nil
}

func (d *planSessionRestoreDatabase) ExecSession(
	_ context.Context,
	statements ...string,
) error {
	d.sessionCalls = append(
		d.sessionCalls,
		append([]string(nil), statements...),
	)
	return nil
}

func TestPlanDefinitionsCoverSixScenariosWithLiteralSQL(t *testing.T) {
	defs, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"planchange_stats_target": false, "planchange_stats_lookup": false,
		"planchange_stats_ndistinct": false, "planchange_stats_extended": false,
		"planchange_index_drop": false, "planchange_index_shape": false,
	}
	bind := regexp.MustCompile(`\$[0-9]+|\?`)
	for _, def := range defs {
		if _, ok := want[def.Name]; !ok {
			t.Fatalf("unexpected definition %s", def.Name)
		}
		want[def.Name] = true
		if len(def.Candidates) < 2 {
			t.Fatalf("%s candidates=%d", def.Name, len(def.Candidates))
		}
		for _, query := range def.Candidates {
			if bind.MatchString(query) {
				t.Fatalf("%s uses bind placeholder: %s", def.Name, query)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing definition %s", name)
		}
	}
}

func TestPlanchangeDefinitionsUseApprovedIdentities(t *testing.T) {
	want := map[ScenarioCode]string{
		601: "planchange_stats_target", 602: "planchange_stats_lookup",
		603: "planchange_stats_ndistinct", 604: "planchange_stats_extended",
		605: "planchange_index_drop", 606: "planchange_index_shape",
	}
	defs, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range defs {
		if got := want[def.Code]; got != def.Name {
			t.Fatalf("definition=%+v want name=%q", def, got)
		}
	}
}

func TestPlanchangeStatsLookupReuses601PointTraffic(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("Bench")
	if err != nil {
		t.Fatal(err)
	}
	var one, two PlanScenarioDefinition
	for _, definition := range definitions {
		switch definition.Code {
		case 601:
			one = definition
		case 602:
			two = definition
		}
	}
	if two.Name != "planchange_stats_lookup" ||
		!reflect.DeepEqual(two.Candidates, one.Candidates) ||
		two.ExpectedBaselineToken != "plan_data_lookup_idx" {
		t.Fatalf("601=%+v 602=%+v", one, two)
	}
}

func TestPlanchangeStatsTargetUsesUniquePointLookups(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("Bench")
	if err != nil {
		t.Fatal(err)
	}
	var definition PlanScenarioDefinition
	for _, candidate := range definitions {
		if candidate.Code == 601 {
			definition = candidate
			break
		}
	}
	if definition.Code != 601 {
		t.Fatal("601 definition is unavailable")
	}
	if got, want := definition.ExpectedBaselineToken, "plan_data_lookup_idx"; got != want {
		t.Fatalf("601 baseline token=%q want=%q", got, want)
	}
	want := []string{
		`SELECT id,payload FROM "Bench".plan_data WHERE lookup_key=1`,
		`SELECT id,payload FROM "Bench".plan_data WHERE lookup_key=500000`,
		`SELECT id,payload FROM "Bench".plan_data WHERE lookup_key=1000000`,
	}
	if !reflect.DeepEqual(definition.Candidates, want) {
		t.Fatalf("601 candidates=%q want=%q", definition.Candidates, want)
	}
	for _, candidate := range definition.Candidates {
		if strings.Contains(candidate, "stats_target_key") || strings.Contains(candidate, "BETWEEN") || strings.Contains(candidate, "dist_key=") {
			t.Fatalf("601 must use only point lookup predicates: %q", candidate)
		}
	}
}

func TestPlanchangeStatsTargetDropsAndRecreatesCanonicalUniqueIndex(t *testing.T) {
	mutations, err := PlanMutationSet("run-1", "Bench", "planchange_stats_target")
	if err != nil {
		t.Fatal(err)
	}
	if len(mutations) != 1 {
		t.Fatalf("601 mutations=%d want=1: %+v", len(mutations), mutations)
	}
	mutation := mutations[0]
	if got, want := mutation.ForwardSQL, `DROP INDEX "Bench".plan_data_lookup_idx`; got != want {
		t.Fatalf("601 forward=%q want=%q", got, want)
	}
	wantCreate := `CREATE UNIQUE INDEX plan_data_lookup_idx ON "Bench".plan_data (lookup_key,dist_key)`
	if got := mutation.InverseSQL; got != wantCreate {
		t.Fatalf("601 inverse=%q want=%q", got, wantCreate)
	}
	if !strings.Contains(mutation.VerifySQL, "pg_get_indexdef") || mutation.VerifyValue != wantCreate {
		t.Fatalf("601 verification=%q expected=%q", mutation.VerifySQL, mutation.VerifyValue)
	}
	if !mutation.SkipInverseWhenRestored {
		t.Fatal("601 index drop must skip duplicate inverse when baseline is restored")
	}
}

func TestPlanchangeIndexDropRetainsCanonicalBaselineIndex(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("Bench")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Code != 605 {
			continue
		}
		if got, want := definition.ExpectedBaselineToken, "plan_index_drop_idx"; got != want {
			t.Fatalf("605 baseline token=%q want=%q", got, want)
		}
		return
	}
	t.Fatal("605 definition is unavailable")
}

func TestPlanDefinitionsPreserveMixedCaseSchemaIdentifiers(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("Bench")
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		for _, candidate := range definition.Candidates {
			if !strings.Contains(candidate, `"Bench".plan_data`) {
				t.Fatalf("candidate lost schema case: %s", candidate)
			}
		}
	}
	mutations, err := PlanMutationSet(
		"run-1", "Bench", "planchange_index_drop",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		mutations[0].ForwardSQL,
		`DROP INDEX "Bench".plan_index_drop_idx`,
	) || !strings.Contains(
		mutations[0].InverseSQL,
		`ON "Bench".plan_data`,
	) {
		t.Fatalf("mixed-case index mutation=%+v", mutations[0])
	}
}

func TestEveryPlanMutationHasInverseAndRestoreVerification(t *testing.T) {
	for _, name := range []string{
		"planchange_stats_target", "planchange_index_unusable", "planchange_stats_ndistinct",
		"planchange_stats_extended", "planchange_index_drop", "planchange_index_shape",
	} {
		mutations, err := PlanMutationSet("run-1", "gsbench", name)
		if err != nil {
			t.Fatal(err)
		}
		if name == "planchange_index_shape" && len(mutations) != 2 {
			t.Fatalf("shape mutations=%d want=2", len(mutations))
		}
		for _, mutation := range mutations {
			if mutation.Scenario != name || mutation.ForwardSQL == "" ||
				mutation.InverseSQL == "" || mutation.VerifySQL == "" {
				t.Fatalf("%s mutation=%+v", name, mutation)
			}
			if strings.Contains(mutation.ForwardSQL, ";") || strings.Contains(mutation.InverseSQL, ";") {
				t.Fatalf("%s uses a multi-command mutation unsupported by openGauss: %+v", name, mutation)
			}
		}
	}

	shape, _ := PlanMutationSet("run-1", "gsbench", "planchange_index_shape")
	if !strings.Contains(shape[0].ForwardSQL, "DROP INDEX") ||
		!strings.Contains(shape[1].ForwardSQL, "index_shape_tail,index_shape_lead") {
		t.Fatalf("shape mutations=%+v", shape)
	}
	unusable, _ := PlanMutationSet("run-1", "gsbench", "planchange_index_unusable")
	if !strings.Contains(unusable[0].ForwardSQL, "UNUSABLE") ||
		!strings.Contains(unusable[0].InverseSQL, "REBUILD") {
		t.Fatalf("unusable mutation=%+v", unusable)
	}
}

func TestNonIdempotentPlanInversesRequireBaselineSkipMarker(t *testing.T) {
	for _, test := range []struct {
		scenario string
		kind     string
	}{
		{"planchange_stats_target", "index_drop"},
		{"planchange_stats_extended", "statistics_extended"},
		{"planchange_index_drop", "index_drop"},
		{"planchange_index_shape", "index_shape_drop_good"},
	} {
		mutations, err := PlanMutationSet("run-1", "gsbench", test.scenario)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, mutation := range mutations {
			if mutation.Kind != test.kind {
				continue
			}
			found = true
			if !mutation.SkipInverseWhenRestored {
				t.Fatalf("%s %s missing skip marker", test.scenario, test.kind)
			}
			action := SQLAction(mutation)
			if !actionSkipsPlannedInverseWhenRestored(action) {
				t.Fatalf("%s %s marker missing from journal action", test.scenario, test.kind)
			}
		}
		if !found {
			t.Fatalf("%s missing mutation %s", test.scenario, test.kind)
		}
	}
}

func TestIndexPlanMutationsUseCompleteCanonicalDefinitions(t *testing.T) {
	drop, err := PlanMutationSet(
		"run-1", "gsbench", "planchange_index_drop",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := compactPlanDDL(drop[0].InverseSQL), compactPlanDDL(
		"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data (index_drop_key,dist_key,id)",
	); got != want {
		t.Fatalf("605 inverse=%q want=%q", drop[0].InverseSQL, want)
	}
	if !strings.Contains(drop[0].VerifySQL, "pg_get_indexdef") ||
		compactPlanDDL(drop[0].VerifyValue) != compactPlanDDL(drop[0].InverseSQL) {
		t.Fatalf(
			"605 restore verification does not inspect the canonical index shape: sql=%q expected=%q",
			drop[0].VerifySQL,
			drop[0].VerifyValue,
		)
	}

	shape, err := PlanMutationSet(
		"run-1", "gsbench", "planchange_index_shape",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := compactPlanDDL(shape[0].InverseSQL), compactPlanDDL(
		"CREATE INDEX plan_index_shape_good_idx ON gsbench.plan_data (index_shape_lead,index_shape_tail,dist_key,id)",
	); got != want {
		t.Fatalf("606 good inverse=%q want=%q", shape[0].InverseSQL, want)
	}
	if !strings.Contains(shape[0].VerifySQL, "pg_get_indexdef") ||
		compactPlanDDL(shape[0].VerifyValue) != compactPlanDDL(shape[0].InverseSQL) {
		t.Fatalf(
			"606 restore verification does not inspect the canonical index shape: sql=%q expected=%q",
			shape[0].VerifySQL,
			shape[0].VerifyValue,
		)
	}
	if got, want := compactPlanDDL(shape[1].ForwardSQL), compactPlanDDL(
		"CREATE INDEX plan_index_shape_bad_idx ON gsbench.plan_data (index_shape_tail,index_shape_lead,dist_key,id)",
	); got != want {
		t.Fatalf("606 bad forward=%q want=%q", shape[1].ForwardSQL, want)
	}
}

type planStatisticsRestoreDatabase struct {
	events []string
}

func (d *planStatisticsRestoreDatabase) Exec(
	_ context.Context,
	query string,
	_ ...any,
) (sql.Result, error) {
	d.events = append(d.events, query)
	return nil, nil
}

func (*planStatisticsRestoreDatabase) Scan(
	context.Context,
	string,
	[]any,
	...any,
) error {
	return nil
}

func (d *planStatisticsRestoreDatabase) ExecSession(
	_ context.Context,
	statements ...string,
) error {
	d.events = append(d.events, "SESSION: "+strings.Join(statements, " | "))
	return nil
}

func TestCombinedPlanStatisticsRestoreExecutesPrerequisitesBeforeAnalyze(
	t *testing.T,
) {
	var actions []Action
	var sequence int64
	for _, scenario := range []string{
		"planchange_stats_ndistinct",
		"planchange_stats_extended",
	} {
		mutations, err := PlanMutationSet("run-stats", "gsbench", scenario)
		if err != nil {
			t.Fatal(err)
		}
		for _, mutation := range mutations {
			sequence++
			mutation.TargetProduct = ProductGaussDB
			action := SQLAction(mutation)
			action.Sequence = sequence
			action.State = MutationApplied
			actions = append(actions, action)
		}
	}

	_, ordered, err := prepareRestorePlan(
		RestoreDiscovery{DatabaseActions: actions},
		"run-stats",
	)
	if err != nil {
		t.Fatal(err)
	}
	database := &planStatisticsRestoreDatabase{}
	executor := dbActionExecutor{db: database}
	for _, action := range ordered {
		if err := executor.Restore(context.Background(), action); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{
		`ALTER TABLE "gsbench".plan_data ADD STATISTICS ((stats_corr_a,stats_corr_b))`,
		`ALTER TABLE "gsbench".plan_data ALTER COLUMN stats_ndistinct_key RESET (n_distinct)`,
		`ALTER TABLE "gsbench".plan_data ALTER COLUMN stats_ndistinct_key SET STATISTICS -1`,
		`SESSION: SET default_statistics_target=-2 | ANALYZE "gsbench".plan_data ((stats_corr_a,stats_corr_b)) | RESET default_statistics_target`,
		`ANALYZE "gsbench".plan_data(stats_ndistinct_key)`,
	}
	if !reflect.DeepEqual(database.events, want) {
		t.Fatalf("restore events=%q want=%q", database.events, want)
	}
}

func compactPlanDDL(statement string) string {
	return strings.NewReplacer(
		" ", "",
		"\t", "",
		"\n", "",
		`"`, "",
	).Replace(strings.ToLower(statement))
}

func TestPlanChangeMutationsAreIndependentlyRestorable(t *testing.T) {
	for _, scenario := range []string{"planchange_stats_ndistinct", "planchange_stats_extended"} {
		mutations, err := PlanMutationSet("run-1", "gsbench", scenario)
		if err != nil {
			t.Fatal(err)
		}
		for _, mutation := range mutations {
			if strings.Contains(mutation.InverseSQL, ";") || mutation.InverseSQL == "" || mutation.VerifySQL == "" {
				t.Fatalf("%s mutation is not independently recoverable: %+v", scenario, mutation)
			}
		}
	}
	extended, _ := PlanMutationSet("run-1", "gsbench", "planchange_stats_extended")
	if !strings.Contains(extended[0].InverseSQL, "ADD STATISTICS") {
		t.Fatalf("extended statistics inverse=%q", extended[0].InverseSQL)
	}
}

func TestEveryPlanMutationRestoresAfterAnInterruptedAction(t *testing.T) {
	for _, scenario := range []string{"planchange_stats_ndistinct", "planchange_stats_extended"} {
		mutations, err := PlanMutationSet("run-1", "gsbench", scenario)
		if err != nil {
			t.Fatal(err)
		}
		for _, mutation := range mutations {
			store := &memoryActionStore{}
			executor := &memoryActionExecutor{}
			journal := NewJournal(store, executor, ProductOpenGauss)
			if err := journal.Apply(context.Background(), mutation); err != nil {
				t.Fatalf("%s apply %s: %v", scenario, mutation.Kind, err)
			}
			action := store.entries[0]
			action.State = MutationApplied
			if err := journal.restoreCoordinatorActions(context.Background(), []Action{action}); err != nil {
				t.Fatalf("%s interrupted restore %s: %v", scenario, mutation.Kind, err)
			}
			if len(executor.verifyActions) != 1 {
				t.Fatalf("%s %s inverse verification=%d", scenario, mutation.Kind, len(executor.verifyActions))
			}
		}
	}
}

func TestExtendedStatisticsAnalyzeRestoresOnOneSession(t *testing.T) {
	mutations, err := PlanMutationSet(
		"run-1", "gsbench", "planchange_stats_extended",
	)
	if err != nil {
		t.Fatal(err)
	}
	var analyze Mutation
	for _, mutation := range mutations {
		if mutation.Kind == "statistics_extended_analyze" {
			analyze = mutation
			break
		}
	}
	want := []string{
		"SET default_statistics_target=-2",
		"ANALYZE \"gsbench\".plan_data ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	}
	if !reflect.DeepEqual(analyze.InverseSessionSQL, want) {
		t.Fatalf("inverse session SQL=%v want=%v", analyze.InverseSessionSQL, want)
	}
	action := SQLAction(analyze)
	database := &planSessionRestoreDatabase{}
	executor := dbActionExecutor{db: database}
	if err := executor.Preflight(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if err := executor.Restore(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if len(database.execCalls) != 0 ||
		!reflect.DeepEqual(database.sessionCalls, [][]string{want}) {
		t.Fatalf(
			"exec=%v session=%v",
			database.execCalls,
			database.sessionCalls,
		)
	}
}
