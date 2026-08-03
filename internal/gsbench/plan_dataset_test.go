package gsbench

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPlanIndexDefinitionsReturnImmutableCanonicalCatalog(t *testing.T) {
	want := []planIndexDefinition{
		{Name: "plan_data_lookup_idx", Table: "plan_data", Columns: []string{"lookup_key", "dist_key"}, Unique: true},
		{Name: "plan_stats_target_idx", Table: "plan_data", Columns: []string{"stats_target_key", "dist_key", "id"}},
		{Name: "plan_stats_ndistinct_idx", Table: "plan_data", Columns: []string{"stats_ndistinct_key", "dist_key", "id"}},
		{Name: "plan_stats_corr_idx", Table: "plan_data", Columns: []string{"stats_corr_a", "stats_corr_b", "dist_key", "id"}},
		{Name: "plan_index_unusable_idx", Table: "plan_data", Columns: []string{"index_unusable_key", "dist_key", "id"}},
		{Name: "plan_index_drop_idx", Table: "plan_data", Columns: []string{"index_drop_key", "dist_key", "id"}},
		{Name: "plan_index_shape_good_idx", Table: "plan_data", Columns: []string{"index_shape_lead", "index_shape_tail", "dist_key", "id"}},
	}

	first := planIndexDefinitions()
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("plan index definitions=%+v want=%+v", first, want)
	}
	first[0].Name = "mutated"
	first[0].Columns[0] = "mutated"
	if second := planIndexDefinitions(); !reflect.DeepEqual(second, want) {
		t.Fatalf("catalog was mutated through getter: %+v", second)
	}
}

func TestPlanIndexDDLBuildsCanonicalUniqueLookupIndex(t *testing.T) {
	definition, ok := planIndexDefinitionByName("plan_data_lookup_idx")
	if !ok {
		t.Fatal("canonical lookup index is unavailable")
	}
	got, err := planIndexDDL("Bench", definition, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE UNIQUE INDEX plan_data_lookup_idx ON "Bench".plan_data (lookup_key,dist_key)`
	if got != want {
		t.Fatalf("lookup index DDL=%q want=%q", got, want)
	}
}

func TestCanonicalPlanIndexesHaveNoOtherLookupKeyAccessPath(t *testing.T) {
	for _, definition := range planIndexDefinitions() {
		if definition.Name == "plan_data_lookup_idx" {
			continue
		}
		for _, column := range definition.Columns {
			if column == "lookup_key" {
				t.Fatalf("%s provides lookup_key access after 601 drops its index", definition.Name)
			}
		}
	}
}

func TestPlanDataUsesDesignedTwelvePercentBudget(t *testing.T) {
	quick := planDataBatch("gsbench", 5<<30)
	if quick.Rows != 1_006_632 {
		t.Fatalf("quick plan rows=%d want=1006632", quick.Rows)
	}
	stress := planDataBatch("gsbench", 20<<30)
	if stress.Rows != 4_026_531 {
		t.Fatalf("stress plan rows=%d want=4026531", stress.Rows)
	}
	if quick.BatchSize != 104_857 {
		t.Fatalf("quick plan batch size=%d want=104857", quick.BatchSize)
	}
}

func TestPlanDataDDLContainsSixIndependentKeyGroups(t *testing.T) {
	ddl := strings.Join(append(planDataDDL("gsbench"), planDataPostMigrationDDL("gsbench")...), "\n")
	for _, token := range []string{
		"stats_target_key", "stats_ndistinct_key", "stats_corr_a", "stats_corr_b",
		"index_unusable_key", "index_drop_key", "index_shape_lead", "index_shape_tail",
		"plan_stats_target_idx", "plan_stats_ndistinct_idx", "plan_stats_corr_idx",
		"plan_index_unusable_idx", "plan_index_drop_idx", "plan_index_shape_good_idx",
	} {
		if !strings.Contains(ddl, token) {
			t.Errorf("plan DDL missing %s", token)
		}
	}
}

func TestPlanDataSlowWorkloadColumnsAreFilledByInsert(t *testing.T) {
	batch := planDataBatch("gsbench", 5<<30)
	for _, token := range []string{
		"dist_key", "lookup_key", "skew_key", "mod(g,1048576)+1",
		"stats_target_key", "stats_ndistinct_key", "stats_corr_a", "stats_corr_b",
		"index_unusable_key", "index_drop_key", "index_shape_lead", "index_shape_tail",
	} {
		if !strings.Contains(batch.InsertSQL, token) {
			t.Errorf("insert SQL missing %s", token)
		}
	}
}

func TestStatsTargetDataHasSeveralHeavyHittersForLowTargetPlanJump(t *testing.T) {
	values := planDataValues()
	for _, token := range []string{"mod(g,100)<80", "mod(g,4)+1", "mod(g,1000)+100"} {
		if !strings.Contains(values, token) {
			t.Fatalf("plan data distribution missing %q:\n%s", token, values)
		}
	}
	migrations := planDataMigrations("gsbench")
	found := false
	for _, migration := range migrations {
		if migration.Name == "plan_data_v3_stats_target" &&
			strings.Contains(migration.UpdateSQL, "mod(id,4)+1") {
			found = true
		}
	}
	if !found {
		t.Fatal("plan_data_v3_stats_target migration is missing")
	}
}

func TestDatasetPlanDataMigrationResumesFromItsOwnHighWater(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingDatasetExecutor{completed: map[string]int64{
		"plan_data":            250_000,
		"plan_data_v2_columns": 100_000,
	}}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	var v2Updates, v3Updates int
	for _, query := range exec.statements {
		if !strings.HasPrefix(query, `UPDATE "gsbench".plan_data SET`) {
			continue
		}
		if strings.Contains(query, "stats_ndistinct_key=") {
			v2Updates++
		}
		if strings.Contains(query, "mod(id,4)+1") {
			v3Updates++
		}
	}
	if v2Updates != 2 || v3Updates != 3 {
		t.Fatalf("migration updates v2=%d want=2 v3=%d want=3", v2Updates, v3Updates)
	}
}

type openGaussColumnExecutor struct {
	recordingDatasetExecutor
	existing map[string]bool
}

func (e *openGaussColumnExecutor) Exec(ctx context.Context, query string, args ...any) error {
	if strings.Contains(query, "ADD COLUMN IF NOT EXISTS") {
		return fmt.Errorf(`syntax error at or near "NOT EXISTS"`)
	}
	return e.recordingDatasetExecutor.Exec(ctx, query, args...)
}

func (e *openGaussColumnExecutor) ColumnExists(_ context.Context, _, _, column string) (bool, error) {
	return e.existing[column], nil
}

func TestDatasetInitAddsOnlyMissingPlanColumnsWithoutIfNotExists(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	completed := map[string]int64{}
	for _, batch := range plan.Batches {
		completed[batch.Table] = batch.Rows
	}
	exec := &openGaussColumnExecutor{
		recordingDatasetExecutor: recordingDatasetExecutor{completed: completed},
		existing:                 map[string]bool{"stats_target_key": true},
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(exec.statements, "\n")
	if strings.Contains(joined, "ADD COLUMN IF NOT EXISTS") {
		t.Fatalf("unsupported openGauss syntax was executed:\n%s", joined)
	}
	if strings.Contains(joined, "ADD COLUMN stats_target_key") {
		t.Fatalf("existing column was added again:\n%s", joined)
	}
	if !strings.Contains(joined, "ADD COLUMN stats_ndistinct_key bigint") {
		t.Fatalf("missing column was not added:\n%s", joined)
	}
}
