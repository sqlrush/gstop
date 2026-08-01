package gsbench

import (
	"strings"
	"testing"
)

func TestDatasetDialectContainsEveryDesignedObject(t *testing.T) {
	statements := DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	}).TableDDL("gsbench")
	tableNames := []string{
		"meta_dataset", "meta_runs", "meta_journal", "meta_batches", "meta_plan_cache",
		"customers", "accounts", "orders", "order_items", "dim_product", "dim_store",
		"fact_sales", "sort_data", "network_ingress", "global_cache_targets", "plan_data",
		"hardparse_targets", "lock_targets", "lock_table_targets", "lock_ddl_targets",
		"lock_mode_targets", "ddl_global_1", "ddl_global_2", "ddl_global_3", "ddl_global_4",
		"dist_lock_targets", "replication_targets", "replication_conflict_targets",
		"cluster_skew_data", "dist_join_data", "dist_small_hash", "dist_txn_targets",
		"vacuum_targets",
	}
	indexNames := []string{
		"accounts_customer_idx", "orders_customer_idx", "order_items_order_idx",
		"fact_sales_product_idx", "fact_sales_customer_idx", "sort_data_sort_idx",
		"plan_data_lookup_idx", "plan_stats_target_idx", "plan_stats_ndistinct_idx",
		"plan_stats_corr_idx", "plan_index_unusable_idx", "plan_index_drop_idx",
		"plan_index_shape_good_idx", "hardparse_targets_lookup_idx",
		"replication_conflict_run_idx", "vacuum_targets_group_idx",
	}
	gotTables := map[string]bool{}
	gotIndexes := map[string]bool{}
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			t.Fatal(err)
		}
		target := gotTables
		if object.Kind == DatasetObjectIndex {
			target = gotIndexes
		}
		if target[object.Name] {
			t.Fatalf("duplicate %s %s", object.Kind, object.Name)
		}
		target[object.Name] = true
	}
	if len(gotTables) != 33 || len(gotIndexes) != 16 {
		t.Fatalf("catalog tables=%d want=33 indexes=%d want=16",
			len(gotTables), len(gotIndexes))
	}
	for _, table := range tableNames {
		if !gotTables[table] {
			t.Errorf("DDL missing table %s", table)
		}
	}
	for _, index := range indexNames {
		if !gotIndexes[index] {
			t.Errorf("DDL missing index %s", index)
		}
	}
}

func TestDatasetDialectPlanIndexesMatchCanonicalCatalogExactlyOnce(t *testing.T) {
	statements := DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	}).TableDDL("Bench")
	actual := make(map[string][]string)
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			t.Fatal(err)
		}
		if object.Kind == DatasetObjectIndex {
			actual[object.Name] = append(actual[object.Name], statement)
		}
	}
	for _, definition := range planIndexDefinitions() {
		expected, err := planIndexDDL("Bench", definition, false)
		if err != nil {
			t.Fatal(err)
		}
		got := actual[definition.Name]
		if len(got) != 1 {
			t.Fatalf("plan index %s count=%d want=1", definition.Name, len(got))
		}
		if !datasetIndexMatches(got[0], expected) {
			t.Fatalf(
				"plan index %s definition=%q want=%q",
				definition.Name,
				got[0],
				expected,
			)
		}
	}
}

func TestDistributedDatasetUsesExplicitDistribution(t *testing.T) {
	dialect := DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	})
	ddl := strings.Join(dialect.TableDDL("gsbench"), "\n")
	for _, token := range []string{
		"DISTRIBUTE BY HASH (dist_key)",
		"DISTRIBUTE BY REPLICATION",
		"PRIMARY KEY (run_id, action_id)",
	} {
		if !strings.Contains(ddl, token) {
			t.Fatalf("missing %q in %s", token, ddl)
		}
	}
}

