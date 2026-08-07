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

type PlanBaselineFinding struct {
	ScenarioCodes []ScenarioCode
	Check         string
	Target        string
	Actual        string
	Expected      string
	Statements    []string
	Detail        string
}

type datasetBaselineCatalog interface {
	SchemaExists(context.Context, string) (bool, error)
	DatasetObjectExists(context.Context, DatasetObject) (bool, error)
	ValidateDatasetObject(context.Context, DatasetObject) error
}

func InspectDatasetBaseline(
	ctx context.Context,
	cfg BenchConfig,
	environment Environment,
	catalog datasetBaselineCatalog,
) ([]PlanBaselineFinding, error) {
	if catalog == nil {
		return nil, fmt.Errorf("dataset baseline catalog is unavailable")
	}
	plan, err := PlanDataset(cfg, Capacity{}, environment)
	if err != nil {
		return nil, err
	}
	statements := append([]string(nil), plan.DDL...)
	statements = append(statements, plan.PostMigrationDDL...)
	findings := make([]PlanBaselineFinding, 0)
	schemaExists, schemaErr := catalog.SchemaExists(ctx, plan.Schema)
	if schemaErr != nil {
		findings = append(findings, PlanBaselineFinding{
			Check: "schema_presence", Target: plan.Schema,
			Actual: "unavailable", Expected: "present",
			Detail: journalSafeErrorText(schemaErr.Error()),
		})
	} else if !schemaExists {
		quotedSchema, _ := quoteDatasetSchema(plan.Schema)
		findings = append(findings, PlanBaselineFinding{
			Check: "schema_presence", Target: plan.Schema,
			Actual: "missing", Expected: "present",
			Statements: []string{"CREATE SCHEMA " + quotedSchema},
		})
	}
	for _, statement := range statements {
		object, err := parseDatasetObject(statement)
		if err != nil {
			return nil, err
		}
		codes := datasetObjectScenarioCodes(cfg, environment, object)
		if schemaErr == nil && !schemaExists {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes,
				Check:         "object_presence", Target: object.Name,
				Actual: "missing", Expected: "present",
				Statements: []string{object.DDL},
			})
			continue
		}
		exists, err := catalog.DatasetObjectExists(ctx, object)
		if err != nil {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes,
				Check:         "object_presence", Target: object.Name,
				Actual: "unavailable", Expected: "present",
				Detail: journalSafeErrorText(err.Error()),
			})
			continue
		}
		if !exists {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes,
				Check:         "object_presence", Target: object.Name,
				Actual: "missing", Expected: "present",
				Statements: []string{object.DDL},
			})
			continue
		}
		if err := catalog.ValidateDatasetObject(ctx, object); err != nil {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes,
				Check:         "object_contract", Target: object.Name,
				Actual: "mismatch", Expected: "canonical_gsbench_shape",
				Detail: journalSafeErrorText(err.Error()),
			})
		}
	}
	return findings, nil
}

func datasetObjectScenarioCodes(
	cfg BenchConfig,
	environment Environment,
	object DatasetObject,
) []ScenarioCode {
	dependency := object.Name
	if object.Kind == DatasetObjectIndex {
		if table := datasetIndexTargetTable(object.DDL); table != "" {
			dependency = table
		}
	}
	runtime := &Runtime{Config: cfg, Environment: environment}
	codes := make([]ScenarioCode, 0)
	for _, definition := range implementedScenarioDefinitions() {
		var statements []string
		var err error
		if definition.Code >= 621 && definition.Code <= 625 {
			var statement string
			statement, err = HardParseStatement(
				definition.Code,
				cfg.Data.Schema,
				42,
			)
			if err == nil {
				statements = []string{statement}
			}
		} else {
			statements, err = ScenarioWorkloadStatements(
				runtime,
				definition.Name,
			)
		}
		if err != nil {
			continue
		}
		for _, statement := range statements {
			if sqlReferencesDatasetRelation(statement, cfg.Data.Schema, dependency) {
				codes = append(codes, definition.Code)
				break
			}
		}
	}
	return codes
}

