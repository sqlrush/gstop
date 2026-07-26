package gsbench

import "fmt"

type PlanScenarioDefinition struct {
	Code                  ScenarioCode
	Name                  string
	Candidates            []string
	ExpectedBaselineToken string
}

func PlanScenarioDefinitions(schema string) ([]PlanScenarioDefinition, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	table := schema + ".plan_data"
	rangeQueries := func(column string, widths ...int) []string {
		out := make([]string, 0, len(widths))
		for _, width := range widths {
			out = append(out, fmt.Sprintf(
				"SELECT count(*),sum(id) FROM %s WHERE %s BETWEEN 100000 AND %d",
				table, column, 100000+width,
			))
		}
		return out
	}
	return []PlanScenarioDefinition{
		{
			Code: 601, Name: "planchange_stats_target",
			Candidates: []string{
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_target_key=1", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_target_key=2", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_target_key=3", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_target_key=4", table),
			},
			ExpectedBaselineToken: "Seq Scan",
		},
		{
			Code: 602, Name: "planchange_index_unusable",
			Candidates:            rangeQueries("index_unusable_key", 10_000, 50_000, 200_000),
			ExpectedBaselineToken: "plan_index_unusable_idx",
		},
		{
			Code: 603, Name: "planchange_stats_ndistinct",
			Candidates: []string{
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_ndistinct_key=424242", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_ndistinct_key=777777", table),
			},
			ExpectedBaselineToken: "plan_stats_ndistinct_idx",
		},
		{
			Code: 604, Name: "planchange_stats_extended",
			Candidates: []string{
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_corr_a BETWEEN 100 AND 419 AND stats_corr_b BETWEEN 100 AND 419", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_corr_a BETWEEN 100 AND 429 AND stats_corr_b BETWEEN 100 AND 429", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE stats_corr_a BETWEEN 100 AND 409 AND stats_corr_b BETWEEN 100 AND 409", table),
			},
			ExpectedBaselineToken: "plan_stats_corr_idx",
		},
		{
			Code: 605, Name: "planchange_index_drop",
			Candidates:            rangeQueries("index_drop_key", 10_000, 50_000, 200_000),
			ExpectedBaselineToken: "plan_index_drop_idx",
		},
		{
			Code: 606, Name: "planchange_index_shape",
			Candidates: []string{
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE index_shape_lead=42 AND index_shape_tail BETWEEN 100000 AND 500000", table),
				fmt.Sprintf("SELECT count(*),sum(id) FROM %s WHERE index_shape_lead=42 AND index_shape_tail BETWEEN 100000 AND 1000000", table),
			},
			ExpectedBaselineToken: "plan_index_shape_good_idx",
		},
	}, nil
}