func TestDistributedDatasetUsesDesignedMetadataDistribution(t *testing.T) {
	ddl := strings.Join(DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	}).TableDDL("gsbench"), "\n")
	for _, token := range []string{
		"PRIMARY KEY (run_id, action_id)",
		"DISTRIBUTE BY HASH (run_id)",
		"PRIMARY KEY (signature, scenario_code)",
		"DISTRIBUTE BY HASH (signature)",
		"target_product varchar(32) NOT NULL",
		"last_error text",
	} {
		if !strings.Contains(ddl, token) {
			t.Errorf("distributed metadata DDL missing %q", token)
		}
	}
	if strings.Contains(ddl, "error_text") {
		t.Fatal("journal DDL retained obsolete error_text column")
	}
	if strings.Contains(strings.ToLower(ddl), "bigserial") {
		t.Fatal("distributed metadata uses replicated bigserial")
	}
}

func TestDistributedDatasetUsesDesignedSpecialDistributionKeys(t *testing.T) {
	ddl := strings.Join(DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	}).TableDDL("gsbench"), "\n")
	normalized := strings.Join(strings.Fields(ddl), " ")
	for _, token := range []string{
		"PRIMARY KEY (lookup_key, id) ) DISTRIBUTE BY HASH (lookup_key)",
		"PRIMARY KEY (skew_key, id) ) DISTRIBUTE BY HASH (skew_key)",
		`CREATE TABLE "gsbench".dim_product `,
		`CREATE TABLE "gsbench".dim_store `,
	} {
		if !strings.Contains(normalized, token) {
			t.Errorf("distributed DDL missing %q", token)
		}
	}
	if count := strings.Count(ddl, "DISTRIBUTE BY REPLICATION"); count != 3 {
		t.Fatalf("replicated table count=%d want=3", count)
	}
}

func TestDistributedDatasetDistributionGoldenMatchesEveryTable(t *testing.T) {
	statements := DatasetDialectFor(Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
	}).TableDDL("gsbench")
	replicated := map[string]bool{
		"meta_dataset": true,
		"dim_product":  true,
		"dim_store":    true,
	}
	hashKey := map[string]string{
		"meta_runs": "run_id", "meta_journal": "run_id",
		"meta_batches": "table_name", "meta_plan_cache": "signature",
		"customers": "dist_key", "accounts": "dist_key",
		"orders": "dist_key", "order_items": "dist_key",
		"fact_sales": "dist_key", "sort_data": "dist_key",
		"network_ingress": "dist_key", "global_cache_targets": "dist_key",
		"plan_data": "dist_key", "hardparse_targets": "lookup_key",
		"lock_targets": "dist_key", "lock_table_targets": "dist_key",
		"lock_ddl_targets": "dist_key", "lock_mode_targets": "dist_key",
		"ddl_global_1": "dist_key", "ddl_global_2": "dist_key",
		"ddl_global_3": "dist_key", "ddl_global_4": "dist_key",
		"dist_lock_targets": "dist_key", "replication_targets": "dist_key",
		"replication_conflict_targets": "dist_key",
		"cluster_skew_data":            "skew_key", "dist_join_data": "dist_key",
		"dist_small_hash": "dist_key", "dist_txn_targets": "dist_key",
		"vacuum_targets": "dist_key",
	}
	tableCount := 0
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			t.Fatal(err)
		}
		if object.Kind != DatasetObjectTable {
			continue
		}
		tableCount++
		if replicated[object.Name] {
			if object.Distribution != "REPLICATION" {
				t.Errorf("%s distribution=%q want REPLICATION",
					object.Name, object.Distribution)
			}
			continue
		}
		key, ok := hashKey[object.Name]
		if !ok {
			t.Errorf("unexpected table %s", object.Name)
			continue
		}
		wantDistribution := "HASH (" + key + ")"
		if object.Distribution != wantDistribution {
			t.Errorf("%s distribution=%q want %q",
				object.Name, object.Distribution, wantDistribution)
		}
		shape, err := expectedDatasetTableShape(object.DDL)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(
			normalizeDatasetSQL(shape.PrimaryKey),
			"("+strings.ToLower(key),
		) {
			t.Errorf("%s primary key %q does not lead with distribution key %s",
				object.Name, shape.PrimaryKey, key)
		}
	}
	if tableCount != 33 || len(replicated)+len(hashKey) != 33 {
		t.Fatalf("table golden coverage=%d catalog=%d", len(replicated)+len(hashKey), tableCount)
	}
}

