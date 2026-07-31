package gsbench

import (
	"context"
	"fmt"
	"strings"
)

const datasetVersion = "4"

const datasetProspectiveSizeFactor int64 = 2

type Capacity struct {
	TotalBytes int64
	FreeBytes  int64
}

type DatasetPlan struct {
	Schema            string
	Profile           string
	Product           Product
	Topology          Topology
	EstimatedBytes    int64
	ReservedFreeBytes int64
	AvailableForData  int64
	DDL               []string
	Columns           []TableColumn
	Migrations        []TableMigration
	PostMigrationDDL  []string
	Batches           []TableBatch
	HighWater         map[string]int64
}

type TableBatch struct {
	Table             string
	WeightPercent     int64
	EstimatedRowBytes int64
	Rows              int64
	BatchSize         int64
	InsertSQL         string
}

type DatasetExecutor interface {
	Exec(ctx context.Context, query string, args ...any) error
	SchemaExists(ctx context.Context, schema string) (bool, error)
	BatchHighWater(ctx context.Context, table string) (int64, error)
	ColumnExists(ctx context.Context, schema, table, column string) (bool, error)
}

type DatasetCapacityChecker interface {
	CheckCapacity(ctx context.Context) error
}

type DatasetAtomicBatchExecutor interface {
	ApplyDatasetBatch(
		ctx context.Context,
		schema string,
		batch TableBatch,
		start, end int64,
		version string,
	) error
}

type DatasetSizeSample struct {
	TotalBytes int64
	Source     string
	NodeCount  int
}

type DatasetPhysicalInspector interface {
	DatasetSize(ctx context.Context, schema string) (DatasetSizeSample, error)
	ValidateDatasetLayout(ctx context.Context, plan DatasetPlan) error
}

type DatasetObjectKind string

const (
	DatasetObjectTable DatasetObjectKind = "table"
	DatasetObjectIndex DatasetObjectKind = "index"
)

type DatasetObject struct {
	Kind         DatasetObjectKind
	Name         string
	DDL          string
	Distribution string
}

type DatasetObjectCatalog interface {
	DatasetObjectExists(ctx context.Context, object DatasetObject) (bool, error)
	MigrateLegacyDatasetObject(ctx context.Context, object DatasetObject) error
	ValidateDatasetObject(ctx context.Context, object DatasetObject) error
}

type DatasetVersionCatalog interface {
	DatasetVersion(ctx context.Context, schema string) (string, error)
	RecordDatasetVersion(ctx context.Context, schema, version string) error
}

type DatasetPostMigrationCatalog interface {
	ConvergeDatasetObject(ctx context.Context, object DatasetObject) error
}

type DatasetProgress func(format string, args ...any)

type DatasetManager struct {
	exec     DatasetExecutor
	progress DatasetProgress
}

func NewDatasetManager(exec DatasetExecutor, progress ...DatasetProgress) *DatasetManager {
	manager := &DatasetManager{exec: exec}
	if len(progress) > 0 {
		manager.progress = progress[0]
	}
	return manager
}

func (m *DatasetManager) report(format string, args ...any) {
	if m.progress != nil {
		m.progress(format, args...)
	}
}