func datasetIndexTargetTable(ddl string) string {
	fields := strings.Fields(ddl)
	for index, field := range fields {
		if strings.EqualFold(field, "ON") && index+1 < len(fields) {
			qualified := strings.Trim(fields[index+1], `"`)
			if dot := strings.LastIndex(qualified, "."); dot >= 0 {
				qualified = qualified[dot+1:]
			}
			return strings.Trim(qualified, `"`)
		}
	}
	return ""
}

func sqlReferencesDatasetRelation(statement, schema, relation string) bool {
	normalized := strings.ToLower(statement)
	schema = strings.ToLower(strings.Trim(schema, `"`))
	relation = strings.ToLower(strings.Trim(relation, `"`))
	for _, candidate := range []string{
		schema + "." + relation,
		`"` + schema + `".` + relation,
		`"` + schema + `"."` + relation + `"`,
	} {
		if strings.Contains(normalized, candidate) {
			return true
		}
	}
	return false
}

func InspectPlanBaseline(
	ctx context.Context,
	db *Database,
	schema string,
) ([]PlanBaselineFinding, error) {
	if db == nil {
		return nil, fmt.Errorf("plan baseline database is unavailable")
	}
	if _, err := PlanBaselineRepairSteps(schema); err != nil {
		return nil, err
	}
	quotedSchema, _ := quoteDatasetSchema(schema)
	table := quotedSchema + ".plan_data"
	var tableCount int
	if err := db.Scan(
		ctx,
		"SELECT count(*) FROM pg_tables WHERE schemaname=$1 AND tablename='plan_data'",
		[]any{schema},
		&tableCount,
	); err != nil {
		return nil, fmt.Errorf("inspect %s: %w", table, err)
	}
	if tableCount != 1 {
		return []PlanBaselineFinding{{
			ScenarioCodes: []ScenarioCode{601, 602, 603, 604, 605, 606},
			Check:         "table_presence", Target: table,
			Actual: "missing", Expected: "present",
			Detail: "run gsbench init before plan scenarios",
		}}, nil
	}

	var findings []PlanBaselineFinding
	add := func(codes []ScenarioCode, check, target, actual, expected string, statements ...string) {
		findings = append(findings, PlanBaselineFinding{
			ScenarioCodes: append([]ScenarioCode(nil), codes...),
			Check:         check, Target: target, Actual: actual, Expected: expected,
			Statements: append([]string(nil), statements...),
		})
	}
	for _, definition := range planIndexDefinitions() {
		expected, err := planIndexDDL(schema, definition, false)
		if err != nil {
			return nil, err
		}
		var actual string
		err = db.Scan(
			ctx,
			planIndexDefinitionQuery,
			[]any{schema, definition.Name},
			&actual,
		)
		codes := planBaselineIndexScenarios(definition.Name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			add(codes, "index_definition", definition.Name, "missing", expected, expected)
			continue
		case err != nil:
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes, Check: "index_definition",
				Target: definition.Name, Actual: "unavailable",
				Expected: expected, Detail: journalSafeErrorText(err.Error()),
			})
			continue
		case !datasetIndexMatches(actual, expected):
			add(
				codes,
				"index_definition",
				definition.Name,
				actual,
				expected,
				"DROP INDEX IF EXISTS "+quotedSchema+"."+definition.Name,
				expected,
			)
		}
		var usable int
		if err := db.Scan(
			ctx,
			`SELECT count(*) FROM pg_index WHERE indexrelid='`+quotedSchema+`.`+definition.Name+
				`'::regclass AND indisusable AND indisready AND indisvalid`,
			nil,
			&usable,
		); err != nil {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes, Check: "index_usability",
				Target: definition.Name, Actual: "unavailable", Expected: "usable",
				Detail: journalSafeErrorText(err.Error()),
			})
		} else if usable != 1 {
			add(
				codes,
				"index_usability",
				definition.Name,
				"unusable",
				"usable",
				"ALTER INDEX "+quotedSchema+"."+definition.Name+" REBUILD",
			)
		}
	}

	countFinding := func(
		codes []ScenarioCode,
		check, target, query string,
		want int,
		statements ...string,
	) {
		var got int
		if err := db.Scan(ctx, query, nil, &got); err != nil {
			findings = append(findings, PlanBaselineFinding{
				ScenarioCodes: codes, Check: check, Target: target,
				Actual: "unavailable", Expected: fmt.Sprintf("count=%d", want),
				Detail: journalSafeErrorText(err.Error()),
			})
		} else if got != want {
			add(
				codes, check, target, fmt.Sprintf("count=%d", got),
				fmt.Sprintf("count=%d", want), statements...,
			)
		}
	}
	countFinding(
		[]ScenarioCode{606}, "unexpected_index", "plan_index_shape_bad_idx",
		"SELECT count(*) FROM pg_indexes WHERE schemaname='"+schema+"' AND indexname='plan_index_shape_bad_idx'",
		0,
		"DROP INDEX IF EXISTS "+quotedSchema+".plan_index_shape_bad_idx",
	)
	countFinding(
		[]ScenarioCode{602}, "column_statistics", table+".lookup_key",
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='lookup_key' AND `+
			`COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`,
		1,
		"ALTER TABLE "+table+" ALTER COLUMN lookup_key RESET (n_distinct)",
		"ANALYZE "+table+"(lookup_key)",
	)
	countFinding(
		[]ScenarioCode{601}, "statistics_target", table+".stats_target_key",
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='stats_target_key' AND attstattarget=-1`,
		1,
		"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS -1",
		"ANALYZE "+table+"(stats_target_key)",
	)
	countFinding(
		[]ScenarioCode{603}, "column_statistics", table+".stats_ndistinct_key",
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='stats_ndistinct_key' AND attstattarget=-1 AND `+
			`COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`,
		1,
		"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key RESET (n_distinct)",
		"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key SET STATISTICS -1",
		"ANALYZE "+table+"(stats_ndistinct_key)",
	)
	countFinding(
		[]ScenarioCode{604}, "extended_statistics", table+".(stats_corr_a,stats_corr_b)",
		`SELECT count(*) FROM pg_statistic_ext WHERE starelid='`+table+`'::regclass`,
		1,
		"ALTER TABLE "+table+" ADD STATISTICS ((stats_corr_a,stats_corr_b))",
		"SET default_statistics_target=-2",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	)
	return findings, nil
}

func planBaselineIndexScenarios(name string) []ScenarioCode {
	switch name {
	case "plan_data_lookup_idx":
		return []ScenarioCode{601, 602}
	case "plan_stats_target_idx":
		return []ScenarioCode{601}
	case "plan_stats_ndistinct_idx":
		return []ScenarioCode{603}
	case "plan_stats_corr_idx":
		return []ScenarioCode{604}
	case "plan_index_unusable_idx":
		return []ScenarioCode{602}
	case "plan_index_drop_idx":
		return []ScenarioCode{605}
	case "plan_index_shape_good_idx":
		return []ScenarioCode{606}
	default:
		return []ScenarioCode{601, 602, 603, 604, 605, 606}
	}
}

func PlanBaselineRepairSteps(schema string) ([]string, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	table := quotedSchema + ".plan_data"
	definitions := planIndexDefinitions()
	steps := make([]string, 0, len(definitions)+10)
	for _, definition := range definitions {
		statement, err := planIndexDDL(schema, definition, true)
		if err != nil {
			return nil, err
		}
		steps = append(steps, statement)
	}
	steps = append(steps,
		"DROP INDEX IF EXISTS "+quotedSchema+".plan_index_shape_bad_idx",
		"ALTER TABLE "+table+" ALTER COLUMN lookup_key RESET (n_distinct)",
		"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS -1",
		"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key RESET (n_distinct)",
		"ALTER TABLE "+table+" ADD STATISTICS ((stats_corr_a,stats_corr_b))",
		"ANALYZE "+table+"(lookup_key)",
		"ANALYZE "+table+"(stats_target_key)",
		"ANALYZE "+table+"(stats_ndistinct_key)",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
		"ANALYZE "+table,
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
		if _, err := db.execMaintenance(ctx, statement); err != nil {
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

	var lookupNDistinctOK int
	if err := db.Scan(ctx,
		`SELECT count(*) FROM pg_attribute WHERE attrelid='`+table+
			`'::regclass AND attname='lookup_key' AND `+
			`COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`,
		nil, &lookupNDistinctOK); err != nil {
		errs = append(errs, err)
		record("lookup_key n_distinct", "FAILED")
	} else if lookupNDistinctOK == 0 {
		exec("lookup_key n_distinct",
			"ALTER TABLE "+table+
				" ALTER COLUMN lookup_key RESET (n_distinct)")
	} else {
		record("lookup_key n_distinct", "ALREADY_OK")
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

	exec("analyze lookup_key", "ANALYZE "+table+"(lookup_key)")
	exec("analyze stats_target_key", "ANALYZE "+table+"(stats_target_key)")
	exec("analyze stats_ndistinct_key", "ANALYZE "+table+"(stats_ndistinct_key)")
	if err := AnalyzeExtendedStatistics(ctx, db, table); err != nil {
		errs = append(errs, fmt.Errorf("analyze extended_statistics: %w", err))
		record("analyze extended_statistics", "FAILED")
	} else {
		record("analyze extended_statistics", "RESTORED")
	}
	exec("analyze plan_data", "ANALYZE "+table)

	return results, errors.Join(errs...)
}