func TestCentralizedDatasetContainsNoDistributeClause(t *testing.T) {
	dialect := DatasetDialectFor(Environment{Topology: TopologyCentralized})
	if ddl := strings.Join(dialect.TableDDL("gsbench"), "\n"); strings.Contains(ddl, "DISTRIBUTE BY") {
		t.Fatalf("centralized DDL contains DISTRIBUTE BY:\n%s", ddl)
	}
}

func TestOpenGaussDatasetContainsNoDistributeClause(t *testing.T) {
	dialect := DatasetDialectFor(Environment{
		Product:  ProductOpenGauss,
		Topology: TopologyStandalone,
	})
	if ddl := strings.Join(dialect.TableDDL("gsbench"), "\n"); strings.Contains(ddl, "DISTRIBUTE BY") {
		t.Fatalf("openGauss DDL contains DISTRIBUTE BY:\n%s", ddl)
	}
}

func TestDatasetDialectQuotesValidatedSchemaIdentifiers(t *testing.T) {
	dialect := DatasetDialectFor(testDatasetEnvironment())
	ddl := strings.Join(dialect.TableDDL("Bench"), "\n")
	if !strings.Contains(ddl, `CREATE TABLE "Bench".meta_dataset`) ||
		!strings.Contains(ddl, `ON "Bench".accounts`) {
		t.Fatalf("mixed-case schema is not quoted:\n%s", ddl)
	}
	batch := datasetBatchByName(t, dialect.BatchPlans("select", 5<<30), "customers")
	if !strings.Contains(batch.InsertSQL, `INSERT INTO "select".customers`) {
		t.Fatalf("reserved schema is not quoted:\n%s", batch.InsertSQL)
	}
	if got := dialect.TableDDL(`unsafe-name`); len(got) != 0 {
		t.Fatalf("unsafe schema emitted DDL: %v", got)
	}
}

func TestDatasetBatchPlansContainEveryPublicInitObject(t *testing.T) {
	batches := DatasetDialectFor(Environment{}).BatchPlans("gsbench", 100<<30)
	got := make(map[string]bool, len(batches))
	for _, batch := range batches {
		got[batch.Table] = true
	}
	for _, table := range []string{
		"customers", "accounts", "orders", "order_items", "dim_product", "dim_store",
		"fact_sales", "sort_data", "global_cache_targets", "plan_data", "hardparse_targets",
		"lock_targets", "lock_table_targets", "lock_ddl_targets", "lock_mode_targets",
		"ddl_global_1", "ddl_global_2", "ddl_global_3", "ddl_global_4", "dist_lock_targets",
		"replication_targets", "replication_conflict_targets", "dist_join_data",
		"dist_small_hash", "dist_txn_targets", "vacuum_targets",
	} {
		if !got[table] {
			t.Errorf("batch plans missing %s", table)
		}
	}
	for _, delayed := range []string{"network_ingress", "cluster_skew_data"} {
		if got[delayed] {
			t.Errorf("%s must remain empty after public init", delayed)
		}
	}
}

func TestDatasetBatchPlansUseDesignedCapacityFormula(t *testing.T) {
	batches := DatasetDialectFor(Environment{}).BatchPlans("gsbench", 100<<30)
	fact := datasetBatchByName(t, batches, "fact_sales")
	if fact.Rows != 111_848_106 {
		t.Fatalf("fact_sales rows=%d want=111848106", fact.Rows)
	}
	if fact.BatchSize != 250_000 {
		t.Fatalf("fact_sales batch size=%d want=250000", fact.BatchSize)
	}
	vacuum := datasetBatchByName(t, batches, "vacuum_targets")
	if vacuum.BatchSize != 58_254 {
		t.Fatalf("vacuum_targets batch size=%d want=58254", vacuum.BatchSize)
	}
}