func PlanDataset(cfg BenchConfig, capacity Capacity, env Environment) (DatasetPlan, error) {
	if !identifierRE.MatchString(cfg.Data.Schema) {
		return DatasetPlan{}, fmt.Errorf("unsafe schema %q", cfg.Data.Schema)
	}
	if capacity.TotalBytes <= 0 || capacity.FreeBytes <= 0 || capacity.FreeBytes > capacity.TotalBytes {
		return DatasetPlan{}, fmt.Errorf("invalid disk capacity: %+v", capacity)
	}
	minFree := cfg.Data.MinFreeDiskPercent
	if minFree == 0 {
		minFree = 20
	}
	reserved := capacity.TotalBytes * int64(minFree) / 100
	available := capacity.FreeBytes - reserved
	profile := strings.ToLower(cfg.Run.Profile)
	if profile == "" {
		profile = "quick"
	}
	if profile != "quick" && profile != "stress" {
		return DatasetPlan{}, fmt.Errorf("unknown dataset profile %q", profile)
	}
	target := cfg.Data.TargetBytes
	if target <= 0 {
		requestedGB := int64(cfg.Data.MaxSizeGB)
		if requestedGB <= 0 {
			requestedGB = 5
			if profile == "stress" {
				requestedGB = 20
			}
		}
		target = requestedGB << 30
	}
	if target < 1<<30 {
		return DatasetPlan{}, fmt.Errorf("dataset target %d is below 1GB", target)
	}
	if target > maxDatasetBytes {
		return DatasetPlan{}, fmt.Errorf("dataset target %d exceeds 2TB", target)
	}
	if target > available {
		return DatasetPlan{}, fmt.Errorf(
			"dataset capacity rejected: target=%d free=%d reserved=%d safe_available=%d",
			target, capacity.FreeBytes, reserved, available,
		)
	}
	plan := DatasetPlan{
		Schema:            cfg.Data.Schema,
		Profile:           profile,
		Product:           env.Product,
		Topology:          env.Topology,
		EstimatedBytes:    target,
		ReservedFreeBytes: reserved,
		AvailableForData:  available,
		HighWater:         map[string]int64{},
	}
	dialect := DatasetDialectFor(env)
	for _, statement := range dialect.TableDDL(plan.Schema) {
		if isPlanDataIndexDDL(statement) {
			plan.PostMigrationDDL = append(plan.PostMigrationDDL, statement)
			continue
		}
		plan.DDL = append(plan.DDL, statement)
	}
	plan.Columns = planDataColumns()
	plan.Migrations = dialect.Migrations(plan.Schema)
	plan.Batches = dialect.BatchPlans(plan.Schema, target)
	return plan, nil
}

func LoadDatasetHighWater(
	ctx context.Context,
	exec DatasetExecutor,
	plan *DatasetPlan,
) error {
	if plan == nil {
		return fmt.Errorf("dataset plan is required")
	}
	plan.HighWater = make(map[string]int64, len(plan.Batches))
	exists, err := exec.SchemaExists(ctx, plan.Schema)
	if err != nil {
		return fmt.Errorf("inspect dataset schema for high-water marks: %w", err)
	}
	if !exists {
		return nil
	}
	if catalog, ok := exec.(DatasetObjectCatalog); ok {
		exists, err := catalog.DatasetObjectExists(ctx, DatasetObject{
			Kind: DatasetObjectTable,
			Name: "meta_batches",
		})
		if err != nil {
			return fmt.Errorf("inspect meta_batches for high-water marks: %w", err)
		}
		if !exists {
			return nil
		}
	}
	for _, batch := range plan.Batches {
		high, err := exec.BatchHighWater(ctx, batch.Table)
		if err != nil {
			return fmt.Errorf("read %s high-water mark: %w", batch.Table, err)
		}
		plan.HighWater[batch.Table] = high
	}
	return nil
}

