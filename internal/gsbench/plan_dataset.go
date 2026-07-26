package gsbench

import (
	"fmt"
	"strings"
)

const (
	planDataWeight   int64 = 12
	planDataRowBytes int64 = 640
	planDataMinRows  int64 = 1_000_000
)

type TableMigration struct {
	Name        string
	SourceTable string
	BatchSize   int64
	UpdateSQL   string
}

type TableColumn struct {
	Table       string
	Name        string
	Declaration string
}

func planDataDDL(schema string) []string {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil
	}
	prefix := "CREATE TABLE " + quotedSchema + ".plan_data "
	for _, statement := range DatasetDialectFor(Environment{
		Product:  ProductOpenGauss,
		Topology: TopologyStandalone,
	}).TableDDL(schema) {
		if strings.HasPrefix(statement, prefix) {
			return []string{statement}
		}
	}
	return nil
}

func planDataColumns() []TableColumn {
	return []TableColumn{
		{Table: "plan_data", Name: "dist_key", Declaration: "bigint"},
		{Table: "plan_data", Name: "stats_target_key", Declaration: "int"},
		{Table: "plan_data", Name: "stats_ndistinct_key", Declaration: "bigint"},
		{Table: "plan_data", Name: "stats_corr_a", Declaration: "int"},
		{Table: "plan_data", Name: "stats_corr_b", Declaration: "int"},
		{Table: "plan_data", Name: "index_unusable_key", Declaration: "bigint"},
		{Table: "plan_data", Name: "index_drop_key", Declaration: "bigint"},
		{Table: "plan_data", Name: "index_shape_lead", Declaration: "int"},
		{Table: "plan_data", Name: "index_shape_tail", Declaration: "bigint"},
	}
}

func planDataPostMigrationDDL(schema string) []string {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil
	}
	table := quotedSchema + ".plan_data"
	return []string{
		fmt.Sprintf(`CREATE INDEX plan_data_lookup_idx ON %s (lookup_key,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_stats_target_idx ON %s (stats_target_key,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_stats_ndistinct_idx ON %s (stats_ndistinct_key,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_stats_corr_idx ON %s (stats_corr_a,stats_corr_b,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_index_unusable_idx ON %s (index_unusable_key,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_index_drop_idx ON %s (index_drop_key,dist_key,id)`, table),
		fmt.Sprintf(`CREATE INDEX plan_index_shape_good_idx ON %s (index_shape_lead,index_shape_tail,dist_key,id)`, table),
	}
}

func isPlanDataIndexDDL(statement string) bool {
	for _, name := range []string{
		"plan_data_lookup_idx",
		"plan_stats_target_idx",
		"plan_stats_ndistinct_idx",
		"plan_stats_corr_idx",
		"plan_index_unusable_idx",
		"plan_index_drop_idx",
		"plan_index_shape_good_idx",
	} {
		if strings.HasPrefix(statement, "CREATE INDEX "+name+" ") {
			return true
		}
	}
	return false
}

func planDataValues() string {
	return `mod(g,1048576)+1,
		g,
		g,
		CASE WHEN mod(g,100)<95 THEN 1 ELSE mod(g,1000) END,
		CASE WHEN mod(g,100)<80 THEN mod(g,4)+1 ELSE mod(g,1000)+100 END,
		mod(g,1000000)+1,
		mod(g,1000),
		mod(g,1000),
		g,
		g,
		mod(g,1000),
		g,
		repeat('s',400)`
}

func planDataBatch(schema string, targetBytes int64) TableBatch {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return TableBatch{}
	}
	rows := targetRows(targetBytes, planDataWeight, planDataRowBytes, planDataMinRows)
	columns := `(dist_key,id,lookup_key,skew_key,stats_target_key,stats_ndistinct_key,stats_corr_a,stats_corr_b,index_unusable_key,index_drop_key,index_shape_lead,index_shape_tail,payload)`
	return TableBatch{
		Table:             "plan_data",
		WeightPercent:     planDataWeight,
		EstimatedRowBytes: planDataRowBytes,
		Rows:              rows,
		BatchSize:         datasetBatchRows(planDataRowBytes),
		InsertSQL: fmt.Sprintf(
			`INSERT INTO %s.plan_data %s SELECT %s FROM generate_series($1,$2) AS g`,
			quotedSchema, columns, planDataValues(),
		),
	}
}

func planDataMigrations(schema string) []TableMigration {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil
	}
	return []TableMigration{
		{
			Name:        "plan_data_v2_columns",
			SourceTable: "plan_data",
			BatchSize:   100_000,
			UpdateSQL: fmt.Sprintf(`UPDATE %s.plan_data SET
			dist_key=mod(id,1048576)+1,
			stats_target_key=CASE WHEN mod(id,100)<95 THEN 1 ELSE mod(id,1000)+2 END,
			stats_ndistinct_key=mod(id,1000000)+1,
			stats_corr_a=mod(id,1000),
			stats_corr_b=mod(id,1000),
			index_unusable_key=id,
			index_drop_key=id,
			index_shape_lead=mod(id,1000),
			index_shape_tail=id
			WHERE id BETWEEN $1 AND $2
			  AND (dist_key IS NULL OR stats_target_key IS NULL OR stats_ndistinct_key IS NULL
			    OR stats_corr_a IS NULL OR stats_corr_b IS NULL
			    OR index_unusable_key IS NULL OR index_drop_key IS NULL
			    OR index_shape_lead IS NULL OR index_shape_tail IS NULL)`, quotedSchema),
		},
		{
			Name:        "plan_data_v3_stats_target",
			SourceTable: "plan_data",
			BatchSize:   100_000,
			UpdateSQL: fmt.Sprintf(`UPDATE %s.plan_data SET
			stats_target_key=CASE WHEN mod(id,100)<80 THEN mod(id,4)+1 ELSE mod(id,1000)+100 END
			WHERE id BETWEEN $1 AND $2`, quotedSchema),
		},
	}
}
