package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type BaselineRepairResult struct {
	Target string
	Status string
}

func PlanBaselineRepairSteps(schema string) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	table := quotedSchema + ".plan_data"
	definitions := planIndexDefinitions()
	steps := make([]string, 0, len(definitions)+7)
	for _, definition := range definitions {
		statement, err := planIndexDDL(schema, definition, true)
		if err != nil {
			return nil, err
		}
		steps = append(steps, statement)
	}
	steps = append(steps,
		"DROP INDEX IF EXISTS "+quotedSchema+".plan_index_shape_bad_idx",
		"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS -1",
		"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key RESET (n_distinct)",
		"ALTER TABLE "+table+" ADD STATISTICS ((stats_corr_a,stats_corr_b))",
		"ANALYZE "+table+"(stats_target_key)",
		"ANALYZE "+table+"(stats_ndistinct_key)",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
	)
	return steps, nil
}

func RepairPlanBaseline(ctx context.Context, db *Database, schema string) ([]BaselineRepairResult, error) {
	if _, err := PlanBaselineRepairSteps(schema); err != nil {
		return nil, err
	}
	quotedSchema, _ := quoteDatasetSchema(schema)
	table := quotedSchema + ".plan_data"
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
	ensureIndex := func(definition planIndexDefinition) {
		expected, err := planIndexDDL(schema, definition, false)
		if err != nil {
			errs = append(errs, err)
			record(definition.Name, "FAILED")
			return
		}
		var actual string
		err = db.Scan(
			ctx,
			planIndexDefinitionQuery,
			[]any{schema, definition.Name},
			&actual,
		)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			exec(definition.Name, expected)
		case err != nil:
			errs = append(errs, fmt.Errorf(
				"inspect %s definition: %w",
				definition.Name,
				err,
			))
			record(definition.Name, "FAILED")
		case datasetIndexMatches(actual, expected):
			record(definition.Name, "ALREADY_OK")
		default:
			if exec(
				definition.Name+" definition",
				"DROP INDEX "+quotedSchema+"."+definition.Name,
			) {
				exec(definition.Name, expected)
			}
		}
	}

	for _, definition := range planIndexDefinitions() {
		ensureIndex(definition)
	}

	var usable int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_index WHERE indexrelid='`+quotedSchema+
			`.plan_index_unusable_idx'::regclass AND indisusable AND indisready AND indisvalid`,
		nil, &usable); err != nil {
		errs = append(errs, err)
		record("plan_index_unusable_idx usability", "FAILED")
	} else if usable == 0 {
		exec("plan_index_unusable_idx usability",
			"ALTER INDEX "+quotedSchema+".plan_index_unusable_idx REBUILD")
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
		exec("plan_index_shape_bad_idx", "DROP INDEX "+quotedSchema+".plan_index_shape_bad_idx")
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

const planIndexDefinitionQuery = `SELECT pg_catalog.pg_get_indexdef(c.oid)
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
	WHERE n.nspname=$1 AND c.relname=$2 AND c.relkind='i'`

func AnalyzeExtendedStatistics(ctx context.Context, db *Database, table string) error {
	return db.ExecSession(ctx,
		"SET default_statistics_target=-2",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	)
}

func VerifyPlanBaseline(ctx context.Context, db *Database, schema string) error {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return fmt.Errorf("unsafe schema %q", schema)
	}
	table := quotedSchema + ".plan_data"
	var errs []error
	for _, definition := range planIndexDefinitions() {
		expected, err := planIndexDDL(schema, definition, false)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var actual string
		if err := db.Scan(
			ctx,
			planIndexDefinitionQuery,
			[]any{schema, definition.Name},
			&actual,
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"baseline index %s definition: %w",
				definition.Name,
				err,
			))
		} else if !datasetIndexMatches(actual, expected) {
			errs = append(errs, fmt.Errorf(
				"baseline index %s definition %q, expected %q",
				definition.Name,
				actual,
				expected,
			))
		}
		var count int
		query := `SELECT count(*) FROM pg_index WHERE indexrelid='` + quotedSchema + `.` + definition.Name +
			`'::regclass AND indisusable AND indisready AND indisvalid`
		if err := db.Scan(ctx, query, nil, &count); err != nil {
			errs = append(errs, err)
		} else if count != 1 {
			errs = append(errs, fmt.Errorf(
				"baseline index %s is not usable",
				definition.Name,
			))
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
