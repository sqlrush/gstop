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
		"planchange_stats_target": false, "planchange_index_unusable": false,
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
		601: "planchange_stats_target", 602: "planchange_index_unusable",
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
	if got, want := compactPlanDDL(shape[1].ForwardSQL), compactPlanDDL(
		"CREATE INDEX plan_index_shape_bad_idx ON gsbench.plan_data (index_shape_tail,index_shape_lead,dist_key,id)",
	); got != want {
		t.Fatalf("606 bad forward=%q want=%q", shape[1].ForwardSQL, want)
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
	for _, scenario := range []string{"planchange_stats_target", "planchange_stats_ndistinct", "planchange_stats_extended"} {
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
	stats, _ := PlanMutationSet("run-1", "gsbench", "planchange_stats_target")
	if !strings.Contains(stats[0].InverseSQL, "SET STATISTICS -1") {
		t.Fatalf("stats target inverse=%q", stats[0].InverseSQL)
	}
	extended, _ := PlanMutationSet("run-1", "gsbench", "planchange_stats_extended")
	if !strings.Contains(extended[0].InverseSQL, "ADD STATISTICS") {
		t.Fatalf("extended statistics inverse=%q", extended[0].InverseSQL)
	}
}

func TestEveryPlanMutationRestoresAfterAnInterruptedAction(t *testing.T) {
	for _, scenario := range []string{"planchange_stats_target", "planchange_stats_ndistinct", "planchange_stats_extended"} {
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
