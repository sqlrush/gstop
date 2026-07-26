package gsbench

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func datasetConfig(profile string, maxGB int) BenchConfig {
	return BenchConfig{
		Run:  RunConfig{Profile: profile},
		Data: DataConfig{Schema: "gsbench", MaxSizeGB: maxGB, MinFreeDiskPercent: 20, ReuseExisting: true},
	}
}

func testDatasetEnvironment() Environment {
	return Environment{Product: ProductOpenGauss, Topology: TopologyStandalone}
}

type guardedDatasetExecutor struct {
	recordingDatasetExecutor
	checks int
}

func (e *guardedDatasetExecutor) CheckCapacity(context.Context) error {
	e.checks++
	return errors.New("disk safety threshold reached")
}

func TestDatasetChecksDiskCapacityBetweenBatches(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	exec := &guardedDatasetExecutor{recordingDatasetExecutor: recordingDatasetExecutor{completed: map[string]int64{}}}
	err = NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "disk safety") {
		t.Fatalf("err=%v", err)
	}
	if exec.checks != 1 {
		t.Fatalf("capacity checks=%d", exec.checks)
	}
}

func TestDatasetQuickPlanRespectsFiveGBAndDiskReserve(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes > 5<<30 {
		t.Fatalf("estimated bytes = %d", plan.EstimatedBytes)
	}
	if plan.ReservedFreeBytes < 6<<30 {
		t.Fatalf("reserved bytes = %d", plan.ReservedFreeBytes)
	}
	if plan.EstimatedBytes > plan.AvailableForData {
		t.Fatalf("plan exceeds safe capacity: %+v", plan)
	}
}

func TestDatasetStressPlanDefaultsToAtMostTwentyGB(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("stress", 20), Capacity{TotalBytes: 200 << 30, FreeBytes: 100 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes != 20<<30 {
		t.Fatalf("estimated bytes = %d", plan.EstimatedBytes)
	}
}

func TestDatasetExplicitSizeOverridesProfileCap(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.TargetBytes = 100 << 30
	plan, err := PlanDataset(cfg, Capacity{TotalBytes: 1 << 40, FreeBytes: 1 << 40}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes != 100<<30 {
		t.Fatalf("estimated bytes=%d", plan.EstimatedBytes)
	}
}

func TestDatasetRejectsTargetAboveTwoTiB(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.TargetBytes = (2 << 40) + 1
	if _, err := PlanDataset(cfg, Capacity{TotalBytes: 4 << 40, FreeBytes: 4 << 40}, testDatasetEnvironment()); err == nil {
		t.Fatal("accepted dataset target above 2TiB")
	}
}

func TestDatasetAcceptsExactTwoTiBBoundary(t *testing.T) {
	cfg := datasetConfig("stress", 20)
	cfg.Data.TargetBytes = 2 << 40
	plan, err := PlanDataset(
		cfg,
		Capacity{TotalBytes: 4 << 40, FreeBytes: 4 << 40},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.EstimatedBytes != 2<<40 {
		t.Fatalf("estimated bytes=%d", plan.EstimatedBytes)
	}
}

func TestDatasetRejectsTargetBelowOneGiB(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.TargetBytes = (1 << 30) - 1
	if _, err := PlanDataset(
		cfg,
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		Environment{Product: ProductOpenGauss, Topology: TopologyStandalone},
	); err == nil {
		t.Fatal("accepted dataset target below 1GiB")
	}
}

func TestDatasetPlanSelectsDetectedDistributedDialect(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 5),
		Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30},
		Environment{Product: ProductGaussDB, Topology: TopologyDistributed},
	)
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Join(plan.DDL, "\n")
	if !strings.Contains(ddl, "DISTRIBUTE BY HASH (dist_key)") ||
		!strings.Contains(ddl, "DISTRIBUTE BY REPLICATION") {
		t.Fatalf("distributed plan did not select distributed DDL:\n%s", ddl)
	}
}

func TestDatasetPlanDefersPlanIndexesUntilAfterMigration(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 5),
		Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	const index = "CREATE INDEX plan_stats_target_idx "
	if got := strings.Count(strings.Join(plan.DDL, "\n"), index); got != 0 {
		t.Fatalf("pre-migration plan index count=%d want=0", got)
	}
	if got := strings.Count(strings.Join(plan.PostMigrationDDL, "\n"), index); got != 1 {
		t.Fatalf("post-migration plan index count=%d want=1", got)
	}
}