func TestEveryDatasetBatchMatchesGoldenFormulaAndInsertContract(t *testing.T) {
	const target = int64(100 << 30)
	batches := DatasetDialectFor(Environment{}).BatchPlans("Bench", target)
	type scalableContract struct {
		weight, rowBytes, minRows int64
	}
	scalable := map[string]scalableContract{
		"customers": {2, 320, 10_000}, "accounts": {6, 352, 100_000},
		"orders": {6, 160, 100_000}, "order_items": {7, 128, 200_000},
		"fact_sales": {20, 192, 500_000}, "sort_data": {8, 640, 100_000},
		"plan_data": {12, 640, 1_000_000}, "hardparse_targets": {3, 320, 100_000},
		"replication_targets":          {2, 320, 100_000},
		"replication_conflict_targets": {1, 320, 100_000},
		"dist_join_data":               {2, 192, 100_000}, "dist_small_hash": {1, 128, 10_000},
		"dist_txn_targets": {2, 128, 100_000}, "vacuum_targets": {5, 1_152, 100_000},
	}
	fixedRows := map[string]int64{
		"dim_product": 100_000, "dim_store": 10_000,
		"global_cache_targets": 10_000, "lock_targets": 10_000,
		"lock_table_targets": 1_000, "lock_ddl_targets": 1_000,
		"lock_mode_targets": 1_000, "ddl_global_1": 10_000,
		"ddl_global_2": 10_000, "ddl_global_3": 10_000,
		"ddl_global_4": 10_000, "dist_lock_targets": 100_000,
	}
	if len(batches) != len(scalable)+len(fixedRows) {
		t.Fatalf("batch count=%d golden=%d", len(batches), len(scalable)+len(fixedRows))
	}
	seen := map[string]bool{}
	for _, batch := range batches {
		if seen[batch.Table] {
			t.Fatalf("duplicate batch %s", batch.Table)
		}
		seen[batch.Table] = true
		if !strings.Contains(batch.InsertSQL, `INSERT INTO "Bench".`+batch.Table) ||
			!strings.Contains(batch.InsertSQL, "$1") ||
			!strings.Contains(batch.InsertSQL, "$2") {
			t.Errorf("%s insert contract=%q", batch.Table, batch.InsertSQL)
		}
		if batch.BatchSize != datasetBatchRows(batch.EstimatedRowBytes) {
			t.Errorf("%s batch size=%d", batch.Table, batch.BatchSize)
		}
		if contract, ok := scalable[batch.Table]; ok {
			if batch.WeightPercent != contract.weight ||
				batch.EstimatedRowBytes != contract.rowBytes ||
				batch.Rows != targetRows(
					target, contract.weight, contract.rowBytes, contract.minRows,
				) {
				t.Errorf("%s batch=%+v contract=%+v", batch.Table, batch, contract)
			}
			continue
		}
		if batch.Rows != fixedRows[batch.Table] || batch.WeightPercent != 0 {
			t.Errorf("%s fixed batch=%+v", batch.Table, batch)
		}
	}
}

func TestDatasetTargetBytesChangesRowsAndBatchCount(t *testing.T) {
	dialect := DatasetDialectFor(Environment{})
	small := datasetBatchByName(t, dialect.BatchPlans("gsbench", 1<<30), "fact_sales")
	large := datasetBatchByName(t, dialect.BatchPlans("gsbench", 100<<30), "fact_sales")
	if small.Rows != 1_118_481 {
		t.Fatalf("1GiB fact_sales rows=%d want=1118481", small.Rows)
	}
	if large.Rows <= small.Rows {
		t.Fatalf("target bytes did not increase rows: small=%d large=%d", small.Rows, large.Rows)
	}
	smallCount := (small.Rows + small.BatchSize - 1) / small.BatchSize
	largeCount := (large.Rows + large.BatchSize - 1) / large.BatchSize
	if smallCount != 5 || largeCount != 448 {
		t.Fatalf("batch counts small=%d want=5 large=%d want=448", smallCount, largeCount)
	}
}

func datasetBatchByName(t *testing.T, batches []TableBatch, name string) TableBatch {
	t.Helper()
	for _, batch := range batches {
		if batch.Table == name {
			return batch
		}
	}
	t.Fatalf("batch plan missing %s", name)
	return TableBatch{}
}
