package gsbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type datasetBaselineCatalogTest struct {
	schemaExists bool
	existing     map[string]bool
	invalid      map[string]error
}

func (c datasetBaselineCatalogTest) SchemaExists(
	context.Context,
	string,
) (bool, error) {
	return c.schemaExists, nil
}

func (c datasetBaselineCatalogTest) DatasetObjectExists(
	_ context.Context,
	object DatasetObject,
) (bool, error) {
	return c.existing[object.Name], nil
}

func (c datasetBaselineCatalogTest) ValidateDatasetObject(
	_ context.Context,
	object DatasetObject,
) error {
	return c.invalid[object.Name]
}

func TestInspectDatasetBaselineReportsMissingAndInvalidObjectsWithoutMutation(t *testing.T) {
	cfg := BenchConfig{
		Run:  RunConfig{Profile: "quick"},
		Data: DataConfig{Schema: "gsbench", TargetBytes: 1 << 30},
	}
	catalog := datasetBaselineCatalogTest{
		schemaExists: true,
		existing:     map[string]bool{"meta_dataset": true},
		invalid:      map[string]error{"meta_dataset": errors.New("column shape differs")},
	}
	findings, err := InspectDatasetBaseline(
		context.Background(), cfg, testDatasetEnvironment(), catalog,
	)
	if err != nil {
		t.Fatal(err)
	}
	var invalid, missing *PlanBaselineFinding
	for index := range findings {
		finding := &findings[index]
		switch finding.Target {
		case "meta_dataset":
			invalid = finding
		case "meta_runs":
			missing = finding
		}
	}
	if invalid == nil || len(invalid.Statements) != 0 ||
		!strings.Contains(invalid.Detail, "column shape differs") {
		t.Fatalf("invalid finding=%+v", invalid)
	}
	if missing == nil || len(missing.Statements) != 1 ||
		!strings.HasPrefix(missing.Statements[0], "CREATE TABLE") {
		t.Fatalf("missing finding=%+v", missing)
	}
}

func TestInspectDatasetBaselineStartsWithCreateSchemaWhenSchemaIsMissing(t *testing.T) {
	cfg := BenchConfig{
		Run:  RunConfig{Profile: "quick"},
		Data: DataConfig{Schema: "Bench", TargetBytes: 1 << 30},
	}
	findings, err := InspectDatasetBaseline(
		context.Background(), cfg, testDatasetEnvironment(),
		datasetBaselineCatalogTest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) < 2 || findings[0].Target != "Bench" ||
		len(findings[0].Statements) != 1 ||
		findings[0].Statements[0] != `CREATE SCHEMA "Bench"` {
		t.Fatalf("findings=%+v", findings)
	}
	if !recoveryDiscoveryCanFallBackToBaseline(findings, "Bench") {
		t.Fatalf("missing schema did not enable baseline-only discovery: %+v", findings)
	}
}

func TestRecoveryDiscoveryFallsBackWhenJournalTableIsMissing(t *testing.T) {
	findings := []PlanBaselineFinding{{
		Check: "object_presence", Target: "meta_journal",
		Actual: "missing", Expected: "present",
	}}
	if !recoveryDiscoveryCanFallBackToBaseline(findings, "gsbench") {
		t.Fatalf("findings=%+v", findings)
	}
}

func TestDatasetBaselineAssociatesScenarioDependenciesWithMissingObjects(t *testing.T) {
	cfg := BenchConfig{
		Run:  RunConfig{Profile: "quick"},
		Data: DataConfig{Schema: "gsbench", TargetBytes: 1 << 30},
	}
	findings, err := InspectDatasetBaseline(
		context.Background(), cfg, testDatasetEnvironment(),
		datasetBaselineCatalogTest{schemaExists: true, existing: map[string]bool{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]ScenarioCode{
		"network_ingress":   322,
		"hardparse_targets": 625,
		"vacuum_targets":    801,
	}
	for target, code := range want {
		found := false
		for _, finding := range findings {
			if finding.Target == target && containsScenarioCode(finding.ScenarioCodes, code) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("target=%s missing scenario=%03d findings=%+v", target, code, findings)
		}
	}
}

func TestRunScopedRecoveryMergesOnlyItsDatasetDependencies(t *testing.T) {
	cfg := BenchConfig{
		Run:  RunConfig{Profile: "quick"},
		Data: DataConfig{Schema: "gsbench", TargetBytes: 1 << 30},
	}
	findings, err := InspectDatasetBaseline(
		context.Background(), cfg, testDatasetEnvironment(),
		datasetBaselineCatalogTest{schemaExists: true, existing: map[string]bool{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	plan := MergePlanBaselineFindings(
		RecoveryPlan{Items: []RecoveryPlanItem{{
			RunID: "run-801", ScenarioCode: 801, Target: "journal-vacuum-action",
		}}},
		findings,
		RecoveryPlanFilter{RunID: "run-801"},
	)
	var vacuumFound, unrelatedFound bool
	for _, item := range plan.Items {
		if item.Target == "vacuum_targets" {
			vacuumFound = true
		}
		if item.Target == "hardparse_targets" {
			unrelatedFound = true
		}
	}
	if !vacuumFound || unrelatedFound {
		t.Fatalf("run-scoped plan=%+v", plan.Items)
	}
}

func containsScenarioCode(codes []ScenarioCode, want ScenarioCode) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