func TestDatasetPlanContainsNoIfNotExistsDDL(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 5),
		Capacity{TotalBytes: 30 << 30, FreeBytes: 30 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Join(append(append([]string{}, plan.DDL...), plan.PostMigrationDDL...), "\n")
	if strings.Contains(ddl, "IF NOT EXISTS") {
		t.Fatalf("dataset plan contains unsupported IF NOT EXISTS:\n%s", ddl)
	}
}

func TestDatasetPlanFailsWhenSafeFreeSpaceIsTooSmall(t *testing.T) {
	_, err := PlanDataset(datasetConfig("quick", 5), Capacity{TotalBytes: 100 << 30, FreeBytes: 15 << 30}, testDatasetEnvironment())
	if err == nil {
		t.Fatal("expected disk reserve error")
	}
	for _, field := range []string{"target=", "free=", "reserved=", "safe_available="} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("capacity diagnostic missing %q: %v", field, err)
		}
	}
}

func TestDatasetDDLIncludesEveryScenarioTable(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	ddl := strings.Join(plan.DDL, "\n")
	for _, table := range []string{
		"accounts", "customers", "orders", "order_items", "fact_sales",
		"dim_product", "dim_store", "plan_data", "lock_targets", "lock_table_targets", "lock_ddl_targets", "vacuum_targets",
		"meta_runs", "meta_journal", "meta_batches", "meta_plan_cache",
	} {
		if !strings.Contains(ddl, `"gsbench".`+table) {
			t.Errorf("DDL missing %s", table)
		}
	}
}

type recordingDatasetExecutor struct {
	statements          []string
	completed           map[string]int64
	schemaExists        bool
	schemaChecks        int
	schemaExistsOnRetry bool
	createSchemaErr     error
}

type catalogDatasetExecutor struct {
	recordingDatasetExecutor
	existing  map[string]bool
	validated []string
	migrated  []string
	events    []string
	version   string
	recorded  string
}

type atomicDatasetExecutor struct {
	recordingDatasetExecutor
	applied []string
	fail    error
}

func (e *atomicDatasetExecutor) ApplyDatasetBatch(
	_ context.Context,
	schema string,
	batch TableBatch,
	start, end int64,
	version string,
) error {
	e.applied = append(e.applied, fmt.Sprintf(
		"%s:%s:%d-%d:%s", schema, batch.Table, start, end, version,
	))
	return e.fail
}

func (e *catalogDatasetExecutor) DatasetObjectExists(
	_ context.Context,
	object DatasetObject,
) (bool, error) {
	return e.existing[string(object.Kind)+":"+object.Name], nil
}

func (e *catalogDatasetExecutor) MigrateLegacyDatasetObject(
	_ context.Context,
	object DatasetObject,
) error {
	e.migrated = append(e.migrated, object.Name)
	e.events = append(e.events, "migrate:"+object.Name)
	return nil
}

func (e *catalogDatasetExecutor) ValidateDatasetObject(
	_ context.Context,
	object DatasetObject,
) error {
	e.validated = append(e.validated, object.Name)
	e.events = append(e.events, "validate:"+object.Name)
	return nil
}

func (e *catalogDatasetExecutor) DatasetVersion(context.Context, string) (string, error) {
	return e.version, nil
}

func (e *catalogDatasetExecutor) RecordDatasetVersion(_ context.Context, _ string, version string) error {
	e.recorded = version
	return nil
}

func (e *recordingDatasetExecutor) Exec(_ context.Context, query string, _ ...any) error {
	e.statements = append(e.statements, query)
	if strings.HasPrefix(query, "CREATE SCHEMA ") && e.createSchemaErr != nil {
		return e.createSchemaErr
	}
	return nil
}

func (e *recordingDatasetExecutor) SchemaExists(context.Context, string) (bool, error) {
	e.schemaChecks++
	if e.schemaChecks > 1 && e.schemaExistsOnRetry {
		return true, nil
	}
	return e.schemaExists, nil
}

func (e *recordingDatasetExecutor) BatchHighWater(_ context.Context, table string) (int64, error) {
	return e.completed[table], nil
}

func (e *recordingDatasetExecutor) ColumnExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func TestDatasetInitSkipsCompletedBatches(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 2), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Batches[0]
	exec := &recordingDatasetExecutor{completed: map[string]int64{first.Table: first.Rows}}
	manager := NewDatasetManager(exec)
	if err := manager.Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, query := range exec.statements {
		if strings.Contains(query, `INSERT INTO "gsbench".`+first.Table) {
			t.Fatalf("completed table was generated again: %s", query)
		}
	}
}

