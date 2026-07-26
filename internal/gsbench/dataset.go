package gsbench

import (
	"context"
	"fmt"
	"strings"
)

const datasetVersion = "1"

type Capacity struct {
	TotalBytes int64
	FreeBytes  int64
}

type DatasetPlan struct {
	Schema            string
	Profile           string
	EstimatedBytes    int64
	ReservedFreeBytes int64
	AvailableForData  int64
	DDL               []string
	Batches           []TableBatch
}

type TableBatch struct {
	Table     string
	Rows      int64
	BatchSize int64
	InsertSQL string
}

type DatasetExecutor interface {
	Exec(ctx context.Context, query string, args ...any) error
	BatchHighWater(ctx context.Context, table string) (int64, error)
}

type DatasetCapacityChecker interface {
	CheckCapacity(ctx context.Context) error
}

type DatasetManager struct {
	exec DatasetExecutor
}

func NewDatasetManager(exec DatasetExecutor) *DatasetManager {
	return &DatasetManager{exec: exec}
}

func PlanDataset(cfg BenchConfig, capacity Capacity) (DatasetPlan, error) {
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
	if available <= 0 {
		return DatasetPlan{}, fmt.Errorf("free disk %d is below reserved safety space %d", capacity.FreeBytes, reserved)
	}
	profile := strings.ToLower(cfg.Run.Profile)
	if profile == "" {
		profile = "quick"
	}
	profileCapGB := int64(5)
	if profile == "stress" {
		profileCapGB = 20
	} else if profile != "quick" {
		return DatasetPlan{}, fmt.Errorf("unknown dataset profile %q", profile)
	}
	requestedGB := int64(cfg.Data.MaxSizeGB)
	if requestedGB <= 0 {
		requestedGB = profileCapGB
	}
	if requestedGB > profileCapGB {
		requestedGB = profileCapGB
	}
	target := requestedGB << 30
	if target > available {
		return DatasetPlan{}, fmt.Errorf("dataset requires %d bytes but only %d are safe to use", target, available)
	}
	plan := DatasetPlan{
		Schema:            cfg.Data.Schema,
		Profile:           profile,
		EstimatedBytes:    target,
		ReservedFreeBytes: reserved,
		AvailableForData:  available,
	}
	plan.DDL = datasetDDL(plan.Schema)
	plan.Batches = datasetBatches(plan.Schema, target)
	return plan, nil
}

