package gsbench

import (
	"fmt"
	"strings"
)

type DatasetDialect interface {
	TableDDL(schema string) []string
	BatchPlans(schema string, targetBytes int64) []TableBatch
	Migrations(schema string) []TableMigration
}

type datasetDialect struct {
	distributed bool
}

func DatasetDialectFor(env Environment) DatasetDialect {
	return datasetDialect{
		distributed: env.Product == ProductGaussDB && env.Topology == TopologyDistributed,
	}
}

func (d datasetDialect) TableDDL(schema string) []string {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil
	}
	logical := logicalDatasetDDL(quotedSchema)
	out := make([]string, len(logical))
	for i, statement := range logical {
		out[i] = d.expand(statement)
	}
	return out
}

func (d datasetDialect) BatchPlans(schema string, targetBytes int64) []TableBatch {
	if _, ok := quoteDatasetSchema(schema); !ok {
		return nil
	}
	return datasetBatches(schema, targetBytes)
}

func (d datasetDialect) Migrations(schema string) []TableMigration {
	if _, ok := quoteDatasetSchema(schema); !ok {
		return nil
	}
	return planDataMigrations(schema)
}

func quoteDatasetSchema(schema string) (string, bool) {
	if len(schema) >= 2 && schema[0] == '"' && schema[len(schema)-1] == '"' {
		inner := schema[1 : len(schema)-1]
		if !identifierRE.MatchString(inner) {
			return "", false
		}
		return schema, true
	}
	if !identifierRE.MatchString(schema) {
		return "", false
	}
	return `"` + schema + `"`, true
}