func TestDatasetInitReportsNoShrinkWhenHighWaterExceedsTarget(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 1), Capacity{
		TotalBytes: 20 << 30, FreeBytes: 20 << 30,
	}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	first := plan.Batches[0]
	exec := &recordingDatasetExecutor{
		completed: completedDatasetBatches(plan),
	}
	exec.completed[first.Table] = first.Rows + 100
	var progress []string
	manager := NewDatasetManager(exec, func(format string, args ...any) {
		progress = append(progress, fmt.Sprintf(format, args...))
	})
	if err := manager.Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(progress, "\n")
	if !strings.Contains(text, "action=no_shrink") ||
		!strings.Contains(text, "table="+first.Table) {
		t.Fatalf("progress=%q", text)
	}
}

func TestDatasetInitRecordsCompleteBatchMetadata(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 1), Capacity{
		TotalBytes: 20 << 30, FreeBytes: 20 << 30,
	}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	completed := completedDatasetBatches(plan)
	first := plan.Batches[0]
	completed[first.Table] = first.Rows - 1
	exec := &recordingDatasetExecutor{completed: completed}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	const columns = "meta_batches(table_name,high_water,target_rows,estimated_row_bytes,dataset_version)"
	if !strings.Contains(strings.Join(exec.statements, "\n"), columns) {
		t.Fatalf("batch metadata insert missing required columns:\n%s", strings.Join(exec.statements, "\n"))
	}
}

func completedDatasetBatches(plan DatasetPlan) map[string]int64 {
	completed := make(map[string]int64)
	for _, batch := range plan.Batches {
		completed[batch.Table] = batch.Rows
	}
	for _, migration := range plan.Migrations {
		completed[migration.Name] = completed[migration.SourceTable]
	}
	return completed
}

func TestDatasetInitChecksNamespaceBeforePlainCreate(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 1), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingDatasetExecutor{completed: completedDatasetBatches(plan)}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if exec.schemaChecks != 1 {
		t.Fatalf("schema checks=%d", exec.schemaChecks)
	}
	if len(exec.statements) == 0 || exec.statements[0] != `CREATE SCHEMA "gsbench"` {
		t.Fatalf("statements=%v", exec.statements)
	}
	if strings.Contains(strings.Join(exec.statements, "\n"), "CREATE SCHEMA IF NOT EXISTS") {
		t.Fatal("unsupported schema syntax executed")
	}
}

func TestDatasetInitSkipsExistingSchema(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 1), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingDatasetExecutor{
		completed: completedDatasetBatches(plan), schemaExists: true,
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, statement := range exec.statements {
		if strings.HasPrefix(statement, "CREATE SCHEMA") {
			t.Fatalf("existing schema recreated: %s", statement)
		}
	}
}

func TestDatasetInitRechecksSchemaAfterConcurrentCreate(t *testing.T) {
	plan, err := PlanDataset(datasetConfig("quick", 1), Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30}, testDatasetEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingDatasetExecutor{
		completed:           completedDatasetBatches(plan),
		createSchemaErr:     errors.New("duplicate_schema"),
		schemaExistsOnRetry: true,
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if exec.schemaChecks != 2 {
		t.Fatalf("schema checks=%d", exec.schemaChecks)
	}
}

func TestDatasetInitQuotesSchemaAtEveryInterpolationBoundary(t *testing.T) {
	cfg := datasetConfig("quick", 1)
	cfg.Data.Schema = "Bench"
	plan, err := PlanDataset(
		cfg,
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	exec := &recordingDatasetExecutor{completed: completedDatasetBatches(plan)}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(exec.statements, "\n")
	for _, unsafe := range []string{"CREATE SCHEMA Bench", "TABLE Bench.", "INTO Bench.", "ANALYZE Bench."} {
		if strings.Contains(joined, unsafe) {
			t.Fatalf("unquoted schema boundary %q:\n%s", unsafe, joined)
		}
	}
	for _, required := range []string{`CREATE SCHEMA "Bench"`, `ALTER TABLE "Bench".plan_data`, `ANALYZE "Bench".customers`} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing quoted boundary %q:\n%s", required, joined)
		}
	}
}