func AnalyzeExtendedStatistics(ctx context.Context, db *Database, table string) error {
	return db.ExecMaintenanceSession(ctx,
		"SET default_statistics_target=-2",
		"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	)
}

func VerifyPlanBaseline(ctx context.Context, db *Database, schema string) error {
	return verifyPlanBaseline(ctx, db, schema, nil)
}

func VerifyPlanBaselineScenarios(
	ctx context.Context,
	db *Database,
	schema string,
	codes []ScenarioCode,
) error {
	return verifyPlanBaseline(ctx, db, schema, codes)
}

func verifyPlanBaseline(
	ctx context.Context,
	db *Database,
	schema string,
	codes []ScenarioCode,
) error {
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
		{"lookup n_distinct option", `SELECT count(*) FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='lookup_key' AND COALESCE(array_to_string(attoptions,','),'') NOT LIKE '%n_distinct=%'`, 1},
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
	} else if err := verifyPlanBaselinePlans(
		ctx,
		databasePlanBaselineExplainer{db},
		selectPlanBaselineDefinitions(definitions, codes),
	); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func selectPlanBaselineDefinitions(
	definitions []PlanScenarioDefinition,
	codes []ScenarioCode,
) []PlanScenarioDefinition {
	if len(codes) == 0 {
		return definitions
	}
	selectedCodes := make(map[ScenarioCode]struct{}, len(codes))
	for _, code := range codes {
		selectedCodes[code] = struct{}{}
	}
	selected := make([]PlanScenarioDefinition, 0, len(selectedCodes))
	for _, definition := range definitions {
		if _, ok := selectedCodes[definition.Code]; ok {
			selected = append(selected, definition)
		}
	}
	return selected
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
		if definition.Code == 602 {
			for index, candidate := range definition.Candidates {
				plan, err := explainer.Explain(ctx, candidate)
				if err != nil {
					errs = append(errs, fmt.Errorf(
						"%s explain baseline candidate %d: %w",
						definition.Name,
						index+1,
						err,
					))
					continue
				}
				if !strings.Contains(plan, definition.ExpectedBaselineToken) ||
					strings.Contains(plan, "Seq Scan") {
					errs = append(errs, fmt.Errorf(
						"%s baseline candidate %d is missing %q or still uses Seq Scan",
						definition.Name,
						index+1,
						definition.ExpectedBaselineToken,
					))
				}
			}
			continue
		}
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

func verifyPlanFaultPlans(
	ctx context.Context,
	explainer planBaselineExplainer,
	definition PlanScenarioDefinition,
) error {
	if explainer == nil {
		return fmt.Errorf("fault plan explainer is unavailable")
	}
	var errs []error
	for index, candidate := range definition.Candidates {
		plan, err := explainer.Explain(ctx, candidate)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"%s explain fault candidate %d: %w",
				definition.Name,
				index+1,
				err,
			))
			continue
		}
		if !strings.Contains(plan, "Seq Scan") ||
			strings.Contains(plan, definition.ExpectedBaselineToken) {
			errs = append(errs, fmt.Errorf(
				"%s fault candidate %d did not change to Seq Scan",
				definition.Name,
				index+1,
			))
		}
	}
	return errors.Join(errs...)
}