func logicalDatasetDDL(schema string) []string {
	table := func(name, columns, distribution string) string {
		return fmt.Sprintf("CREATE TABLE %s.%s (\n%s\n) %s", schema, name, columns, distribution)
	}
	index := func(name, target, columns string) string {
		return fmt.Sprintf("CREATE INDEX %s ON %s.%s (%s)", name, schema, target, columns)
	}
	ddl := []string{
		table("meta_dataset", `	key varchar(128) PRIMARY KEY,
	value text NOT NULL,
	updated_at timestamp NOT NULL DEFAULT current_timestamp`, "@REPLICATION"),
		table("meta_runs", `	run_id varchar(96) NOT NULL,
	scenarios text NOT NULL,
	phase varchar(32) NOT NULL,
	status varchar(32) NOT NULL,
	owner_name varchar(128) NOT NULL,
	started_at timestamp NOT NULL,
	updated_at timestamp NOT NULL,
	detail text,
	PRIMARY KEY (run_id)`, "@HASH(run_id)"),
		table("meta_journal", `	run_id varchar(96) NOT NULL,
	action_id bigint NOT NULL,
	scenario_code integer NOT NULL,
	action_kind varchar(64) NOT NULL,
	target_product varchar(32) NOT NULL,
	target_node varchar(128),
	target_endpoint varchar(256),
	original_state text,
	forward_action text NOT NULL,
	inverse_action text NOT NULL,
	verify_action text,
	verify_value text,
	state varchar(32) NOT NULL,
	last_error text,
	created_at timestamp NOT NULL DEFAULT current_timestamp,
	updated_at timestamp NOT NULL DEFAULT current_timestamp,
	PRIMARY KEY (run_id, action_id)`, "@HASH(run_id)"),
		table("meta_batches", `	table_name varchar(128) NOT NULL,
	high_water bigint NOT NULL,
	target_rows bigint NOT NULL,
	estimated_row_bytes bigint NOT NULL,
	dataset_version varchar(32) NOT NULL,
	updated_at timestamp NOT NULL DEFAULT current_timestamp,
	PRIMARY KEY (table_name)`, "@HASH(table_name)"),
		table("meta_plan_cache", `	signature varchar(64) NOT NULL,
	scenario_code integer NOT NULL,
	sql_text text NOT NULL,
	plan_text text NOT NULL,
	updated_at timestamp NOT NULL DEFAULT current_timestamp,
	PRIMARY KEY (signature, scenario_code)`, "@HASH(signature)"),
		table("customers", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	region_id integer NOT NULL,
	name varchar(96),
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("accounts", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	customer_id bigint NOT NULL,
	balance numeric(18,2) NOT NULL,
	payload varchar(256),
	updated_at timestamp NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("orders", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	customer_id bigint NOT NULL,
	status integer NOT NULL,
	amount numeric(18,2) NOT NULL,
	created_at timestamp NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("order_items", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	order_id bigint NOT NULL,
	product_id integer NOT NULL,
	quantity integer NOT NULL,
	amount numeric(18,2) NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("dim_product", `	id integer PRIMARY KEY,
	category_id integer NOT NULL,
	name varchar(96),
	payload varchar(256)`, "@REPLICATION"),
		table("dim_store", `	id integer PRIMARY KEY,
	region_id integer NOT NULL,
	name varchar(96)`, "@REPLICATION"),
		table("fact_sales", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	sale_date date NOT NULL,
	customer_id bigint NOT NULL,
	product_id integer NOT NULL,
	store_id integer NOT NULL,
	amount numeric(18,2) NOT NULL,
	quantity integer NOT NULL,
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("sort_data", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	group_id integer NOT NULL,
	sort_key bigint NOT NULL,
	payload varchar(512),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("network_ingress", `	run_id varchar(96) NOT NULL,
	dist_key bigint NOT NULL,
	seq bigint NOT NULL,
	payload varchar(1024) NOT NULL,
	created_at timestamp NOT NULL DEFAULT current_timestamp,
	PRIMARY KEY (dist_key, run_id, seq)`, "@HASH(dist_key)"),
		table("global_cache_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value varchar(64),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("plan_data", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	lookup_key bigint NOT NULL,
	skew_key integer NOT NULL,
	stats_target_key integer NOT NULL,
	stats_ndistinct_key bigint NOT NULL,
	stats_corr_a integer NOT NULL,
	stats_corr_b integer NOT NULL,
	index_unusable_key bigint NOT NULL,
	index_drop_key bigint NOT NULL,
	index_shape_lead integer NOT NULL,
	index_shape_tail bigint NOT NULL,
	payload varchar(512),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("hardparse_targets", `	lookup_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (lookup_key, id)`, "@HASH(lookup_key)"),
		table("lock_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("lock_table_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("lock_ddl_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("lock_mode_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("ddl_global_1", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("ddl_global_2", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("ddl_global_3", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("ddl_global_4", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("dist_lock_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("replication_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	version bigint NOT NULL,
	payload varchar(256),
	updated_at timestamp NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("replication_conflict_targets", `	dist_key bigint NOT NULL,
	run_id varchar(96) NOT NULL,
	id bigint NOT NULL,
	payload varchar(256),
	created_at timestamp NOT NULL,
	PRIMARY KEY (dist_key, run_id, id)`, "@HASH(dist_key)"),
		table("cluster_skew_data", `	skew_key bigint NOT NULL,
	id bigint NOT NULL,
	payload varchar(256),
	PRIMARY KEY (skew_key, id)`, "@HASH(skew_key)"),
		table("dist_join_data", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	join_key bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(128),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("dist_small_hash", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	product_id integer NOT NULL,
	category_id integer NOT NULL,
	payload varchar(64),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("dist_txn_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	value bigint NOT NULL,
	payload varchar(64),
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
		table("vacuum_targets", `	dist_key bigint NOT NULL,
	id bigint NOT NULL,
	group_id integer NOT NULL,
	version bigint NOT NULL,
	payload varchar(1024),
	updated_at timestamp NOT NULL,
	PRIMARY KEY (dist_key, id)`, "@HASH(dist_key)"),
	}
	ddl = append(ddl,
		index("accounts_customer_idx", "accounts", "customer_id, dist_key, id"),
		index("orders_customer_idx", "orders", "customer_id, dist_key, id"),
		index("order_items_order_idx", "order_items", "order_id, dist_key, id"),
		index("fact_sales_product_idx", "fact_sales", "product_id, dist_key, id"),
		index("fact_sales_customer_idx", "fact_sales", "customer_id, dist_key, id"),
		index("sort_data_sort_idx", "sort_data", "sort_key, dist_key, id"),
	)
	ddl = append(ddl, planDataPostMigrationDDL(schema)...)
	return append(ddl,
		index("hardparse_targets_lookup_idx", "hardparse_targets", "lookup_key, id"),
		index("replication_conflict_run_idx", "replication_conflict_targets", "run_id, dist_key, id"),
		index("vacuum_targets_group_idx", "vacuum_targets", "group_id, dist_key, id"),
	)
}

func (d datasetDialect) expand(statement string) string {
	if !d.distributed {
		statement = strings.ReplaceAll(statement, " @REPLICATION", "")
		hashStart := strings.LastIndex(statement, " @HASH(")
		if hashStart >= 0 {
			statement = statement[:hashStart]
		}
		return statement
	}
	statement = strings.ReplaceAll(statement, " @REPLICATION", " DISTRIBUTE BY REPLICATION")
	hashStart := strings.LastIndex(statement, " @HASH(")
	if hashStart < 0 {
		return statement
	}
	columns := strings.TrimSuffix(statement[hashStart+len(" @HASH("):], ")")
	return statement[:hashStart] + " DISTRIBUTE BY HASH (" + columns + ")"
}
