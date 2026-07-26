package gsbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type BaselineRepairResult struct {
	Target string
	Status string
}

func PlanBaselineRepairSteps(schema string) ([]string, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	table := schema + ".plan_data"
	return []string{
		"CREATE INDEX IF NOT EXISTS plan_stats_target_idx ON " + table + " (stats_target_key)",
		"CREATE INDEX IF NOT EXISTS plan_stats_ndistinct_idx ON " + table + " (stats_ndistinct_key)",
		"CREATE INDEX IF NOT EXISTS plan_stats_corr_idx ON " + table + " (stats_corr_a,stats_corr_b)",
		"CREATE INDEX IF NOT EXISTS plan_index_unusable_idx ON " + table + " (index_unusable_key)",
		"CREATE INDEX IF NOT EXISTS plan_index_drop_idx ON " + table + " (index_drop_key)",
		"DROP INDEX IF EXISTS " + schema + ".plan_index_shape_bad_idx",
		"CREATE INDEX IF NOT EXISTS plan_index_shape_good_idx ON " + table + " (index_shape_lead,index_shape_tail)",
		"ALTER TABLE " + table + " ALTER COLUMN stats_target_key SET STATISTICS -1",
		"ALTER TABLE " + table + " ALTER COLUMN stats_ndistinct_key RESET (n_distinct)",
		"ALTER TABLE " + table + " ADD STATISTICS ((stats_corr_a,stats_corr_b))",
		"ANALYZE " + table + "(stats_target_key)",
		"ANALYZE " + table + "(stats_ndistinct_key)",
		"ANALYZE " + table + " ((stats_corr_a,stats_corr_b))",
	}, nil
}