func datasetDDL(schema string) []string {
	q := func(name string) string { return schema + "." + name }
	return []string{
		"CREATE SCHEMA IF NOT EXISTS " + schema,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (key varchar(128) PRIMARY KEY, value text NOT NULL, updated_at timestamp NOT NULL DEFAULT current_timestamp)`, q("meta_dataset")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (run_id varchar(96) PRIMARY KEY, scenarios text NOT NULL, phase varchar(32) NOT NULL, status varchar(32) NOT NULL, owner_name varchar(128) NOT NULL, started_at timestamp NOT NULL, updated_at timestamp NOT NULL, detail text)`, q("meta_runs")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigserial PRIMARY KEY, run_id varchar(96) NOT NULL, scenario varchar(64) NOT NULL, kind varchar(64), target text NOT NULL, original_value text, forward_sql text NOT NULL, inverse_sql text NOT NULL, verify_sql text, verify_value text, state varchar(32) NOT NULL, error_text text, created_at timestamp NOT NULL DEFAULT current_timestamp, updated_at timestamp NOT NULL DEFAULT current_timestamp)`, q("meta_journal")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (table_name varchar(128) PRIMARY KEY, high_water bigint NOT NULL, dataset_version varchar(32) NOT NULL, updated_at timestamp NOT NULL DEFAULT current_timestamp)`, q("meta_batches")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, customer_id bigint NOT NULL, balance numeric(18,2) NOT NULL, payload varchar(256), updated_at timestamp NOT NULL)`, q("accounts")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, region_id int NOT NULL, name varchar(96), payload varchar(256))`, q("customers")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, customer_id bigint NOT NULL, status int NOT NULL, amount numeric(18,2) NOT NULL, created_at timestamp NOT NULL)`, q("orders")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, order_id bigint NOT NULL, product_id bigint NOT NULL, quantity int NOT NULL, amount numeric(18,2) NOT NULL)`, q("order_items")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id int PRIMARY KEY, category_id int NOT NULL, name varchar(96), payload varchar(256))`, q("dim_product")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id int PRIMARY KEY, region_id int NOT NULL, name varchar(96))`, q("dim_store")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint NOT NULL, sale_date date NOT NULL, customer_id bigint NOT NULL, product_id int NOT NULL, store_id int NOT NULL, amount numeric(18,2) NOT NULL, quantity int NOT NULL, payload varchar(256))`, q("fact_sales")),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS fact_sales_product_idx ON %s (product_id)`, q("fact_sales")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, lookup_key bigint NOT NULL, skew_key int NOT NULL, payload varchar(512))`, q("plan_data")),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS plan_data_lookup_idx ON %s (lookup_key)`, q("plan_data")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, value bigint NOT NULL, payload varchar(256))`, q("lock_targets")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, value bigint NOT NULL, payload varchar(256))`, q("lock_table_targets")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, value bigint NOT NULL, payload varchar(256))`, q("lock_ddl_targets")),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY, group_id int NOT NULL, version bigint NOT NULL, payload varchar(1024), updated_at timestamp NOT NULL)`, q("vacuum_targets")),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS vacuum_targets_group_idx ON %s (group_id)`, q("vacuum_targets")),
	}
}

func datasetBatches(schema string, targetBytes int64) []TableBatch {
	rows := func(weight, rowBytes int64) int64 {
		value := targetBytes * weight / 100 / rowBytes
		if value < 1000 {
			return 1000
		}
		return value
	}
	batch := int64(250_000)
	customers := rows(4, 320)
	orders := rows(8, 128)
	return []TableBatch{
		{"customers", customers, batch, fmt.Sprintf(`INSERT INTO %s.customers SELECT g, mod(g,100), 'customer-' || g, repeat('c',128) FROM generate_series($1,$2) AS g`, schema)},
		{"accounts", rows(12, 320), batch, fmt.Sprintf(`INSERT INTO %s.accounts SELECT g, mod(g,%d)+1, 1000, repeat('a',128), current_timestamp FROM generate_series($1,$2) AS g`, schema, customers)},
		{"orders", orders, batch, fmt.Sprintf(`INSERT INTO %s.orders SELECT g, mod(g,%d)+1, mod(g,5), mod(g,10000), current_timestamp - mod(g,365) FROM generate_series($1,$2) AS g`, schema, customers)},
		{"order_items", rows(10, 96), batch, fmt.Sprintf(`INSERT INTO %s.order_items SELECT g, mod(g,%d)+1, mod(g,100000)+1, mod(g,10)+1, mod(g,5000) FROM generate_series($1,$2) AS g`, schema, orders)},
		{"dim_product", 100_000, batch, fmt.Sprintf(`INSERT INTO %s.dim_product SELECT g, mod(g,1000), 'product-' || g, repeat('p',128) FROM generate_series($1,$2) AS g`, schema)},
		{"dim_store", 10_000, batch, fmt.Sprintf(`INSERT INTO %s.dim_store SELECT g, mod(g,100), 'store-' || g FROM generate_series($1,$2) AS g`, schema)},
		{"fact_sales", rows(35, 160), batch, fmt.Sprintf(`INSERT INTO %s.fact_sales SELECT g, current_date - mod(g,730), mod(g,%d)+1, mod(g,100000)+1, mod(g,10000)+1, mod(g,100000)/100.0, mod(g,20)+1, repeat('f',96) FROM generate_series($1,$2) AS g`, schema, customers)},
		{"plan_data", rows(8, 600), batch, fmt.Sprintf(`INSERT INTO %s.plan_data SELECT g, g, CASE WHEN mod(g,100)<95 THEN 1 ELSE mod(g,1000) END, repeat('s',400) FROM generate_series($1,$2) AS g`, schema)},
		{"lock_targets", 1000, batch, fmt.Sprintf(`INSERT INTO %s.lock_targets SELECT g, 0, repeat('l',128) FROM generate_series($1,$2) AS g`, schema)},
		{"lock_table_targets", 1000, batch, fmt.Sprintf(`INSERT INTO %s.lock_table_targets SELECT g, 0, repeat('t',128) FROM generate_series($1,$2) AS g`, schema)},
		{"lock_ddl_targets", 1000, batch, fmt.Sprintf(`INSERT INTO %s.lock_ddl_targets SELECT g, 0, repeat('d',128) FROM generate_series($1,$2) AS g`, schema)},
		{"vacuum_targets", rows(20, 1100), batch, fmt.Sprintf(`INSERT INTO %s.vacuum_targets SELECT g, mod(g,1000), 0, repeat('v',900), current_timestamp FROM generate_series($1,$2) AS g`, schema)},
	}
}

func (m *DatasetManager) Init(ctx context.Context, plan DatasetPlan) error {
	for _, statement := range plan.DDL {
		if err := m.exec.Exec(ctx, statement); err != nil {
			return fmt.Errorf("initialize dataset DDL: %w", err)
		}
	}
	for _, table := range plan.Batches {
		high, err := m.exec.BatchHighWater(ctx, table.Table)
		if err != nil {
			return fmt.Errorf("read %s high-water mark: %w", table.Table, err)
		}
		if high >= table.Rows {
			continue
		}
		for start := high + 1; start <= table.Rows; start += table.BatchSize {
			if checker, ok := m.exec.(DatasetCapacityChecker); ok {
				if err := checker.CheckCapacity(ctx); err != nil {
					return fmt.Errorf("dataset disk safety check: %w", err)
				}
			}
			end := start + table.BatchSize - 1
			if end > table.Rows {
				end = table.Rows
			}
			if err := m.exec.Exec(ctx, table.InsertSQL, start, end); err != nil {
				return fmt.Errorf("populate %s rows %d-%d: %w", table.Table, start, end, err)
			}
			if err := m.recordHighWater(ctx, plan.Schema, table.Table, end); err != nil {
				return err
			}
		}
	}
	for _, table := range plan.Batches {
		if err := m.exec.Exec(ctx, "ANALYZE "+plan.Schema+"."+table.Table); err != nil {
			return fmt.Errorf("analyze %s: %w", table.Table, err)
		}
	}
	return nil
}

func (m *DatasetManager) recordHighWater(ctx context.Context, schema, table string, high int64) error {
	if err := m.exec.Exec(ctx, "DELETE FROM "+schema+".meta_batches WHERE table_name=$1", table); err != nil {
		return fmt.Errorf("replace %s high-water mark: %w", table, err)
	}
	if err := m.exec.Exec(ctx, "INSERT INTO "+schema+".meta_batches(table_name,high_water,dataset_version) VALUES($1,$2,$3)", table, high, datasetVersion); err != nil {
		return fmt.Errorf("record %s high-water mark: %w", table, err)
	}
	return nil
}