func PlanMutationSet(runID, schema, scenario string) ([]Mutation, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	table := schema + ".plan_data"
	code, ok := planChangeCodeForName(scenario)
	if !ok {
		return nil, fmt.Errorf("unknown plan scenario %q", scenario)
	}
	base := func(kind, target, forward, inverse, verifySQL, verifyValue string) Mutation {
		return Mutation{
			RunID: runID, ScenarioCode: code, Scenario: scenario, Kind: kind, Target: target,
			ForwardSQL: forward, InverseSQL: inverse,
			VerifySQL: verifySQL, VerifyValue: verifyValue,
		}
	}
	indexExists := func(name string) string {
		return `SELECT count(*) FROM pg_indexes WHERE schemaname='` + schema +
			`' AND indexname='` + name + `'`
	}
	switch scenario {
	case "planchange_stats_target":
		verify := `SELECT attstattarget FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_target_key'`
		return []Mutation{
			base(
				"statistics_target", table+".stats_target_key",
				"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS 1",
				"ALTER TABLE "+table+" ALTER COLUMN stats_target_key SET STATISTICS -1",
				verify, "-1",
			),
			base(
				"statistics_target_analyze", table+".stats_target_key analyze",
				"ANALYZE "+table+"(stats_target_key)",
				"ANALYZE "+table+"(stats_target_key)",
				"SELECT 1", "1",
			),
		}, nil
	case "planchange_index_unusable":
		index := schema + ".plan_index_unusable_idx"
		return []Mutation{base(
			"index_unusable", index,
			"ALTER INDEX "+index+" UNUSABLE",
			"ALTER INDEX "+index+" REBUILD",
			`SELECT count(*) FROM pg_index WHERE indexrelid='`+index+`'::regclass AND indisusable AND indisready AND indisvalid`,
			"1",
		)}, nil
	case "planchange_stats_ndistinct":
		verifyNDistinct := `SELECT count(*) FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_ndistinct_key' AND (attoptions IS NULL OR NOT attoptions::text LIKE '%n_distinct=%')`
		verifyTarget := `SELECT attstattarget FROM pg_attribute WHERE attrelid='` + table + `'::regclass AND attname='stats_ndistinct_key'`
		return []Mutation{
			base(
				"statistics_ndistinct_target", table+".stats_ndistinct_key target",
				"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key SET STATISTICS 1",
				"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key SET STATISTICS -1",
				verifyTarget, "-1",
			),
			base(
				"statistics_ndistinct", table+".stats_ndistinct_key",
				"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key SET (n_distinct=1)",
				"ALTER TABLE "+table+" ALTER COLUMN stats_ndistinct_key RESET (n_distinct)",
				verifyNDistinct, "1",
			),
			base(
				"statistics_ndistinct_analyze", table+".stats_ndistinct_key analyze",
				"ANALYZE "+table+"(stats_ndistinct_key)",
				"ANALYZE "+table+"(stats_ndistinct_key)",
				"SELECT 1", "1",
			),
		}, nil
	case "planchange_stats_extended":
		verify := `SELECT count(*) FROM pg_statistic_ext WHERE starelid='` + table + `'::regclass`
		return []Mutation{
			base(
				"statistics_extended", table+".(stats_corr_a,stats_corr_b)",
				"ALTER TABLE "+table+" DELETE STATISTICS ((stats_corr_a,stats_corr_b))",
				"ALTER TABLE "+table+" ADD STATISTICS ((stats_corr_a,stats_corr_b))",
				verify, "1",
			),
			base(
				"statistics_extended_analyze", table+".(stats_corr_a,stats_corr_b) analyze",
				"ANALYZE "+table+"(stats_corr_a,stats_corr_b)",
				"ANALYZE "+table+" ((stats_corr_a,stats_corr_b))",
				"SELECT 1", "1",
			),
		}, nil
	case "planchange_index_drop":
		index := schema + ".plan_index_drop_idx"
		return []Mutation{base(
			"index_drop", index,
			"DROP INDEX "+index,
			"CREATE INDEX plan_index_drop_idx ON "+table+" (index_drop_key)",
			indexExists("plan_index_drop_idx"), "1",
		)}, nil
	case "planchange_index_shape":
		good := schema + ".plan_index_shape_good_idx"
		bad := schema + ".plan_index_shape_bad_idx"
		return []Mutation{
			base(
				"index_shape_drop_good", good,
				"DROP INDEX "+good,
				"CREATE INDEX plan_index_shape_good_idx ON "+table+" (index_shape_lead,index_shape_tail)",
				indexExists("plan_index_shape_good_idx"), "1",
			),
			base(
				"index_shape_create_bad", bad,
				"CREATE INDEX plan_index_shape_bad_idx ON "+table+" (index_shape_tail,index_shape_lead)",
				"DROP INDEX IF EXISTS "+bad,
				indexExists("plan_index_shape_bad_idx"), "0",
			),
		}, nil
	}
	return nil, fmt.Errorf("unknown plan scenario %q", scenario)
}

func planChangeCodeForName(name string) (ScenarioCode, bool) {
	for _, definition := range []struct {
		code ScenarioCode
		name string
	}{
		{601, "planchange_stats_target"}, {602, "planchange_index_unusable"},
		{603, "planchange_stats_ndistinct"}, {604, "planchange_stats_extended"},
		{605, "planchange_index_drop"}, {606, "planchange_index_shape"},
	} {
		if definition.name == name {
			return definition.code, true
		}
	}
	return 0, false
}