func RepairPlanBaseline(ctx context.Context, db *Database, schema string) ([]BaselineRepairResult, error) {
	if _, err := PlanBaselineRepairSteps(schema); err != nil {
		return nil, err
	}
	table := schema + ".plan_data"
	var tableCount int
	if err := db.Scan(ctx,
		"SELECT count(*) FROM pg_tables WHERE schemaname=$1 AND tablename='plan_data'",
		[]any{schema}, &tableCount); err != nil {
		return nil, err
	}
	if tableCount != 1 {
		return nil, fmt.Errorf("%s is unavailable; run gsbench init", table)
	}

	var results []BaselineRepairResult
	var errs []error
	record := func(target, status string) {
		results = append(results, BaselineRepairResult{Target: target, Status: status})
	}
	exec := func(target, statement string) bool {
		if _, err := db.Exec(ctx, statement); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", target, err))
			record(target, "FAILED")
			return false
		}
		record(target, "RESTORED")
		return true
	}
	ensureIndex := func(name, columns string) {
		var count int
		if err := db.Scan(ctx,
			"SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname=$2",
			[]any{schema, name}, &count); err != nil {
			errs = append(errs, err)
			record(name, "FAILED")
			return
		}
		if count == 1 {
			record(name, "ALREADY_OK")
			return
		}
		exec(name, "CREATE INDEX "+name+" ON "+table+" ("+columns+")")
	}

	ensureIndex("plan_stats_target_idx", "stats_target_key")
	ensureIndex("plan_stats_ndistinct_idx", "stats_ndistinct_key")
	ensureIndex("plan_stats_corr_idx", "stats_corr_a,stats_corr_b")
	ensureIndex("plan_index_unusable_idx", "index_unusable_key")
	ensureIndex("plan_index_drop_idx", "index_drop_key")
	ensureIndex("plan_index_shape_good_idx", "index_shape_lead,index_shape_tail")

	var usable int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_index WHERE indexrelid='`+schema+
			`.plan_index_unusable_idx'::regclass AND indisusable AND indisready AND indisvalid`,
		nil, &usable); err != nil {
		errs = append(errs, err)
		record("plan_index_unusable_idx usability", "FAILED")
	} else if usable == 0 {
		exec("plan_index_unusable_idx usability",
			"ALTER INDEX "+schema+".plan_index_unusable_idx REBUILD")
	} else {
		record("plan_index_unusable_idx usability", "ALREADY_OK")
	}

	var badIndex int
	if err := db.Scan(ctx,
		"SELECT count(*) FROM pg_indexes WHERE schemaname=$1 AND indexname='plan_index_shape_bad_idx'",
		[]any{schema}, &badIndex); err != nil {
		errs = append(errs, err)
		record("plan_index_shape_bad_idx", "FAILED")
	} else if badIndex > 0 {
		exec("plan_index_shape_bad_idx", "DROP INDEX "+schema+".plan_index_shape_bad_idx")
	} else {
		record("plan_index_shape_bad_idx", "ALREADY_OK")
	}

	var targetOK int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='stats_target_key' AND attstattarget=-1`,
		nil, &targetOK); err != nil {
		errs = append(errs, err)
		record("stats_target_key", "FAILED")
	} else if targetOK == 0 {
		exec("stats_target_key",
			"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS -1")
	} else {
		record("stats_target_key", "ALREADY_OK")
	}

	var ndistinctOK int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='stats_ndistinct_key' AND attstattarget=-1 AND COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`,
		nil, &ndistinctOK); err != nil {
		errs = append(errs, err)
		record("stats_ndistinct_key", "FAILED")
	} else if ndistinctOK == 0 {
		exec("stats_ndistinct_key",
			"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key RESET (n_distinct)")
		exec("stats_ndistinct_key target",
			"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key SET STATISTICS -1")
	} else {
		record("stats_ndistinct_key", "ALREADY_OK")
	}

	var extended int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_statistic_ext WHERE starelid='`+table+`'::regclass`,
		nil, &extended); err != nil {
		errs = append(errs, err)
		record("extended_statistics", "FAILED")
	} else if extended == 0 {
		exec("extended_statistics",
			"ALTER TABLE "+table+" ADD STATISTICS ((stats_corr_a,stats_corr_b))")
	} else {
		record("extended_statistics", "ALREADY_OK")
	}

	exec("analyze stats_target_key", "ANALYZE "+table+"(stats_target_key)")
	exec("analyze stats_ndistinct_key", "ANALYZE "+table+"(stats_ndistinct_key)")
	if err := AnalyzeExtendedStatistics(ctx, db, table); err != nil {
		errs = append(errs, fmt.Errorf("analyze extended_statistics: %w", err))
		record("analyze extended_statistics", "FAILED")
	} else {
		record("analyze extended_statistics", "RESTORED")
	}

	return results, errors.Join(errs...)
}

func AnalyzeExtendedStatistics(ctx context.Context, db *Database, table string) error {
	return db.ExecSession(ctx,
		"SET default_statistics_target=-2",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	)
}

func VerifyPlanBaseline(ctx context.Context, db *Database, schema string) error {
	if !identifierRE.MatchString(schema) {
		return fmt.Errorf("unsafe schema %q", schema)
	}
	table := schema + ".plan_data"
	var errs []error
	indexes := []string{
		"plan_stats_target_idx", "plan_stats_ndistinct_idx", "plan_stats_corr_idx",
		"plan_index_unusable_idx", "plan_index_drop_idx", "plan_index_shape_good_idx",
	}
	for _, index := range indexes {
		var count int
		query := `SELECT count(*) FROM pg_index WHERE indexrelid='` + schema + `.` + index +
			`'::regclass AND indisusable AND indisready AND indisvalid`
		if err := db.Scan(ctx, query, nil, &count); err != nil {
			errs = append(errs, err)
		} else if count != 1 {
			errs = append(errs, fmt.Errorf("baseline index %s is not usable", index))
		}
	}
	checks := []struct {
		name  string
		query string
		want  int
	}{
		{"bad shape index", `SELECT count(*) FROM pg_indexes WHERE schemaname='` + schema + `' AND indexname='plan_index_shape_bad_idx'`, 0},
		{"statistics target", `SELECT count(*) FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_target_key' AND attstattarget=-1`, 1},
		{"n_distinct option", `SELECT count(*) FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_ndistinct_key' AND COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`, 1},
		{"n_distinct target", `SELECT count(*) FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_ndistinct_key' AND attstattarget=-1`, 1},
	}
	for _, check := range checks {
		var got int
		if err := db.Scan(ctx, check.query, nil, &got); err != nil {
			errs = append(errs, err)
		} else if got != check.want {
			errs = append(errs, fmt.Errorf("%s got %d want %d", check.name, got, check.want))
		}
	}
	var extended int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_statistic_ext WHERE starelid='`+table+`'::regclass`,
		nil, &extended); err != nil {
		errs = append(errs, err)
	} else if extended < 1 {
		errs = append(errs, fmt.Errorf("extended statistics are missing"))
	}
	definitions, err := PlanScenarioDefinitions(schema)
	if err != nil {
		errs = append(errs, err)
	} else if err := verifyPlanBaselinePlans(ctx, databasePlanBaselineExplainer{db}, definitions); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type planBaselineExplainer interface {
	Explain(context.Context, string) (string, error)
}

type databasePlanBaselineExplainer struct{ db *Database }

func (e databasePlanBaselineExplainer) Explain(ctx context.Context, query string) (string, error) {
	return explainLiteral(ctx, e.db, query)
}

func verifyPlanBaselinePlans(
	ctx context.Context,
	explainer planBaselineExplainer,
	definitions []PlanScenarioDefinition,
) error {
	if explainer == nil {
		return fmt.Errorf("baseline plan explainer is unavailable")
	}
	var errs []error
	for _, definition := range definitions {
		matched := false
		for _, candidate := range definition.Candidates {
			plan, err := explainer.Explain(ctx, candidate)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s explain baseline: %w", definition.Name, err))
				break
			}
			if strings.Contains(plan, definition.ExpectedBaselineToken) {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Errorf("%s baseline plan is missing %q", definition.Name, definition.ExpectedBaselineToken))
		}
	}
	return errors.Join(errs...)
}