func datasetBatches(schema string, targetBytes int64) []TableBatch {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil
	}
	schema = quotedSchema
	scalable := func(table string, weight, rowBytes, minRows int64, insertSQL string) TableBatch {
		rows := targetBytes * weight / 100 / rowBytes
		if rows < minRows {
			rows = minRows
		}
		return TableBatch{
			Table:             table,
			WeightPercent:     weight,
			EstimatedRowBytes: rowBytes,
			Rows:              rows,
			BatchSize:         datasetBatchRows(rowBytes),
			InsertSQL:         insertSQL,
		}
	}
	fixed := func(table string, rows, rowBytes int64, insertSQL string) TableBatch {
		return TableBatch{
			Table:             table,
			EstimatedRowBytes: rowBytes,
			Rows:              rows,
			BatchSize:         datasetBatchRows(rowBytes),
			InsertSQL:         insertSQL,
		}
	}
	customers := targetRows(targetBytes, 2, 320, 10_000)
	orders := targetRows(targetBytes, 6, 160, 100_000)
	return []TableBatch{
		scalable("customers", 2, 320, 10_000, fmt.Sprintf(`INSERT INTO %s.customers
			(dist_key,id,region_id,name,payload)
			SELECT g,g,mod(g,100),'customer-' || g,repeat('c',128)
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("accounts", 6, 352, 100_000, fmt.Sprintf(`INSERT INTO %s.accounts
			(dist_key,id,customer_id,balance,payload,updated_at)
			SELECT mod(g,%d)+1,g,mod(g,%d)+1,1000,repeat('a',128),current_timestamp
			FROM generate_series($1,$2) AS g`, schema, customers, customers)),
		scalable("orders", 6, 160, 100_000, fmt.Sprintf(`INSERT INTO %s.orders
			(dist_key,id,customer_id,status,amount,created_at)
			SELECT mod(g,%d)+1,g,mod(g,%d)+1,mod(g,5),mod(g,10000),
			current_date-(mod(g,365)::integer)
			FROM generate_series($1,$2) AS g`, schema, customers, customers)),
		scalable("order_items", 7, 128, 200_000, fmt.Sprintf(`INSERT INTO %s.order_items
			(dist_key,id,order_id,product_id,quantity,amount)
			SELECT mod(g,%d)+1,g,mod(g,%d)+1,mod(g,100000)+1,mod(g,10)+1,mod(g,5000)
			FROM generate_series($1,$2) AS g`, schema, orders, orders)),
		fixed("dim_product", 100_000, 320, fmt.Sprintf(`INSERT INTO %s.dim_product
			(id,category_id,name,payload)
			SELECT g,mod(g,1000),'product-' || g,repeat('p',128)
			FROM generate_series($1,$2) AS g`, schema)),
		fixed("dim_store", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.dim_store
			(id,region_id,name)
			SELECT g,mod(g,100),'store-' || g
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("fact_sales", 20, 192, 500_000, fmt.Sprintf(`INSERT INTO %s.fact_sales
			(dist_key,id,sale_date,customer_id,product_id,store_id,amount,quantity,payload)
			SELECT mod(g,1048576)+1,g,current_date-(mod(g,730)::integer),
			mod(g,%d)+1,mod(g,100000)+1,mod(g,10000)+1,
			mod(g,100000)/100.0,mod(g,20)+1,repeat('f',96)
			FROM generate_series($1,$2) AS g`, schema, customers)),
		scalable("sort_data", 8, 640, 100_000, fmt.Sprintf(`INSERT INTO %s.sort_data
			(dist_key,id,group_id,sort_key,payload)
			SELECT mod(g,1048576)+1,g,mod(g,4096),mod(g*7919,2147483647),
			repeat(chr(65+mod(g,26)::integer),512)
			FROM generate_series($1,$2) AS g`, schema)),
		fixed("global_cache_targets", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.global_cache_targets
			(dist_key,id,value)
			SELECT g,g,'cache-' || g FROM generate_series($1,$2) AS g`, schema)),
		planDataBatch(schema, targetBytes),
		scalable("hardparse_targets", 3, 320, 100_000, fmt.Sprintf(`INSERT INTO %s.hardparse_targets
			(lookup_key,id,value,payload)
			SELECT mod(g,1000000)+1,g,mod(g,10000),repeat('h',192)
			FROM generate_series($1,$2) AS g`, schema)),
		fixed("lock_targets", 10_000, 320, fmt.Sprintf(`INSERT INTO %s.lock_targets
			(dist_key,id,value,payload)
			SELECT g,g,0,repeat('l',128) FROM generate_series($1,$2) AS g`, schema)),
		fixed("lock_table_targets", 1_000, 320, fmt.Sprintf(`INSERT INTO %s.lock_table_targets
			(dist_key,id,value,payload)
			SELECT g,g,0,repeat('t',128) FROM generate_series($1,$2) AS g`, schema)),
		fixed("lock_ddl_targets", 1_000, 320, fmt.Sprintf(`INSERT INTO %s.lock_ddl_targets
			(dist_key,id,value,payload)
			SELECT g,g,0,repeat('d',128) FROM generate_series($1,$2) AS g`, schema)),
		fixed("lock_mode_targets", 1_000, 128, fmt.Sprintf(`INSERT INTO %s.lock_mode_targets
			(dist_key,id,value)
			SELECT g,g,0 FROM generate_series($1,$2) AS g`, schema)),
		fixed("ddl_global_1", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.ddl_global_1
			(dist_key,id,value)
			SELECT g,g,0 FROM generate_series($1,$2) AS g`, schema)),
		fixed("ddl_global_2", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.ddl_global_2
			(dist_key,id,value)
			SELECT g,g,0 FROM generate_series($1,$2) AS g`, schema)),
		fixed("ddl_global_3", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.ddl_global_3
			(dist_key,id,value)
			SELECT g,g,0 FROM generate_series($1,$2) AS g`, schema)),
		fixed("ddl_global_4", 10_000, 128, fmt.Sprintf(`INSERT INTO %s.ddl_global_4
			(dist_key,id,value)
			SELECT g,g,0 FROM generate_series($1,$2) AS g`, schema)),
		fixed("dist_lock_targets", 100_000, 320, fmt.Sprintf(`INSERT INTO %s.dist_lock_targets
			(dist_key,id,value,payload)
			SELECT mod(g,1048576)+1,g,0,repeat('x',128)
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("replication_targets", 2, 320, 100_000, fmt.Sprintf(`INSERT INTO %s.replication_targets
			(dist_key,id,version,payload,updated_at)
			SELECT mod(g,1048576)+1,g,0,repeat('r',192),current_timestamp
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("replication_conflict_targets", 1, 320, 100_000, fmt.Sprintf(`INSERT INTO %s.replication_conflict_targets
			(dist_key,run_id,id,payload,created_at)
			SELECT mod(g,1048576)+1,'baseline',g,repeat('c',192),current_timestamp
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("dist_join_data", 2, 192, 100_000, fmt.Sprintf(`INSERT INTO %s.dist_join_data
			(dist_key,id,join_key,value,payload)
			SELECT mod(g*17,1048576)+1,g,mod(g,%d)+1,mod(g,10000),repeat('j',96)
			FROM generate_series($1,$2) AS g`, schema, customers)),
		scalable("dist_small_hash", 1, 128, 10_000, fmt.Sprintf(`INSERT INTO %s.dist_small_hash
			(dist_key,id,product_id,category_id,payload)
			SELECT mod(g*31,1048576)+1,g,mod(g,100000)+1,mod(g,1000),repeat('b',48)
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("dist_txn_targets", 2, 128, 100_000, fmt.Sprintf(`INSERT INTO %s.dist_txn_targets
			(dist_key,id,value,payload)
			SELECT mod(g,1048576)+1,g,0,repeat('z',48)
			FROM generate_series($1,$2) AS g`, schema)),
		scalable("vacuum_targets", 5, 1_152, 100_000, fmt.Sprintf(`INSERT INTO %s.vacuum_targets
			(dist_key,id,group_id,version,payload,updated_at)
			SELECT mod(g,1048576)+1,g,mod(g,1000),0,repeat('v',900),current_timestamp
			FROM generate_series($1,$2) AS g`, schema)),
	}
}

func targetRows(targetBytes, weight, rowBytes, minRows int64) int64 {
	rows := targetBytes * weight / 100 / rowBytes
	if rows < minRows {
		return minRows
	}
	return rows
}

func datasetBatchRows(rowBytes int64) int64 {
	rows := int64(64<<20) / rowBytes
	if rows < 10_000 {
		return 10_000
	}
	if rows > 250_000 {
		return 250_000
	}
	return rows
}

func (m *DatasetManager) Init(ctx context.Context, plan DatasetPlan) error {
	quotedSchema, ok := quoteDatasetSchema(plan.Schema)
	if !ok {
		return fmt.Errorf("unsafe schema %q", plan.Schema)
	}
	if err := m.ensureSchema(ctx, plan.Schema); err != nil {
		return err
	}
	versions, hasVersions := m.exec.(DatasetVersionCatalog)
	ordinaryDDL := plan.DDL
	if hasVersions {
		var err error
		ordinaryDDL, err = m.bootstrapDatasetVersion(
			ctx, plan.Schema, plan.DDL, versions,
		)
		if err != nil {
			return err
		}
	}
	if err := m.ensureDatasetObjects(ctx, ordinaryDDL, DatasetObjectTable); err != nil {
		return err
	}
	if !hasVersions {
		versions = nil
	}
	for _, column := range plan.Columns {
		exists, err := m.exec.ColumnExists(ctx, plan.Schema, column.Table, column.Name)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", column.Table, column.Name, err)
		}
		if exists {
			continue
		}
		statement := "ALTER TABLE " + quotedSchema + "." + column.Table +
			" ADD COLUMN " + column.Name + " " + column.Declaration
		if err := m.exec.Exec(ctx, statement); err != nil {
			return fmt.Errorf("add column %s.%s: %w", column.Table, column.Name, err)
		}
	}
	if err := m.migrate(ctx, plan); err != nil {
		return err
	}
	if catalog, ok := m.exec.(DatasetPostMigrationCatalog); ok {
		for _, statement := range plan.DDL {
			object, err := parseDatasetObject(statement)
			if err != nil {
				return err
			}
			if object.Kind != DatasetObjectTable {
				continue
			}
			if err := catalog.ConvergeDatasetObject(ctx, object); err != nil {
				return fmt.Errorf("converge dataset table %s: %w", object.Name, err)
			}
		}
	}
	if err := m.validateDatasetObjects(ctx, plan.DDL, DatasetObjectTable); err != nil {
		return err
	}
	if err := m.ensureDatasetObjects(ctx, ordinaryDDL, DatasetObjectIndex); err != nil {
		return err
	}
	if err := m.validateDatasetObjects(ctx, plan.DDL, DatasetObjectIndex); err != nil {
		return err
	}
	if err := m.ensureDatasetObjects(
		ctx, plan.PostMigrationDDL, DatasetObjectIndex,
	); err != nil {
		return fmt.Errorf("initialize post-migration DDL: %w", err)
	}
	if err := m.validateDatasetObjects(
		ctx, plan.PostMigrationDDL, DatasetObjectIndex,
	); err != nil {
		return fmt.Errorf("validate post-migration DDL: %w", err)
	}
	inspector, inspectPhysical := m.exec.(DatasetPhysicalInspector)
	var physical DatasetSizeSample
	if inspectPhysical {
		var err error
		physical, err = inspector.DatasetSize(ctx, plan.Schema)
		if err != nil {
			return fmt.Errorf("sample dataset physical size: %w", err)
		}
		if physical.Source == "" {
			return fmt.Errorf("sample dataset physical size: empty size source")
		}
		if err := enforceDatasetHardTarget(
			physical, plan.EstimatedBytes, "initial sample",
		); err != nil {
			return err
		}
		m.report("dataset size_bytes=%d size_source=%s nodes=%d",
			physical.TotalBytes, physical.Source, physical.NodeCount)
	}
	stopOptional := inspectPhysical &&
		physical.TotalBytes*100 >= plan.EstimatedBytes*95
	batches := plan.Batches
	if inspectPhysical {
		batches = make([]TableBatch, 0, len(plan.Batches))
		for _, table := range plan.Batches {
			if table.WeightPercent == 0 {
				batches = append(batches, table)
			}
		}
		for _, table := range plan.Batches {
			if table.WeightPercent > 0 {
				batches = append(batches, table)
			}
		}
	}
	for _, table := range batches {
		high, err := m.exec.BatchHighWater(ctx, table.Table)
		if err != nil {
			return fmt.Errorf("read %s high-water mark: %w", table.Table, err)
		}
		if high >= table.Rows {
			if high > table.Rows {
				m.report(
					"dataset table=%s high_water=%d target_rows=%d action=no_shrink",
					table.Table, high, table.Rows,
				)
			}
			continue
		}
		if table.WeightPercent > 0 && stopOptional {
			m.report(
				"dataset table=%s action=stop_optional size_bytes=%d target_bytes=%d",
				table.Table, physical.TotalBytes, plan.EstimatedBytes,
			)
			continue
		}
		start := high + 1
		for start <= table.Rows {
			if inspectPhysical {
				if physical.TotalBytes >= plan.EstimatedBytes ||
					(table.WeightPercent > 0 &&
						physical.TotalBytes*100 >= plan.EstimatedBytes*95) {
					stopOptional = true
					break
				}
			}
			end := table.Rows
			if table.BatchSize-1 < table.Rows-start {
				end = start + table.BatchSize - 1
			}
			if inspectPhysical && table.EstimatedRowBytes > 0 {
				rowsBeforeTarget := (plan.EstimatedBytes - physical.TotalBytes) /
					(table.EstimatedRowBytes * datasetProspectiveSizeFactor)
				if rowsBeforeTarget <= 0 {
					stopOptional = true
					break
				}
				if end-start+1 > rowsBeforeTarget {
					end = start + rowsBeforeTarget - 1
				}
			}
			if err := m.applyDatasetBatch(ctx, plan, table, start, end); err != nil {
				return err
			}
			if inspectPhysical {
				physical, err = inspector.DatasetSize(ctx, plan.Schema)
				if err != nil {
					return fmt.Errorf("sample dataset size after %s batch: %w",
						table.Table, err)
				}
				m.report("dataset table=%s size_bytes=%d size_source=%s",
					table.Table, physical.TotalBytes, physical.Source)
				if err := enforceDatasetHardTarget(
					physical,
					plan.EstimatedBytes,
					"post-batch "+table.Table,
				); err != nil {
					return err
				}
				if physical.TotalBytes >= plan.EstimatedBytes ||
					physical.TotalBytes*100 >= plan.EstimatedBytes*95 {
					stopOptional = true
				}
			}
			next, ok := nextDatasetBatchStart(end, table.Rows)
			if !ok {
				break
			}
			start = next
		}
	}
	if inspectPhysical {
		var err error
		physical, err = m.calibrateDataset(ctx, plan, inspector, physical)
		if err != nil {
			return err
		}
		if err := enforceDatasetHardTarget(
			physical, plan.EstimatedBytes, "post-calibration",
		); err != nil {
			return err
		}
	}
	for _, table := range plan.Batches {
		if err := m.exec.Exec(ctx, "ANALYZE "+quotedSchema+"."+table.Table); err != nil {
			return fmt.Errorf("analyze %s: %w", table.Table, err)
		}
	}
	if inspectPhysical {
		if err := inspector.ValidateDatasetLayout(ctx, plan); err != nil {
			return fmt.Errorf("validate dataset physical layout: %w", err)
		}
		m.report("dataset final_size_bytes=%d target_bytes=%d size_source=%s",
			physical.TotalBytes, plan.EstimatedBytes, physical.Source)
	}
	if hasVersions {
		if err := versions.RecordDatasetVersion(ctx, plan.Schema, datasetVersion); err != nil {
			return fmt.Errorf("record dataset version: %w", err)
		}
	}
	return nil
}

func nextDatasetBatchStart(committedEnd, targetRows int64) (int64, bool) {
	if committedEnd >= targetRows {
		return 0, false
	}
	return committedEnd + 1, true
}

func enforceDatasetHardTarget(
	sample DatasetSizeSample,
	target int64,
	stage string,
) error {
	if sample.TotalBytes <= target {
		return nil
	}
	return fmt.Errorf(
		"measured dataset overshoot at %s: measured_bytes=%d target_bytes=%d size_source=%s",
		stage, sample.TotalBytes, target, sample.Source,
	)
}

func (m *DatasetManager) bootstrapDatasetVersion(
	ctx context.Context,
	schema string,
	statements []string,
	versions DatasetVersionCatalog,
) ([]string, error) {
	catalog, ok := m.exec.(DatasetObjectCatalog)
	if !ok {
		return nil, fmt.Errorf(
			"dataset version catalog requires object catalog support")
	}
	metaIndex := -1
	var meta DatasetObject
	for i, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			return nil, err
		}
		if object.Kind == DatasetObjectTable && object.Name == "meta_dataset" {
			metaIndex = i
			meta = object
			break
		}
	}
	if metaIndex < 0 {
		return nil, fmt.Errorf("dataset plan has no meta_dataset bootstrap DDL")
	}
	exists, err := catalog.DatasetObjectExists(ctx, meta)
	if err != nil {
		return nil, fmt.Errorf("inspect bootstrap meta_dataset: %w", err)
	}
	if !exists {
		if err := m.exec.Exec(ctx, statements[metaIndex]); err != nil {
			return nil, fmt.Errorf("create bootstrap meta_dataset: %w", err)
		}
	}
	version, err := versions.DatasetVersion(ctx, schema)
	if err != nil {
		return nil, fmt.Errorf("read dataset version: %w", err)
	}
	switch version {
	case "", "1", "2", "3", datasetVersion:
	default:
		return nil, fmt.Errorf("unsupported dataset version %q", version)
	}
	if exists {
		return statements, nil
	}
	ordinary := make([]string, 0, len(statements)-1)
	ordinary = append(ordinary, statements[:metaIndex]...)
	ordinary = append(ordinary, statements[metaIndex+1:]...)
	return ordinary, nil
}

func (m *DatasetManager) applyDatasetBatch(
	ctx context.Context,
	plan DatasetPlan,
	table TableBatch,
	start, end int64,
) error {
	if checker, ok := m.exec.(DatasetCapacityChecker); ok {
		if err := checker.CheckCapacity(ctx); err != nil {
			return fmt.Errorf("dataset disk safety check: %w", err)
		}
	}
	if atomic, ok := m.exec.(DatasetAtomicBatchExecutor); ok {
		if err := atomic.ApplyDatasetBatch(
			ctx, plan.Schema, table, start, end, datasetVersion,
		); err != nil {
			return fmt.Errorf("populate %s rows %d-%d atomically: %w",
				table.Table, start, end, err)
		}
		return nil
	}
	if err := m.exec.Exec(ctx, table.InsertSQL, start, end); err != nil {
		return fmt.Errorf("populate %s rows %d-%d: %w", table.Table, start, end, err)
	}
	if err := m.recordHighWater(
		ctx,
		plan.Schema,
		table.Table,
		end,
		table.Rows,
		table.EstimatedRowBytes,
	); err != nil {
		return err
	}
	return nil
}

func (m *DatasetManager) calibrateDataset(
	ctx context.Context,
	plan DatasetPlan,
	inspector DatasetPhysicalInspector,
	physical DatasetSizeSample,
) (DatasetSizeSample, error) {
	for round := 1; round <= 3 &&
		physical.TotalBytes*100 < plan.EstimatedBytes*90; round++ {
		deficit := plan.EstimatedBytes*95/100 - physical.TotalBytes
		totalWeight := int64(0)
		for _, table := range plan.Batches {
			if table.WeightPercent > 0 {
				totalWeight += table.WeightPercent
			}
		}
		if deficit <= 0 || totalWeight == 0 {
			break
		}
		m.report("dataset calibration_round=%d deficit_bytes=%d", round, deficit)
		for _, table := range plan.Batches {
			if table.WeightPercent <= 0 {
				continue
			}
			if physical.TotalBytes >= plan.EstimatedBytes ||
				physical.TotalBytes*100 >= plan.EstimatedBytes*95 {
				break
			}
			high, err := m.exec.BatchHighWater(ctx, table.Table)
			if err != nil {
				return physical, fmt.Errorf(
					"read %s calibration high-water mark: %w", table.Table, err)
			}
			rowDeficit := deficit * table.WeightPercent /
				totalWeight / table.EstimatedRowBytes
			if rowDeficit < 1 {
				rowDeficit = 1
			}
			if rowDeficit > table.BatchSize {
				rowDeficit = table.BatchSize
			}
			rowsBeforeTarget := (plan.EstimatedBytes - physical.TotalBytes) /
				(table.EstimatedRowBytes * datasetProspectiveSizeFactor)
			if rowsBeforeTarget <= 0 {
				break
			}
			if rowDeficit > rowsBeforeTarget {
				rowDeficit = rowsBeforeTarget
			}
			start := high + 1
			end := high + rowDeficit
			calibrationBatch := table
			calibrationBatch.Rows = end
			if err := m.applyDatasetBatch(
				ctx, plan, calibrationBatch, start, end,
			); err != nil {
				return physical, fmt.Errorf("calibration round %d: %w", round, err)
			}
			physical, err = inspector.DatasetSize(ctx, plan.Schema)
			if err != nil {
				return physical, fmt.Errorf(
					"sample dataset size in calibration round %d: %w", round, err)
			}
			if err := enforceDatasetHardTarget(
				physical,
				plan.EstimatedBytes,
				fmt.Sprintf("calibration round %d table %s", round, table.Table),
			); err != nil {
				return physical, err
			}
		}
	}
	return physical, nil
}

func (m *DatasetManager) ensureDatasetObjects(
	ctx context.Context,
	statements []string,
	kind DatasetObjectKind,
) error {
	catalog, hasCatalog := m.exec.(DatasetObjectCatalog)
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			return err
		}
		if object.Kind != kind {
			continue
		}
		if !hasCatalog {
			if err := m.exec.Exec(ctx, statement); err != nil {
				return fmt.Errorf("create dataset object: %w", err)
			}
			continue
		}
		exists, err := catalog.DatasetObjectExists(ctx, object)
		if err != nil {
			return fmt.Errorf("inspect dataset %s %s: %w", object.Kind, object.Name, err)
		}
		if exists {
			if object.Kind == DatasetObjectTable {
				if err := catalog.MigrateLegacyDatasetObject(ctx, object); err != nil {
					return fmt.Errorf("migrate legacy table %s: %w", object.Name, err)
				}
			}
		} else if err := m.exec.Exec(ctx, object.DDL); err != nil {
			return fmt.Errorf("create dataset %s %s: %w", object.Kind, object.Name, err)
		}
	}
	return nil
}

func (m *DatasetManager) validateDatasetObjects(
	ctx context.Context,
	statements []string,
	kind DatasetObjectKind,
) error {
	catalog, ok := m.exec.(DatasetObjectCatalog)
	if !ok {
		return nil
	}
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			return err
		}
		if object.Kind != kind {
			continue
		}
		if err := catalog.ValidateDatasetObject(ctx, object); err != nil {
			if object.Kind == DatasetObjectTable {
				return fmt.Errorf("validate dataset table %s: %w", object.Name, err)
			}
			return fmt.Errorf("validate dataset %s %s: %w", object.Kind, object.Name, err)
		}
	}
	return nil
}

func parseDatasetObject(statement string) (DatasetObject, error) {
	fields := strings.Fields(statement)
	if len(fields) < 3 || fields[0] != "CREATE" {
		return DatasetObject{}, fmt.Errorf("unsupported dataset DDL %q", statement)
	}
	switch fields[1] {
	case "TABLE":
		qualified := fields[2]
		dot := strings.LastIndex(qualified, ".")
		if dot < 0 || dot == len(qualified)-1 {
			return DatasetObject{}, fmt.Errorf("invalid dataset table DDL %q", statement)
		}
		distribution := ""
		if marker := strings.LastIndex(statement, " DISTRIBUTE BY "); marker >= 0 {
			distribution = strings.TrimSpace(statement[marker+len(" DISTRIBUTE BY "):])
		}
		return DatasetObject{
			Kind:         DatasetObjectTable,
			Name:         strings.Trim(qualified[dot+1:], `"`),
			DDL:          statement,
			Distribution: distribution,
		}, nil
	case "INDEX":
		if len(fields) < 5 || fields[3] != "ON" {
			return DatasetObject{}, fmt.Errorf("invalid dataset index DDL %q", statement)
		}
		return DatasetObject{
			Kind: DatasetObjectIndex,
			Name: strings.Trim(fields[2], `"`),
			DDL:  statement,
		}, nil
	default:
		return DatasetObject{}, fmt.Errorf("unsupported dataset DDL %q", statement)
	}
}

func (m *DatasetManager) ensureSchema(ctx context.Context, schema string) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe schema %q", schema)
	}
	exists, err := m.exec.SchemaExists(ctx, schema)
	if err != nil {
		return fmt.Errorf("check schema %s: %w", schema, err)
	}
	if exists {
		return nil
	}
	if err := m.exec.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		exists, checkErr := m.exec.SchemaExists(ctx, schema)
		if checkErr == nil && exists {
			return nil
		}
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	return nil
}