func TestDatasetInitCatalogChecksAndValidatesExistingObjects(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 1),
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	existing := map[string]bool{}
	for _, statement := range append(append([]string{}, plan.DDL...), plan.PostMigrationDDL...) {
		object, err := parseDatasetObject(statement)
		if err != nil {
			t.Fatal(err)
		}
		existing[string(object.Kind)+":"+object.Name] = true
	}
	exec := &catalogDatasetExecutor{
		recordingDatasetExecutor: recordingDatasetExecutor{
			completed:    completedDatasetBatches(plan),
			schemaExists: true,
		},
		existing: existing,
		version:  datasetVersion,
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, statement := range exec.statements {
		if strings.HasPrefix(statement, "CREATE TABLE") || strings.HasPrefix(statement, "CREATE INDEX") {
			t.Fatalf("existing object recreated: %s", statement)
		}
	}
	if len(exec.validated) != 49 {
		t.Fatalf("validated objects=%d want=49", len(exec.validated))
	}
	if len(exec.migrated) != 33 {
		t.Fatalf("migrated table checks=%d want=33", len(exec.migrated))
	}
	if exec.recorded != datasetVersion {
		t.Fatalf("recorded version=%q want=%q", exec.recorded, datasetVersion)
	}
}

func TestDatasetInitCompletesLegacyMigrationsBeforeCatalogValidation(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 1),
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	exec := &catalogDatasetExecutor{
		recordingDatasetExecutor: recordingDatasetExecutor{
			completed:    completedDatasetBatches(plan),
			schemaExists: true,
		},
		existing: map[string]bool{},
	}
	for _, statement := range plan.DDL {
		object, parseErr := parseDatasetObject(statement)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		exec.existing[string(object.Kind)+":"+object.Name] = true
	}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	firstValidate := -1
	lastMigrate := -1
	for i, event := range exec.events {
		if strings.HasPrefix(event, "migrate:") {
			lastMigrate = i
		}
		if firstValidate < 0 && strings.HasPrefix(event, "validate:") {
			firstValidate = i
		}
	}
	if firstValidate < 0 || lastMigrate < 0 || firstValidate <= lastMigrate {
		t.Fatalf("catalog events=%v", exec.events)
	}
}

func TestDatasetInitRejectsUnsupportedVersionBeforeTableMutation(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 1),
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	exec := &catalogDatasetExecutor{
		recordingDatasetExecutor: recordingDatasetExecutor{
			completed:    completedDatasetBatches(plan),
			schemaExists: true,
		},
		existing: map[string]bool{},
		version:  "99",
	}
	for _, statement := range append(
		append([]string{}, plan.DDL...), plan.PostMigrationDDL...,
	) {
		object, parseErr := parseDatasetObject(statement)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		exec.existing[string(object.Kind)+":"+object.Name] = true
	}
	err = NewDatasetManager(exec).Init(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), `unsupported dataset version "99"`) {
		t.Fatalf("err=%v", err)
	}
	for _, statement := range exec.statements {
		if strings.HasPrefix(statement, "ALTER TABLE") ||
			strings.HasPrefix(statement, "UPDATE ") ||
			strings.HasPrefix(statement, "INSERT ") {
			t.Fatalf("unsupported dataset was mutated: %s", statement)
		}
	}
	for _, event := range exec.events {
		if strings.HasPrefix(event, "migrate:") ||
			strings.HasPrefix(event, "validate:") {
			t.Fatalf("unsupported dataset catalog was mutated/validated: %s", event)
		}
	}
}

func TestDatasetInitUsesAtomicBatchAndHighWaterOperation(t *testing.T) {
	plan, err := PlanDataset(
		datasetConfig("quick", 1),
		Capacity{TotalBytes: 20 << 30, FreeBytes: 20 << 30},
		testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	completed := completedDatasetBatches(plan)
	first := plan.Batches[0]
	completed[first.Table] = first.Rows - 1
	exec := &atomicDatasetExecutor{recordingDatasetExecutor: recordingDatasetExecutor{
		completed: completed,
	}}
	if err := NewDatasetManager(exec).Init(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exec.applied, []string{
		fmt.Sprintf("gsbench:%s:%d-%d:%s", first.Table, first.Rows, first.Rows, datasetVersion),
	}) {
		t.Fatalf("atomic batches=%v", exec.applied)
	}
	joined := strings.Join(exec.statements, "\n")
	if strings.Contains(joined, "DELETE FROM") ||
		strings.Contains(joined, "INSERT INTO \"gsbench\".meta_batches") {
		t.Fatalf("high water was updated outside atomic operation:\n%s", joined)
	}
}