func (m *DatasetManager) migrate(ctx context.Context, plan DatasetPlan) error {
	for _, migration := range plan.Migrations {
		end, err := m.exec.BatchHighWater(ctx, migration.SourceTable)
		if err != nil {
			return fmt.Errorf("read %s migration source: %w", migration.Name, err)
		}
		high, err := m.exec.BatchHighWater(ctx, migration.Name)
		if err != nil {
			return fmt.Errorf("read %s migration high-water: %w", migration.Name, err)
		}
		for start := high + 1; start <= end; start += migration.BatchSize {
			if checker, ok := m.exec.(DatasetCapacityChecker); ok {
				if err := checker.CheckCapacity(ctx); err != nil {
					return fmt.Errorf("dataset disk safety check: %w", err)
				}
			}
			batchEnd := min(end, start+migration.BatchSize-1)
			if err := m.exec.Exec(ctx, migration.UpdateSQL, start, batchEnd); err != nil {
				return fmt.Errorf("migrate %s rows %d-%d: %w", migration.Name, start, batchEnd, err)
			}
			if err := m.recordHighWater(
				ctx,
				plan.Schema,
				migration.Name,
				batchEnd,
				end,
				0,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *DatasetManager) recordHighWater(
	ctx context.Context,
	schema, table string,
	high, targetRows, estimatedRowBytes int64,
) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe schema %q", schema)
	}
	if err := m.exec.Exec(ctx, "DELETE FROM "+quotedSchema+".meta_batches WHERE table_name=$1", table); err != nil {
		return fmt.Errorf("replace %s high-water mark: %w", table, err)
	}
	if err := m.exec.Exec(
		ctx,
		"INSERT INTO "+quotedSchema+".meta_batches(table_name,high_water,target_rows,estimated_row_bytes,dataset_version) VALUES($1,$2,$3,$4,$5)",
		table,
		high,
		targetRows,
		estimatedRowBytes,
		datasetVersion,
	); err != nil {
		return fmt.Errorf("record %s high-water mark: %w", table, err)
	}
	return nil
}
