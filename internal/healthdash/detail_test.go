package healthdash

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/sqlshape"
)

func TestCatalogPlanRankPrefersCurrentExplainOverHistory(t *testing.T) {
	history := planCandidate{stage: StageHistory, source: PlanSourceHistory}
	estimate := planCandidate{stage: StageEstimate, source: PlanSourceExplain}
	relocated := planCandidate{stage: StageRelocate, source: PlanSourceExplain}

	if catalogPlanRank(estimate) <= catalogPlanRank(history) {
		t.Fatal("current EXPLAIN must outrank history for catalog diagnosis")
	}
	if catalogPlanRank(relocated) <= catalogPlanRank(estimate) {
		t.Fatal("active-session EXPLAIN must outrank fallback EXPLAIN")
	}
}

func TestBindAccessSchemasUsesOnlyUnambiguousExplicitRelation(t *testing.T) {
	tests := []struct {
		name      string
		access    TableAccess
		relations []sqlshape.RelationRef
		want      string
	}{
		{
			name:   "single qualified table",
			access: TableAccess{Table: "plan_data"},
			relations: []sqlshape.RelationRef{{
				Schema: sqlshape.Identifier{Value: "gsbench_e2e_20260801_100g"},
				Table:  sqlshape.Identifier{Value: "plan_data"},
			}},
			want: "gsbench_e2e_20260801_100g",
		},
		{
			name:   "plan alias selects one same-name relation",
			access: TableAccess{Table: "plan_data", Alias: "p"},
			relations: []sqlshape.RelationRef{
				{
					Schema: sqlshape.Identifier{Value: "first"},
					Table:  sqlshape.Identifier{Value: "plan_data"},
					Alias:  identifierPointer("p", false),
				},
				{
					Schema: sqlshape.Identifier{Value: "second"},
					Table:  sqlshape.Identifier{Value: "plan_data"},
					Alias:  identifierPointer("q", false),
				},
			},
			want: "first",
		},
		{
			name:   "same table in two schemas remains unresolved",
			access: TableAccess{Table: "plan_data"},
			relations: []sqlshape.RelationRef{
				{Schema: sqlshape.Identifier{Value: "first"}, Table: sqlshape.Identifier{Value: "plan_data"}},
				{Schema: sqlshape.Identifier{Value: "second"}, Table: sqlshape.Identifier{Value: "plan_data"}},
			},
		},
		{
			name:   "conflicting alias remains unresolved",
			access: TableAccess{Table: "plan_data", Alias: "p"},
			relations: []sqlshape.RelationRef{{
				Schema: sqlshape.Identifier{Value: "first"},
				Table:  sqlshape.Identifier{Value: "plan_data"},
				Alias:  identifierPointer("q", false),
			}},
		},
		{
			name:   "SQL alias cannot bind plan access without alias evidence",
			access: TableAccess{Table: "plan_data"},
			relations: []sqlshape.RelationRef{{
				Schema: sqlshape.Identifier{Value: "first"},
				Table:  sqlshape.Identifier{Value: "plan_data"},
				Alias:  identifierPointer("p", false),
			}},
		},
		{
			name:   "quoted table comparison is case sensitive",
			access: TableAccess{Table: "plan_data"},
			relations: []sqlshape.RelationRef{{
				Schema: sqlshape.Identifier{Value: "first"},
				Table:  sqlshape.Identifier{Value: "Plan_Data", Quoted: true},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bindAccessSchemas([]TableAccess{tt.access}, tt.relations)
			if len(got) != 1 || got[0].Schema != tt.want {
				t.Fatalf("bindAccessSchemas() = %+v, want schema %q", got, tt.want)
			}
		})
	}

	existing := bindAccessSchemas(
		[]TableAccess{{Schema: "plan_schema", Table: "plan_data"}},
		[]sqlshape.RelationRef{{
			Schema: sqlshape.Identifier{Value: "sql_schema"},
			Table:  sqlshape.Identifier{Value: "plan_data"},
		}},
	)
	if existing[0].Schema != "plan_schema" {
		t.Fatalf("existing plan schema overwritten: %+v", existing)
	}
}

func TestBindAccessSchemasWithEvidenceIgnoresUnrelatedUnqualifiedRelation(t *testing.T) {
	evidence := sqlshape.RelationEvidence{
		Qualified: []sqlshape.RelationRef{{
			Schema: sqlshape.Identifier{Value: "target"},
			Table:  sqlshape.Identifier{Value: "plan_data"},
			Alias:  identifierPointer("p", false),
		}},
		Unqualified: []sqlshape.RelationRef{{
			Table: sqlshape.Identifier{Value: "dimensions"},
			Alias: identifierPointer("d", false),
		}},
	}
	got := bindAccessSchemasWithEvidence(
		[]TableAccess{{Table: "plan_data", Alias: "p"}}, evidence,
	)
	if len(got) != 1 || got[0].Schema != "target" {
		t.Fatalf("unrelated unqualified relation blocked safe binding: %+v", got)
	}

	evidence.Unqualified = []sqlshape.RelationRef{{
		Table: sqlshape.Identifier{Value: "plan_data"},
	}}
	got = bindAccessSchemasWithEvidence(
		[]TableAccess{{Table: "plan_data", Alias: "p"}}, evidence,
	)
	if got[0].Schema != "target" {
		t.Fatalf("plan alias did not disambiguate same-name relation: %+v", got)
	}

	evidence.Qualified[0].Alias = nil
	got = bindAccessSchemasWithEvidence(
		[]TableAccess{{Table: "plan_data"}}, evidence,
	)
	if got[0].Schema != "" {
		t.Fatalf("same-name unqualified relation was ignored: %+v", got)
	}
}

func identifierPointer(value string, quoted bool) *sqlshape.Identifier {
	return &sqlshape.Identifier{Value: value, Quoted: quoted}
}

func TestDetailSchemaQualifiedSQLResolvesDuplicatePlanTable(t *testing.T) {
	const schema = "gsbench_e2e_20260801_100g"
	const sqlText = `SELECT id,payload FROM "` + schema +
		`".plan_data WHERE lookup_key=500000`
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{
				"Index Scan using plan_data_lookup_idx on plan_data  (cost=0.00..2.47 rows=1 width=412)\n" +
					"  Index Cond: (lookup_key = 500000)",
			}}
		case query == "EXPLAIN "+sqlText:
			return []dbconn.Row{{
				"Seq Scan on plan_data  (cost=0.00..1469484.79 rows=1 width=412)\n" +
					"  Filter: (lookup_key = 500000)",
			}}
		case strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN"):
			return rowsOfStrings("gsbench", schema, "gsbench_v4")
		case strings.Contains(query, "n.nspname = '"+schema+"'") &&
			strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "pg_catalog.pg_attribute"):
			return rowsOfStrings("id", "lookup_key", "payload")
		case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
			strings.Contains(query, "c.relname = 'pg_index'"):
			return rowsOfStrings("indisvalid", "indisready", "indisusable")
		case strings.Contains(query, "FROM pg_stat_user_indexes"):
			return []dbconn.Row{}
		case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
			strings.Contains(query, "c.relname = 'pg_stats'"):
			return rowsOfStrings(
				"schemaname", "tablename", "attname", "n_distinct", "null_frac",
				"most_common_freqs",
			)
		case strings.Contains(query, "FROM pg_catalog.pg_stats"):
			return []dbconn.Row{{"lookup_key", float64(1000000), float64(0), ""}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 3877360001, sqlText)
	if len(detail.Tables) != 1 {
		t.Fatalf("tables=%+v notices=%+v", detail.Tables, detail.Notices)
	}
	table := detail.Tables[0]
	if table.Access.Schema != schema || table.Access.Table != "plan_data" {
		t.Fatalf("access=%+v", table.Access)
	}
	if table.Index.Assessment != IndexUnreasonable ||
		!strings.Contains(table.Index.SuggestedDDL, `ON "`+schema+`"."plan_data"`) ||
		!strings.Contains(table.Index.SuggestedDDL, `("lookup_key")`) {
		t.Fatalf("index diagnosis=%+v", table.Index)
	}
	for _, notice := range detail.Notices {
		if notice.Area == "catalog" && strings.Contains(notice.Message, "无法唯一解析") {
			t.Fatalf("unexpected ambiguity notice: %+v", detail.Notices)
		}
	}
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN") {
			t.Fatalf("bare table catalog resolution should not run: %s", query)
		}
	}
}

func TestDetailDoesNotBorrowSchemaAcrossUnqualifiedRelation(t *testing.T) {
	const sqlText = `SELECT * FROM plan_data WHERE EXISTS (` +
		`SELECT 1 FROM "explicit_schema".plan_data)`
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case query == "EXPLAIN (VERBOSE) "+sqlText:
			return []dbconn.Row{{
				"Seq Scan on plan_data  (cost=0.00..10.00 rows=1 width=8)\n" +
					"  Filter: (id = 1)",
			}}
		case strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN"):
			return rowsOfStrings("explicit_schema", "gsbench", "gsbench_v4")
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 8810, sqlText)
	if len(detail.Tables) != 1 || detail.Tables[0].Access.Schema != "" ||
		detail.Tables[0].Index.Assessment != IndexVerify {
		t.Fatalf("unqualified access was not kept ambiguous: tables=%+v notices=%+v",
			detail.Tables, detail.Notices)
	}
	foundBareResolution := false
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN") {
			foundBareResolution = true
			break
		}
	}
	if !foundBareResolution {
		t.Fatalf("unqualified table bypassed ambiguity check: %v", fake.orderedQueries())
	}
}

func TestDetailPrefersVerboseExplain(t *testing.T) {
	const sqlText = "select id from public.orders where id=1"
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch query {
		case "EXPLAIN (VERBOSE) " + sqlText:
			return []dbconn.Row{{
				"Seq Scan on public.orders  (cost=0.00..10.00 rows=1 width=8)\n" +
					"  Output: id\n  Filter: (orders.id = 1)",
			}}
		case "EXPLAIN " + sqlText:
			return []dbconn.Row{{"Seq Scan on orders  (cost=0.00..99.00 rows=1 width=8)"}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 8801, sqlText)
	if detail.PlanSource != PlanSourceExplain ||
		len(detail.PlanLines) == 0 ||
		!strings.Contains(detail.PlanLines[0], "public.orders") {
		t.Fatalf("detail plan source=%q lines=%v", detail.PlanSource, detail.PlanLines)
	}
	queries := fake.orderedQueries()
	if indexOfExactQuery(queries, "EXPLAIN (VERBOSE) "+sqlText) < 0 {
		t.Fatalf("verbose EXPLAIN was not issued: %v", queries)
	}
	if indexOfExactQuery(queries, "EXPLAIN "+sqlText) >= 0 {
		t.Fatalf("plain EXPLAIN ran despite usable verbose plan: %v", queries)
	}
}

func TestDetailFallsBackWhenVerboseExplainIsUnavailable(t *testing.T) {
	const sqlText = "select id from public.orders where id=2"
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch query {
		case "EXPLAIN (VERBOSE) " + sqlText:
			return nil
		case "EXPLAIN " + sqlText:
			return []dbconn.Row{{"Seq Scan on orders  (cost=0.00..10.00 rows=1 width=8)"}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 8802, sqlText)
	if detail.PlanSource != PlanSourceExplain || len(detail.PlanLines) == 0 {
		t.Fatalf("detail plan source=%q lines=%v", detail.PlanSource, detail.PlanLines)
	}
	queries := fake.orderedQueries()
	verboseIndex := indexOfExactQuery(queries, "EXPLAIN (VERBOSE) "+sqlText)
	plainIndex := indexOfExactQuery(queries, "EXPLAIN "+sqlText)
	if verboseIndex < 0 || plainIndex <= verboseIndex {
		t.Fatalf("EXPLAIN order is not verbose then fallback: %v", queries)
	}
}

func TestDetailDoesNotFallbackAfterVerboseExplainCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &cancelOnVerboseQueryer{cancel: cancel}
	NewDetailLoader(fake).Load(ctx, 8803, "select id from public.orders")

	queries := fake.orderedQueries()
	if indexOfExactQuery(queries, "EXPLAIN (VERBOSE) select id from public.orders") < 0 {
		t.Fatalf("verbose EXPLAIN was not issued: %v", queries)
	}
	if indexOfExactQuery(queries, "EXPLAIN select id from public.orders") >= 0 {
		t.Fatalf("plain EXPLAIN ran after cancellation: %v", queries)
	}
}

func TestDetailMaintenanceStatementsMarkIndexAdviceNotApplicable(t *testing.T) {
	const schema = "gsbench_e2e_20260801_100g"
	for _, sqlText := range []string{
		`ANALYZE "` + schema + `".plan_data`,
		`VACUUM "` + schema + `".plan_data`,
	} {
		t.Run(strings.Fields(sqlText)[0], func(t *testing.T) {
			fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
			fake.respond = func(query string) []dbconn.Row {
				switch {
				case isStatementHistoryProbe(query):
					return statementHistoryDetailColumns()
				case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
					return []dbconn.Row{{
						"Seq Scan on plan_data  (cost=0.00..1000.00 rows=100 width=8)\n" +
							"  Filter: (lookup_key = 500000)",
					}}
				case strings.Contains(query, "n.nspname = '"+schema+"'") &&
					strings.Contains(query, "c.relname = 'plan_data'") &&
					strings.Contains(query, "pg_catalog.pg_attribute"):
					return rowsOfStrings("id", "lookup_key", "payload")
				case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
					strings.Contains(query, "c.relname = 'pg_index'"):
					return rowsOfStrings("indisvalid", "indisready", "indisusable")
				case strings.Contains(query, "FROM pg_stat_user_indexes"):
					return []dbconn.Row{}
				case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
					strings.Contains(query, "c.relname = 'pg_stats'"):
					return rowsOfStrings(
						"schemaname", "tablename", "attname", "n_distinct", "null_frac",
						"most_common_freqs",
					)
				case strings.Contains(query, "FROM pg_catalog.pg_stats"):
					return []dbconn.Row{{"lookup_key", float64(1000000), float64(0), ""}}
				default:
					return []dbconn.Row{}
				}
			}

			detail := NewDetailLoader(fake).Load(context.Background(), 8890, sqlText)
			if len(detail.Tables) != 1 {
				t.Fatalf("tables=%+v notices=%+v", detail.Tables, detail.Notices)
			}
			index := detail.Tables[0].Index
			if index.Assessment != IndexNotApplicable ||
				index.SuggestedDDL != "" || len(index.SuggestedColumns) != 0 ||
				!strings.Contains(strings.Join(index.Reasons, "\n"), "维护语句") {
				t.Fatalf("maintenance index diagnosis=%+v", index)
			}
		})
	}
}

func TestDetailUnqualifiedMaintenanceKeepsNotApplicableWhenSchemaIsAmbiguous(t *testing.T) {
	const sqlText = "ANALYZE plan_data"
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{
				"Seq Scan on plan_data  (cost=0.00..1000.00 rows=100 width=8)",
			}}
		case strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN"):
			return rowsOfStrings("first", "second", "third")
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 8891, sqlText)
	if len(detail.Tables) != 1 {
		t.Fatalf("tables=%+v notices=%+v", detail.Tables, detail.Notices)
	}
	table := detail.Tables[0]
	if table.Access.Schema != "" || table.Index.Assessment != IndexNotApplicable ||
		table.Index.SuggestedDDL != "" ||
		!strings.Contains(strings.Join(table.Index.Reasons, "\n"), "维护语句") {
		t.Fatalf("ambiguous maintenance diagnosis=%+v", table)
	}
	if table.Statistics.State != FreshnessVerify {
		t.Fatalf("ambiguous maintenance statistics=%+v", table.Statistics)
	}
}

func TestCatalogDiagnosisMalformedSQLSuppressesIndexDDL(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
			strings.Contains(query, "c.relname = 'pg_index'"):
			return rowsOfStrings("indisvalid", "indisready", "indisusable")
		case strings.Contains(query, "FROM pg_stat_user_indexes"):
			return []dbconn.Row{}
		default:
			return []dbconn.Row{}
		}
	}

	result := NewDetailLoader(fake).collectTableDiagnosesResult(
		context.Background(),
		[]TableAccess{{
			Schema: "public", Table: "orders", ScanType: "Seq Scan",
			Columns: []ColumnUse{{Column: "customer_id", Kind: ColumnEquality}},
		}},
		RuntimeEvidence{}, PlanAnalysis{},
		`SELECT * FROM public.orders /* unterminated`,
	)
	if len(result.Tables) != 1 {
		t.Fatalf("tables=%+v notices=%+v", result.Tables, result.Notices)
	}
	index := result.Tables[0].Index
	if index.Assessment != IndexVerify || index.SuggestedDDL != "" ||
		len(index.SuggestedColumns) != 0 ||
		!strings.Contains(strings.Join(index.Reasons, "\n"), "解析") {
		t.Fatalf("malformed SQL index diagnosis=%+v", index)
	}
}

func TestMaintenanceKeepsNotApplicableWhenIndexCatalogQueryFails(t *testing.T) {
	for _, sqlText := range []string{`ANALYZE public.orders`, `VACUUM public.orders`} {
		t.Run(strings.Fields(sqlText)[0], func(t *testing.T) {
			fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
			fake.respond = func(query string) []dbconn.Row {
				switch {
				case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
					strings.Contains(query, "c.relname = 'pg_index'"):
					return rowsOfStrings("indisvalid", "indisready", "indisusable")
				case strings.Contains(query, "FROM pg_stat_user_indexes"):
					return nil
				default:
					return []dbconn.Row{}
				}
			}

			result := NewDetailLoader(fake).collectTableDiagnosesResult(
				context.Background(),
				[]TableAccess{{Schema: "public", Table: "orders", ScanType: "Seq Scan"}},
				RuntimeEvidence{}, PlanAnalysis{}, sqlText,
			)
			if result.Failed {
				t.Fatalf("optional maintenance index catalog failure became fatal: %+v", result)
			}
			if len(result.Tables) != 1 {
				t.Fatalf("tables=%+v notices=%+v", result.Tables, result.Notices)
			}
			index := result.Tables[0].Index
			if index.Assessment != IndexNotApplicable || index.SuggestedDDL != "" ||
				!strings.Contains(strings.Join(index.Reasons, "\n"), "维护语句") {
				t.Fatalf("maintenance index diagnosis=%+v", index)
			}
			if len(result.Notices) == 0 ||
				!strings.Contains(result.Notices[0].Message, "索引目录查询失败") {
				t.Fatalf("optional index catalog failure notice missing: %+v", result.Notices)
			}
		})
	}
}

func indexOfExactQuery(queries []string, want string) int {
	for index, query := range queries {
		if query == want {
			return index
		}
	}
	return -1
}

func TestDetailKeepsHistoryForDisplayAndExplainForCatalog(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{
				"Index Scan using plan_data_lookup_idx on plan_data  (cost=0.00..2.47 rows=1 width=412)\n" +
					"  Index Cond: (lookup_key = 500000)",
			}}
		case query == "EXPLAIN select id,payload from plan_data where lookup_key=500000":
			return []dbconn.Row{{
				"Seq Scan on plan_data  (cost=0.00..1469484.79 rows=1 width=412)\n" +
					"  Filter: (lookup_key = 500000)",
			}}
		default:
			return []dbconn.Row{}
		}
	}
	loader := NewDetailLoader(fake)
	loader.catalogDiagnose = func(
		_ context.Context,
		accesses []TableAccess,
		_ RuntimeEvidence,
		_ PlanAnalysis,
		_ string,
	) catalogDiagnosisResult {
		return catalogDiagnosisResult{Tables: []TableDiagnosis{{Access: accesses[0]}}}
	}
	target := DetailTarget{
		RequestID: 120,
		SQLID:     3877360001,
		SQLText:   "select id,payload from plan_data where lookup_key=500000",
	}
	detail := NewLoadingDetail(target)
	loader.LoadStream(context.Background(), target, func(patch DetailPatch) bool {
		MergeDetailPatch(&detail, patch)
		return true
	})

	if detail.PlanSource != PlanSourceHistory {
		t.Fatalf("display source=%q detail=%+v", detail.PlanSource, detail)
	}
	if detail.CatalogSource != PlanSourceExplain {
		t.Fatalf("catalog source=%q detail=%+v", detail.CatalogSource, detail)
	}
	if len(detail.Tables) != 1 || detail.Tables[0].Access.ScanType != "Seq Scan" {
		t.Fatalf("catalog tables=%+v", detail.Tables)
	}
}

func TestDetailSuggestsIndexFromCurrentPlanWhenHistoryReferencesDroppedIndex(t *testing.T) {
	const schema = "gsbench_e2e_20260801_100g"
	const sqlText = "SELECT id,payload FROM \"" + schema +
		"\".plan_data WHERE lookup_key=500000"
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{
				"Index Scan using plan_data_lookup_idx on plan_data  (cost=0.00..2.47 rows=1 width=412)\n" +
					"  Index Cond: (lookup_key = 500000)",
			}}
		case query == "EXPLAIN "+sqlText:
			return []dbconn.Row{{
				"Seq Scan on plan_data  (cost=0.00..1469484.79 rows=1 width=412)\n" +
					"  Filter: (lookup_key = 500000)",
			}}
		case strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "c.relkind IN"):
			return []dbconn.Row{{schema}}
		case strings.Contains(query, "n.nspname = '"+schema+"'") &&
			strings.Contains(query, "c.relname = 'plan_data'") &&
			strings.Contains(query, "pg_catalog.pg_attribute"):
			return rowsOfStrings("id", "lookup_key", "payload")
		case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
			strings.Contains(query, "c.relname = 'pg_index'"):
			return rowsOfStrings("indisvalid", "indisready", "indisusable")
		case strings.Contains(query, "FROM pg_stat_user_indexes"):
			return []dbconn.Row{}
		case strings.Contains(query, "n.nspname = 'pg_catalog'") &&
			strings.Contains(query, "c.relname = 'pg_stats'"):
			return rowsOfStrings(
				"schemaname", "tablename", "attname", "n_distinct", "null_frac",
				"most_common_freqs",
			)
		case strings.Contains(query, "FROM pg_catalog.pg_stats"):
			return []dbconn.Row{{"lookup_key", float64(1000000), float64(0), ""}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 3877360001, sqlText)
	if detail.PlanSource != PlanSourceHistory {
		t.Fatalf("display source=%q", detail.PlanSource)
	}
	if detail.CatalogSource != PlanSourceExplain {
		t.Fatalf("catalog source=%q", detail.CatalogSource)
	}
	if len(detail.Tables) != 1 {
		t.Fatalf("tables=%+v notices=%+v", detail.Tables, detail.Notices)
	}
	table := detail.Tables[0]
	if table.Access.ScanType != "Seq Scan" {
		t.Fatalf("access=%+v", table.Access)
	}
	if table.Index.Assessment != IndexUnreasonable || table.Index.SuggestedDDL == "" {
		t.Fatalf("index diagnosis=%+v", table.Index)
	}
	if !strings.Contains(table.Index.SuggestedDDL, `("lookup_key")`) {
		t.Fatalf("suggested DDL=%q", table.Index.SuggestedDDL)
	}
	if !strings.Contains(
		strings.Join(table.Index.Reasons, "\n"),
		"plan_data_lookup_idx 当前不存在",
	) {
		t.Fatalf("reasons=%v", table.Index.Reasons)
	}
}

func TestDetailUsesHistoricalRealPlanFirst(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	var historyQuery string
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") {
			historyQuery = query
			return []dbconn.Row{{"Seq Scan on orders  (cost=0.00..10.00 rows=1 width=4)"}}
		}
		return []dbconn.Row{}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 77, "select * from orders")

	if detail.PlanSource != PlanSourceHistory || len(detail.PlanLines) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if !strings.Contains(historyQuery, "start_time >= current_timestamp - interval '60 minutes'") {
		t.Fatalf("history lookup is not time-bounded for the start_time index: %s", historyQuery)
	}
}

func TestDetailHistorySkipsEmptyLatestPlan(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			if strings.Contains(query, "query_plan IS NOT NULL") &&
				strings.Contains(query, "BTRIM(query_plan) <> ''") {
				return []dbconn.Row{{"Seq Scan on orders  (cost=0.00..20.00 rows=10 width=8)"}}
			}
			return []dbconn.Row{{nil}}
		case strings.Contains(query, "pg_stat_activity"):
			return []dbconn.Row{}
		case strings.HasPrefix(query, "EXPLAIN "):
			return []dbconn.Row{}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(
		context.Background(), 78, "select * from orders where customer_id = $1",
	)

	if detail.PlanSource != PlanSourceHistory || detail.Plan.Hotspot == nil {
		t.Fatalf("detail=%+v", detail)
	}
	for query := range fake.queries {
		if strings.HasPrefix(query, "EXPLAIN ") {
			t.Fatalf("historical plan was missed and EXPLAIN was attempted: %s", query)
		}
	}
}

func TestDetailHistoryUsesOneBoundedWindow(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	var historyQueries []string
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			historyQueries = append(historyQueries, query)
			if strings.Contains(query, "interval '60 minutes'") {
				return []dbconn.Row{{"Index Scan using orders_customer_idx on orders  (cost=0.10..8.00 rows=1 width=8)"}}
			}
			return []dbconn.Row{}
		case strings.Contains(query, "pg_stat_activity"):
			return []dbconn.Row{}
		case strings.HasPrefix(query, "EXPLAIN "):
			return []dbconn.Row{}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(
		context.Background(), 79, "select * from orders where customer_id = $1",
	)

	if detail.PlanSource != PlanSourceHistory || detail.Plan.Hotspot == nil {
		t.Fatalf("detail=%+v history_queries=%v", detail, historyQueries)
	}
	if len(historyQueries) != 1 ||
		!strings.Contains(historyQueries[0], "interval '60 minutes'") {
		t.Fatalf("history query order=%v", historyQueries)
	}
}

func TestDetailFallsBackToGaussDBRunningPlan(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{}
		case strings.Contains(query, "pg_stat_activity"):
			return []dbconn.Row{{int64(123), "session-123", int64(88), "select running"}}
		case strings.Contains(query, "gs_get_explain(123)"):
			return []dbconn.Row{{"Runtime Plan"}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 88, "")

	if detail.SQLText != "select running" ||
		detail.PlanSource != PlanSourceRelocatedRuntime ||
		len(detail.PlanLines) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDetailFallsBackToReadOnlyExplainEstimate(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{}
		case strings.Contains(query, "FROM dbe_perf.statement "):
			return []dbconn.Row{{"select estimated"}}
		case query == "EXPLAIN select estimated":
			return []dbconn.Row{{"Estimate Plan"}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 99, "")

	if detail.PlanSource != PlanSourceExplain || detail.PlanLines[0] != "Estimate Plan" {
		t.Fatalf("detail = %+v", detail)
	}
	for query := range fake.queries {
		if strings.Contains(strings.ToUpper(query), "EXPLAIN ANALYZE") || strings.Contains(query, "gs_get_explain") {
			t.Fatalf("unsafe or unsupported query issued: %s", query)
		}
	}
}

func TestDetailReportsExplainFailureWithoutExecutingSQL(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") || strings.Contains(query, "pg_stat_activity") {
			return []dbconn.Row{}
		}
		if strings.HasPrefix(query, "EXPLAIN ") {
			return nil
		}
		return []dbconn.Row{}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 100, "select * from t where id = $1")

	if detail.Error == "" || detail.PlanSource != "" || len(detail.PlanLines) != 0 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDetailRejectsMultipleStatementsBeforeExplain(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") || strings.Contains(query, "pg_stat_activity") {
			return []dbconn.Row{}
		}
		return []dbconn.Row{}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 101, "select 1; delete from important")

	if !strings.Contains(detail.Error, "多语句") {
		t.Fatalf("detail error = %q", detail.Error)
	}
}

func TestDetailKeepsPlanWhenOptionalEvidenceIsUnavailable(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") {
			return []dbconn.Row{{"Seq Scan on public.orders  (cost=0.00..90.00 rows=20 width=8)"}}
		}
		return []dbconn.Row{}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 202, "select * from public.orders")

	if detail.Plan.Hotspot == nil || len(detail.PlanLines) != 1 ||
		!strings.Contains(detail.PlanLines[0], "→ HOT") {
		t.Fatalf("detail=%+v", detail)
	}
	if detail.Runtime.CPU.Available || detail.Runtime.ASH.Available {
		t.Fatalf("runtime should degrade explicitly: %+v", detail.Runtime)
	}
}

func TestDetailCPUAndASHQueriesAreBoundedAndReadOnly(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{"Seq Scan on public.orders o  (cost=0.00..90.00 rows=20 width=8)"}}
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "statement_history"):
			return rowsOfStrings("start_time", "cpu_time", "db_time", "unique_query_id")
		case strings.Contains(query, "SELECT start_time, cpu_time, db_time"):
			return []dbconn.Row{{time.Date(2026, 7, 23, 12, 0, 0, 0, time.Local), float64(5000), float64(10000)}}
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "local_active_session"):
			return rowsOfStrings("sample_time", "unique_query_id", "state", "event", "wait_status")
		case strings.Contains(query, "FROM dbe_perf.local_active_session"):
			return []dbconn.Row{{time.Now(), "active", "none", "none"}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 303, "select * from public.orders")

	if !detail.Runtime.CPU.Available || !detail.Runtime.ASH.Available {
		t.Fatalf("runtime=%+v notices=%+v", detail.Runtime, detail.Notices)
	}
	var cpuQuery, ashQuery string
	for query := range fake.queries {
		upper := strings.ToUpper(strings.TrimSpace(query))
		switch {
		case strings.Contains(query, "SELECT start_time, cpu_time, db_time"):
			cpuQuery = query
		case strings.Contains(query, "FROM dbe_perf.local_active_session"):
			ashQuery = query
		}
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe statement issued: %s", query)
		}
	}
	if !strings.Contains(cpuQuery, "ORDER BY start_time DESC LIMIT 20") {
		t.Fatalf("cpu query is not newest-20 bounded: %s", cpuQuery)
	}
	if !strings.Contains(ashQuery, "interval '15 minutes'") ||
		!strings.Contains(ashQuery, "unique_query_id = 303") {
		t.Fatalf("ash query is not time/id bounded: %s", ashQuery)
	}
}

func TestDetailFallsBackFromLocalASHToGSASP(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return []dbconn.Row{{"Result  (cost=0.00..1.00 rows=1 width=4)"}}
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "local_active_session"):
			return []dbconn.Row{}
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "gs_asp"):
			return rowsOfStrings("sample_time", "unique_query_id", "state", "event", "wait_status")
		case strings.Contains(strings.ToLower(query), "from dbe_perf.gs_asp"):
			return []dbconn.Row{{time.Now(), "active", "", ""}}
		default:
			return []dbconn.Row{}
		}
	}

	detail := NewDetailLoader(fake).Load(context.Background(), 404, "select 1")
	if !detail.Runtime.ASH.Available || detail.Runtime.ASH.OnCPUSamples != 1 {
		t.Fatalf("runtime=%+v notices=%+v", detail.Runtime, detail.Notices)
	}
}

func TestDetailLoadsGaussTimestampOnlyStatistics(t *testing.T) {
	lastAnalyze := time.Date(2026, 7, 10, 8, 0, 0, 0, time.Local)
	lastChanged := time.Date(2026, 7, 20, 9, 0, 0, 0, time.Local)
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "pg_stat_user_tables"):
			return rowsOfStrings(
				"schemaname", "relname", "last_analyze", "last_autoanalyze",
				"n_live_tup", "last_data_changed",
			)
		case strings.Contains(query, "SELECT last_analyze, last_autoanalyze, n_live_tup, last_data_changed"):
			return []dbconn.Row{{lastAnalyze, nil, float64(1000), lastChanged}}
		default:
			return []dbconn.Row{}
		}
	}
	loader := NewDetailLoader(fake)

	evidence := loader.loadStatisticsEvidence(context.Background(), TableAccess{
		Schema: "public", Table: "orders",
	})

	if !evidence.Available || !evidence.TimestampOnly ||
		evidence.LastAnalyze != lastAnalyze || evidence.LastDataChanged != lastChanged {
		t.Fatalf("evidence=%+v", evidence)
	}
}

func TestDetailLoadsExactStatisticsWithoutCurrentSettingCall(t *testing.T) {
	lastAnalyze := time.Date(2026, 7, 20, 8, 0, 0, 0, time.Local)
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "pg_stat_user_tables"):
			return rowsOfStrings(
				"schemaname", "relname", "last_analyze", "last_autoanalyze",
				"n_live_tup", "n_mod_since_analyze",
			)
		case strings.Contains(query, "FROM pg_catalog.pg_attribute") &&
			strings.Contains(query, "pg_settings"):
			return rowsOfStrings("name", "setting")
		case strings.Contains(query, "SELECT name, setting") &&
			strings.Contains(query, "autovacuum_analyze_threshold"):
			return []dbconn.Row{
				{"autovacuum_analyze_scale_factor", "0.1"},
				{"autovacuum_analyze_threshold", "50"},
			}
		case strings.Contains(query, "SELECT last_analyze, last_autoanalyze, n_live_tup, n_mod_since_analyze"):
			return []dbconn.Row{{lastAnalyze, nil, float64(1000), float64(75)}}
		default:
			return []dbconn.Row{}
		}
	}
	loader := NewDetailLoader(fake)

	evidence := loader.loadStatisticsEvidence(context.Background(), TableAccess{
		Schema: "public", Table: "orders",
	})

	if !evidence.Available || evidence.TimestampOnly ||
		evidence.ModifiedSinceAnalyze != 75 ||
		evidence.AnalyzeThreshold != 50 || evidence.AnalyzeScaleFactor != .1 {
		t.Fatalf("evidence=%+v", evidence)
	}
	for query := range fake.queries {
		if strings.Contains(query, "current_setting(") {
			t.Fatalf("unsafe absent-GUC call issued: %s", query)
		}
	}
}

type blockingDetailQueryer struct {
	started chan struct{}
	once    sync.Once
}

func (b *blockingDetailQueryer) Query(string) []dbconn.Row { return nil }
func (b *blockingDetailQueryer) ExecuteOnUserDB(string) map[string][]dbconn.Row {
	return nil
}
func (b *blockingDetailQueryer) Kind() dbcompat.Kind { return dbcompat.KindGaussDB }
func (b *blockingDetailQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil
}

func TestDetailLoadHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := &blockingDetailQueryer{started: make(chan struct{})}
	done := make(chan Detail, 1)
	go func() {
		done <- NewDetailLoader(fake).Load(ctx, 77, "select 1")
	}()
	<-fake.started
	cancel()
	select {
	case detail := <-done:
		if !strings.Contains(detail.Error, "取消") {
			t.Fatalf("detail=%+v", detail)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("detail load did not cancel")
	}
}

func rowsOfStrings(values ...string) []dbconn.Row {
	rows := make([]dbconn.Row, 0, len(values))
	for _, value := range values {
		rows = append(rows, dbconn.Row{value})
	}
	return rows
}

func isStatementHistoryProbe(query string) bool {
	return strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "statement_history")
}

func statementHistoryDetailColumns() []dbconn.Row {
	return rowsOfStrings(
		"start_time", "unique_query_id", "query_plan", "cpu_time", "db_time",
	)
}

type orderedDetailQueryer struct {
	kind    dbcompat.Kind
	mu      sync.Mutex
	queries []string
	respond func(string) []dbconn.Row
}

type cancelOnVerboseQueryer struct {
	mu      sync.Mutex
	queries []string
	cancel  context.CancelFunc
}

func (f *cancelOnVerboseQueryer) Query(query string) []dbconn.Row {
	return f.QueryContext(context.Background(), query)
}

func (f *cancelOnVerboseQueryer) QueryContext(_ context.Context, query string) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if strings.HasPrefix(query, "EXPLAIN (VERBOSE) ") {
		f.cancel()
		return nil
	}
	if strings.HasPrefix(query, "EXPLAIN ") {
		return []dbconn.Row{{"Seq Scan on orders  (cost=0.00..10.00 rows=1 width=8)"}}
	}
	return []dbconn.Row{}
}

func (f *cancelOnVerboseQueryer) ExecuteOnUserDB(string) map[string][]dbconn.Row {
	return map[string][]dbconn.Row{}
}

func (f *cancelOnVerboseQueryer) Kind() dbcompat.Kind { return dbcompat.KindOpenGauss }

func (f *cancelOnVerboseQueryer) orderedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func newOrderedDetailQueryer(kind dbcompat.Kind) *orderedDetailQueryer {
	return &orderedDetailQueryer{kind: kind}
}

func (f *orderedDetailQueryer) Query(query string) []dbconn.Row {
	return f.QueryContext(context.Background(), query)
}

func (f *orderedDetailQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	if ctx.Err() != nil {
		return nil
	}
	f.mu.Lock()
	f.queries = append(f.queries, query)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return []dbconn.Row{}
	}
	return respond(query)
}

func (f *orderedDetailQueryer) ExecuteOnUserDB(string) map[string][]dbconn.Row {
	return map[string][]dbconn.Row{}
}

func (f *orderedDetailQueryer) Kind() dbcompat.Kind { return f.kind }

func (f *orderedDetailQueryer) orderedQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func (f *orderedDetailQueryer) indexOf(fragment string) int {
	for i, query := range f.orderedQueries() {
		if strings.Contains(query, fragment) {
			return i
		}
	}
	return -1
}

func containsPlanQuality(patches []DetailPatch, quality PlanQuality) bool {
	for _, patch := range patches {
		if patch.Plan != nil && patch.Plan.Quality == quality {
			return true
		}
	}
	return false
}

type stageBlockingQueryer struct {
	*orderedDetailQueryer
	historyStarted chan struct{}
	releaseHistory chan struct{}
	once           sync.Once
}

func newStageBlockingQueryer(kind dbcompat.Kind) *stageBlockingQueryer {
	base := newOrderedDetailQueryer(kind)
	return &stageBlockingQueryer{
		orderedDetailQueryer: base,
		historyStarted:       make(chan struct{}),
		releaseHistory:       make(chan struct{}),
	}
}

func (f *stageBlockingQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	if isStatementHistoryProbe(query) {
		f.mu.Lock()
		f.queries = append(f.queries, query)
		f.mu.Unlock()
		return statementHistoryDetailColumns()
	}
	if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") {
		f.mu.Lock()
		f.queries = append(f.queries, query)
		f.mu.Unlock()
		f.once.Do(func() { close(f.historyStarted) })
		select {
		case <-f.releaseHistory:
			return []dbconn.Row{}
		case <-ctx.Done():
			return nil
		}
	}
	if query == "EXPLAIN select * from t" {
		f.mu.Lock()
		f.queries = append(f.queries, query)
		f.mu.Unlock()
		return []dbconn.Row{{"Seq Scan on t  (cost=0.00..10.00 rows=1 width=4)"}}
	}
	return f.orderedDetailQueryer.QueryContext(ctx, query)
}

func waitForPatchQuality(
	t *testing.T,
	patches <-chan DetailPatch,
	quality PlanQuality,
	timeout time.Duration,
) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case patch := <-patches:
			if patch.Plan != nil && patch.Plan.Quality == quality {
				return
			}
		case <-timer.C:
			t.Fatalf("did not receive plan quality %d", quality)
		}
	}
}

func TestMergeDetailPatchHonorsRequestPlanQualityAndRevision(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 7, SQLID: 70, SQLText: "select 70"})
	if MergeDetailPatch(&detail, DetailPatch{
		RequestID: 6, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityCapturedRuntime, Revision: 1,
			Source: PlanSourceCapturedRuntime, Lines: []string{"stale request"},
		},
	}) {
		t.Fatal("wrong request patch was accepted")
	}
	if !MergeDetailPatch(&detail, DetailPatch{
		RequestID: 7, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityExplain, Revision: 1,
			Source: PlanSourceExplain, Lines: []string{"Estimate  (cost=0.00..1.00 rows=1 width=4)"},
		},
	}) {
		t.Fatal("estimate patch was rejected")
	}
	if !MergeDetailPatch(&detail, DetailPatch{
		RequestID: 7, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityHistory, Revision: 2,
			Source: PlanSourceHistory, Lines: []string{"History  (cost=0.00..2.00 rows=1 width=4)"},
		},
	}) {
		t.Fatal("higher-quality history patch was rejected")
	}
	if MergeDetailPatch(&detail, DetailPatch{
		RequestID: 7, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityRelocatedRuntime, Revision: 3,
			Source: PlanSourceRelocatedRuntime, Lines: []string{"lower quality"},
		},
	}) {
		t.Fatal("lower-quality plan replaced history")
	}
	if !MergeDetailPatch(&detail, DetailPatch{
		RequestID: 7, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityCapturedRuntime, Revision: 3,
			Identity: "901/session-901/70",
			Source:   PlanSourceCapturedRuntime,
			Lines:    []string{"Captured  (cost=0.00..3.00 rows=1 width=4)"},
		},
	}) {
		t.Fatal("captured runtime did not replace history")
	}
	if MergeDetailPatch(&detail, DetailPatch{
		RequestID: 7, Stage: StagePlan, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityHistory, Revision: 4,
			Identity: "history/70",
			Source:   PlanSourceHistory,
			Lines:    []string{"Late history  (cost=0.00..4.00 rows=1 width=4)"},
		},
	}) {
		t.Fatal("history replaced captured runtime")
	}
	if detail.PlanSource != PlanSourceCapturedRuntime || detail.PlanRevision != 3 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestMergeDetailPatchRejectsObsoleteCatalogRevision(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 8, SQLID: 80})
	detail.PlanRevision = 4
	detail.CatalogGeneration = 2
	if MergeDetailPatch(&detail, DetailPatch{
		RequestID: 8, Stage: StageCatalog, State: StageReady,
		PlanRevision: 3, CatalogGeneration: 9,
		Tables: []TableDiagnosis{{Access: TableAccess{Table: "old"}}},
	}) {
		t.Fatal("obsolete plan catalog patch was accepted")
	}
	if MergeDetailPatch(&detail, DetailPatch{
		RequestID: 8, Stage: StageCatalog, State: StageReady,
		PlanRevision: 4, CatalogGeneration: 1,
		Tables: []TableDiagnosis{{Access: TableAccess{Table: "old-runtime"}}},
	}) {
		t.Fatal("obsolete evidence generation was accepted")
	}
}

func TestDetailValidatesCapturedTupleBeforeGsGetExplain(t *testing.T) {
	queryStart := time.Date(2026, 8, 3, 22, 0, 0, 123000000, time.FixedZone("CST", 8*60*60))
	target := DetailTarget{
		RequestID: 1, SQLID: 91, SQLText: "select 91",
		RepresentativePID: 901, RepresentativeSessionID: "session-901",
		RepresentativeQueryStart: queryStart,
	}
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "FROM pg_stat_activity") &&
			strings.Contains(query, "pid = 901") &&
			strings.Contains(query, "sessionid::text = 'session-901'") &&
			strings.Contains(query, "unique_sql_id = 91") &&
			strings.Contains(query, "query_start = '2026-08-03T22:00:00.123+08:00'::timestamptz"):
			return []dbconn.Row{{int64(901), "session-901", int64(91), "select 91"}}
		case strings.Contains(query, "gs_get_explain(901)"):
			return []dbconn.Row{{"Seq Scan on t  (cost=0.00..10.00 rows=1 width=4)"}}
		default:
			return []dbconn.Row{}
		}
	}
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
		patches = append(patches, p)
		return true
	})
	validationIndex := fake.indexOf("sessionid::text = 'session-901'")
	explainIndex := fake.indexOf("gs_get_explain(901)")
	if validationIndex < 0 || explainIndex < 0 || validationIndex > explainIndex {
		t.Fatalf("query order=%v", fake.orderedQueries())
	}
	if historyIndex := fake.indexOf("SELECT query_plan FROM dbe_perf.statement_history"); historyIndex >= 0 && validationIndex > historyIndex {
		t.Fatalf("history started before captured validation: %v", fake.orderedQueries())
	}
	if !containsPlanQuality(patches, PlanQualityCapturedRuntime) {
		t.Fatalf("patches=%+v", patches)
	}
}

func TestDetailValidatesZeroSessionIDByPIDSQLAndQueryStart(t *testing.T) {
	queryStart := time.Date(2026, 8, 3, 22, 0, 0, 123000000, time.FixedZone("CST", 8*60*60))
	target := DetailTarget{
		RequestID: 2, SQLID: 91, SQLText: "select 91",
		RepresentativePID: 901, RepresentativeQueryStart: queryStart,
	}
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "FROM pg_stat_activity") &&
			strings.Contains(query, "pid = 901") &&
			strings.Contains(query, "unique_sql_id = 91") &&
			strings.Contains(query, "query_start = '2026-08-03T22:00:00.123+08:00'::timestamptz") &&
			!strings.Contains(query, " AND sessionid::text ="):
			return []dbconn.Row{{int64(901), "0", int64(91), "select 91"}}
		case strings.Contains(query, "gs_get_explain(901)"):
			return []dbconn.Row{{"Seq Scan on t  (cost=0.00..10.00 rows=1 width=4)"}}
		default:
			return []dbconn.Row{}
		}
	}
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
		patches = append(patches, p)
		return true
	})
	if !containsPlanQuality(patches, PlanQualityCapturedRuntime) {
		t.Fatalf("zero-session target did not use exact captured runtime: queries=%v patches=%+v",
			fake.orderedQueries(), patches)
	}
}

func TestDetailRejectsReusedCapturedPID(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		if strings.Contains(query, "FROM pg_stat_activity") {
			return []dbconn.Row{{int64(901), "different-session", int64(999), "select other"}}
		}
		return []dbconn.Row{}
	}
	NewDetailLoader(fake).LoadStream(context.Background(), DetailTarget{
		RequestID: 2, SQLID: 91, RepresentativePID: 901,
		RepresentativeSessionID: "session-901",
	}, func(DetailPatch) bool { return true })
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "gs_get_explain(901)") {
			t.Fatalf("reused pid reached gs_get_explain: %s", query)
		}
	}
}

func TestDetailHistoryIsAlwaysBoundedToSixtyMinutes(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		return []dbconn.Row{}
	}
	NewDetailLoader(fake).LoadStream(context.Background(),
		DetailTarget{RequestID: 3, SQLID: 93, SQLText: "select 93"},
		func(DetailPatch) bool { return true })
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "statement_history") &&
			strings.Contains(query, "query_plan") &&
			!strings.Contains(query, "current_timestamp - interval '60 minutes'") {
			t.Fatalf("unbounded history query: %s", query)
		}
	}
}

func TestDetailPublishesEstimateBeforeBlockedHistoryFinishes(t *testing.T) {
	fake := newStageBlockingQueryer(dbcompat.KindOpenGauss)
	loader := NewDetailLoader(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	patches := make(chan DetailPatch, 16)
	done := make(chan struct{})
	go func() {
		loader.LoadStream(ctx, DetailTarget{
			RequestID: 4, SQLID: 94, SQLText: "select * from t",
		}, func(p DetailPatch) bool { patches <- p; return true })
		close(done)
	}()
	waitForPatchQuality(t, patches, PlanQualityExplain, 250*time.Millisecond)
	select {
	case <-done:
		t.Fatal("pipeline finished while history was intentionally blocked")
	default:
	}
	cancel()
	<-done
}

func TestDetailRelocatesAfterCapturedSessionBecomesStale(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "pid = 901"):
			return []dbconn.Row{}
		case strings.Contains(query, "ORDER BY query_start ASC"):
			return []dbconn.Row{{int64(902), "session-902", int64(95), "select 95"}}
		case strings.Contains(query, "pid = 902") &&
			strings.Contains(query, "sessionid::text = 'session-902'"):
			return []dbconn.Row{{int64(902), "session-902", int64(95), "select 95"}}
		case strings.Contains(query, "gs_get_explain(902)"):
			return []dbconn.Row{{"Index Scan on t  (cost=0.00..5.00 rows=1 width=4)"}}
		default:
			return []dbconn.Row{}
		}
	}
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(), DetailTarget{
		RequestID: 5, SQLID: 95, RepresentativePID: 901,
		RepresentativeSessionID: "session-901",
	}, func(p DetailPatch) bool {
		patches = append(patches, p)
		return true
	})
	if !containsPlanQuality(patches, PlanQualityRelocatedRuntime) {
		t.Fatalf("patches=%+v queries=%v", patches, fake.orderedQueries())
	}
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "gs_get_explain(901)") {
			t.Fatalf("stale captured pid used: %s", query)
		}
	}
}

func TestDetailOpenGaussNeverCallsGsGetExplain(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "FROM pg_stat_activity"):
			return []dbconn.Row{{int64(960), "session-960", int64(96), "select * from t"}}
		case query == "EXPLAIN select * from t":
			return []dbconn.Row{{"Seq Scan on t  (cost=0.00..6.00 rows=1 width=4)"}}
		default:
			return []dbconn.Row{}
		}
	}
	NewDetailLoader(fake).LoadStream(context.Background(), DetailTarget{
		RequestID: 6, SQLID: 96, RepresentativePID: 960,
		RepresentativeSessionID: "session-960",
	}, func(DetailPatch) bool { return true })
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "gs_get_explain") {
			t.Fatalf("openGauss issued unsupported query: %s", query)
		}
	}
}

func TestDetailBindSQLDoesNotReachPlainExplain(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	var notices []DiagnosticNotice
	NewDetailLoader(fake).LoadStream(context.Background(), DetailTarget{
		RequestID: 7, SQLID: 97, SQLText: "select * from t where id = $1",
	}, func(p DetailPatch) bool {
		notices = append(notices, p.Notices...)
		return true
	})
	for _, query := range fake.orderedQueries() {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "EXPLAIN ") {
			t.Fatalf("bind SQL reached plain EXPLAIN: %s", query)
		}
	}
	found := false
	for _, notice := range notices {
		found = found || strings.Contains(notice.Message, "绑定变量")
	}
	if !found {
		t.Fatalf("notices=%+v", notices)
	}
}

func TestDetailUsesCachedGsbenchPlanForNormalizedBindSQL(t *testing.T) {
	literalSQL := "select * from gsbench.orders where id=42 limit 1"
	observedSQL := "select * from gsbench.orders where id=? limit ?"
	signature := sqlshape.Signature(literalSQL)
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "c.relname = 'meta_plan_cache'"):
			return []dbconn.Row{{"gsbench"}}
		case strings.Contains(query, `"gsbench"."meta_plan_cache"`) &&
			strings.Contains(query, signature):
			return []dbconn.Row{{
				"Index Scan using orders_pkey on gsbench.orders  (cost=0.00..8.27 rows=1 width=4)",
				literalSQL,
			}}
		default:
			return []dbconn.Row{}
		}
	}
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(), DetailTarget{
		RequestID: 71, SQLID: 971, SQLText: observedSQL,
	}, func(p DetailPatch) bool {
		patches = append(patches, p)
		return true
	})
	if !containsPlanQuality(patches, PlanQualityPreflight) {
		t.Fatalf("preflight plan missing: patches=%+v queries=%v",
			patches, fake.orderedQueries())
	}
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, `"gsbench"."meta_plan_cache"`) &&
			strings.Contains(query, "scenario") {
			t.Fatalf("cached-plan lookup depends on legacy scenario column: %s", query)
		}
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "EXPLAIN ") {
			t.Fatalf("normalized bind SQL reached plain EXPLAIN: %s", query)
		}
	}
}

func TestDetailViewHidesBindFailureWhenReliablePlanExists(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{
		RequestID: 72, SQLID: 972, SQLText: "select * from t where id=?",
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 72, Stage: StagePreflight, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityPreflight, Revision: 1,
			Identity: "cache/test", Source: PlanSourcePreflight,
			Lines: []string{"Index Scan on t  (cost=0.00..1.00 rows=1 width=4)"},
		},
	})
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 72, Stage: StageEstimate, State: StageDone,
		Message: "SQL包含绑定变量，无法安全运行普通EXPLAIN",
		Notices: []DiagnosticNotice{{
			Area: "plan", Message: "SQL包含绑定变量，无法安全运行普通EXPLAIN",
		}},
	})

	view := NewView(140)
	view.RenderDetail(detail)
	text := strings.Join(view.Lines(), "\n")
	if !strings.Contains(text, PlanSourcePreflight) {
		t.Fatalf("cached plan source missing:\n%s", text)
	}
	if strings.Contains(text, "绑定变量") {
		t.Fatalf("misleading bind failure remained despite cached plan:\n%s", text)
	}
}

func TestDetailStageTimeoutDoesNotHideReadyEstimate(t *testing.T) {
	fake := newStageBlockingQueryer(dbcompat.KindOpenGauss)
	var patches []DetailPatch
	NewDetailLoader(fake, 20*time.Millisecond).LoadStream(context.Background(),
		DetailTarget{RequestID: 8, SQLID: 98, SQLText: "select * from t"},
		func(p DetailPatch) bool {
			patches = append(patches, p)
			return true
		})
	var historyTimedOut, estimateReady bool
	for _, patch := range patches {
		historyTimedOut = historyTimedOut ||
			(patch.Stage == StageHistory && patch.State == StageTimeout)
		estimateReady = estimateReady ||
			(patch.Plan != nil && patch.Plan.Quality == PlanQualityExplain)
	}
	if !historyTimedOut || !estimateReady {
		t.Fatalf("patches=%+v", patches)
	}
}

func TestDetailStopsWhenEmitterRejectsPatch(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	var sends atomic.Int32
	NewDetailLoader(fake).LoadStream(context.Background(),
		DetailTarget{RequestID: 9, SQLID: 99, SQLText: "select 99"},
		func(DetailPatch) bool { return sends.Add(1) < 2 })
	if got := sends.Load(); got != 2 {
		t.Fatalf("emitter sends=%d, want 2", got)
	}
}

func TestDetailIssuedSQLIsReadOnlyAndBounded(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		if isStatementHistoryProbe(query) {
			return statementHistoryDetailColumns()
		}
		return []dbconn.Row{}
	}
	NewDetailLoader(fake).LoadStream(context.Background(),
		DetailTarget{RequestID: 10, SQLID: 100, SQLText: "select * from t"},
		func(DetailPatch) bool { return true })
	for _, query := range fake.orderedQueries() {
		upper := strings.ToUpper(strings.TrimSpace(query))
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe statement: %s", query)
		}
		if strings.Contains(query, "statement_history") &&
			!strings.Contains(query, "interval '60 minutes'") &&
			!strings.Contains(query, "pg_attribute") {
			t.Fatalf("unbounded statement_history query: %s", query)
		}
		if (strings.Contains(query, "local_active_session") ||
			strings.Contains(query, "local_active_session_history") ||
			strings.Contains(query, "gs_asp")) &&
			!strings.Contains(query, "interval '15 minutes'") &&
			!strings.Contains(query, "pg_attribute") {
			t.Fatalf("unbounded ASH query: %s", query)
		}
	}
}

func TestDetailUsesGaussDBLocalActiveSessionHistoryWhenLiveViewIsUnavailable(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case strings.Contains(query, "pg_attribute") &&
			strings.Contains(query, "local_active_session'"):
			return []dbconn.Row{}
		case strings.Contains(query, "pg_attribute") &&
			strings.Contains(query, "local_active_session_history"):
			return rowsOfStrings(
				"sample_time", "unique_query_id", "state", "event", "wait_status",
			)
		case strings.Contains(query, "FROM dbe_perf.local_active_session_history"):
			return []dbconn.Row{{time.Now(), "active", "none", "none"}}
		default:
			return []dbconn.Row{}
		}
	}
	ash, notices := NewDetailLoader(fake).collectASHEvidence(context.Background(), 303)
	if !ash.Available {
		t.Fatalf("ASH unavailable, notices=%+v queries=%v", notices, fake.orderedQueries())
	}
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "FROM dbe_perf.local_active_session_history") &&
			(!strings.Contains(query, "interval '15 minutes'") ||
				!strings.Contains(query, "unique_query_id = 303")) {
			t.Fatalf("GaussDB ASH query is not bounded: %s", query)
		}
	}
}

func TestSafeExplainRejectsNamedAndQuestionBindMarkers(t *testing.T) {
	for _, query := range []string{
		"select * from t where id = ?",
		"select * from t where id = :customer_id",
	} {
		if _, err := safeExplainStatement(query); err == nil || !strings.Contains(err.Error(), "绑定变量") {
			t.Fatalf("query=%q err=%v", query, err)
		}
	}
	if statement, err := safeExplainStatement("select now()::date"); err != nil || statement == "" {
		t.Fatalf("PostgreSQL cast was rejected: statement=%q err=%v", statement, err)
	}
}

func TestSafeExplainRejectsUnsupportedStatementClass(t *testing.T) {
	for _, query := range []string{"VACUUM t", "CREATE TABLE x(id int)", "ANALYZE t", "EXPLAIN select 1"} {
		if _, err := safeExplainStatement(query); err == nil {
			t.Fatalf("unsafe statement accepted: %q", query)
		}
	}
}

func TestDetailClassifiesNilPlanQueriesAsStageErrors(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return statementHistoryDetailColumns()
		case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
			return nil
		case query == "EXPLAIN select 1":
			return nil
		default:
			return []dbconn.Row{}
		}
	}
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(),
		DetailTarget{RequestID: 11, SQLID: 101, SQLText: "select 1"},
		func(p DetailPatch) bool {
			patches = append(patches, p)
			return true
		})
	var historyError, estimateError bool
	for _, patch := range patches {
		historyError = historyError ||
			(patch.Stage == StageHistory && patch.State == StageError)
		estimateError = estimateError ||
			(patch.Stage == StageEstimate && patch.State == StageError)
	}
	if !historyError || !estimateError {
		t.Fatalf("patches=%+v", patches)
	}
}

func TestDetailCompletesPlanAndCatalogStagesWhenNoPlanExists(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	var patches []DetailPatch
	NewDetailLoader(fake).LoadStream(context.Background(),
		DetailTarget{RequestID: 12, SQLID: 102},
		func(p DetailPatch) bool {
			patches = append(patches, p)
			return true
		})
	detail := NewLoadingDetail(DetailTarget{RequestID: 12, SQLID: 102})
	for _, patch := range patches {
		MergeDetailPatch(&detail, patch)
	}
	if !detail.Complete ||
		detail.Stages[StagePlan].State != StageDone ||
		detail.Stages[StageCatalog].State != StageDone {
		t.Fatalf("detail=%+v", detail)
	}
}

var _ DetailQueryer = (*orderedDetailQueryer)(nil)

func TestMergeDetailPatchKeepsStageErrorsLocalWhenPlanIsReady(t *testing.T) {
	detail := NewLoadingDetail(DetailTarget{RequestID: 13, SQLID: 103})
	if !MergeDetailPatch(&detail, DetailPatch{
		RequestID: 13, Stage: StageHistory, State: StageError,
		Message: "历史执行计划查询失败或无权限",
	}) {
		t.Fatal("history stage error was not merged")
	}
	if !MergeDetailPatch(&detail, DetailPatch{
		RequestID: 13, Stage: StageEstimate, State: StageReady,
		Plan: &PlanPublication{
			Quality: PlanQualityExplain, Revision: 1,
			Identity: "explain/select 1", Source: PlanSourceExplain,
			Lines: []string{"Result  (cost=0.00..1.00 rows=1 width=4)"},
		},
	}) {
		t.Fatal("ready estimate was not merged")
	}
	if detail.Error != "" || detail.PlanQuality != PlanQualityExplain {
		t.Fatalf("detail=%+v", detail)
	}
	MergeDetailPatch(&detail, DetailPatch{
		RequestID: 13, Stage: StageCatalog, State: StageError,
		Message: "索引目录查询失败",
	})
	if detail.Error != "" || detail.PlanQuality != PlanQualityExplain {
		t.Fatalf("optional catalog error polluted detail: %+v", detail)
	}
}

func TestDetailHistoryErrorDoesNotPolluteReadyEstimate(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
	fake.respond = func(query string) []dbconn.Row {
		switch {
		case isStatementHistoryProbe(query):
			return nil
		case query == "EXPLAIN select 1":
			return []dbconn.Row{{"Result  (cost=0.00..1.00 rows=1 width=4)"}}
		default:
			return []dbconn.Row{}
		}
	}
	detail := NewDetailLoader(fake).Load(context.Background(), 103, "select 1")
	if detail.Error != "" ||
		detail.PlanQuality != PlanQualityExplain ||
		detail.Stages[StageHistory].State != StageError {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestDetailCapturedNilIsErrorAndEmptyIsStale(t *testing.T) {
	run := func(rows []dbconn.Row) StageStatus {
		fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
		fake.respond = func(query string) []dbconn.Row {
			if strings.Contains(query, "pid = 1101") {
				return rows
			}
			return []dbconn.Row{}
		}
		var detail Detail
		target := DetailTarget{
			RequestID: 14, SQLID: 104,
			RepresentativePID: 1101, RepresentativeSessionID: "session-1101",
		}
		detail = NewLoadingDetail(target)
		NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			return true
		})
		return detail.Stages[StageCaptured]
	}
	if status := run(nil); status.State != StageError {
		t.Fatalf("nil captured lookup status=%+v", status)
	}
	if status := run([]dbconn.Row{}); status.State != StageDone {
		t.Fatalf("empty captured lookup status=%+v", status)
	}
}

func TestDetailRelocationNilIsErrorAndEmptyIsNoMatch(t *testing.T) {
	run := func(rows []dbconn.Row) StageStatus {
		fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
		fake.respond = func(query string) []dbconn.Row {
			if strings.Contains(query, "ORDER BY query_start ASC") {
				return rows
			}
			return []dbconn.Row{}
		}
		target := DetailTarget{RequestID: 15, SQLID: 105, SQLText: "select 1"}
		detail := NewLoadingDetail(target)
		NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			return true
		})
		return detail.Stages[StageRelocate]
	}
	if status := run(nil); status.State != StageError {
		t.Fatalf("nil relocation status=%+v", status)
	}
	if status := run([]dbconn.Row{}); status.State != StageDone {
		t.Fatalf("empty relocation status=%+v", status)
	}
}

func TestDetailRuntimeEvidenceNilIsErrorAndEmptyIsUnavailable(t *testing.T) {
	run := func(nilProbe bool) (StageStatus, StageStatus) {
		fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
		fake.respond = func(query string) []dbconn.Row {
			if nilProbe && strings.Contains(query, "pg_attribute") {
				return nil
			}
			return []dbconn.Row{}
		}
		target := DetailTarget{RequestID: 16, SQLID: 106, SQLText: "select 1"}
		detail := NewLoadingDetail(target)
		NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			return true
		})
		return detail.Stages[StageCPU], detail.Stages[StageASH]
	}
	cpu, ash := run(true)
	if cpu.State != StageError || ash.State != StageError {
		t.Fatalf("nil probe cpu=%+v ash=%+v", cpu, ash)
	}
	cpu, ash = run(false)
	if cpu.State != StageDone || ash.State != StageDone {
		t.Fatalf("empty probe cpu=%+v ash=%+v", cpu, ash)
	}
}

func TestDetailHistoryCapabilityFailureDoesNotIssuePlanQuery(t *testing.T) {
	fake := newOrderedDetailQueryer(dbcompat.KindGaussDB)
	fake.respond = func(query string) []dbconn.Row {
		if strings.Contains(query, "pg_attribute") &&
			strings.Contains(query, "statement_history") {
			return rowsOfStrings("start_time", "unique_query_id")
		}
		return []dbconn.Row{}
	}
	target := DetailTarget{RequestID: 17, SQLID: 107, SQLText: "select 1"}
	detail := NewLoadingDetail(target)
	NewDetailLoader(fake).LoadStream(context.Background(), target, func(p DetailPatch) bool {
		MergeDetailPatch(&detail, p)
		return true
	})
	if status := detail.Stages[StageHistory]; status.State != StageError {
		t.Fatalf("history status=%+v notices=%+v", status, detail.Notices)
	}
	for _, query := range fake.orderedQueries() {
		if strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history") {
			t.Fatalf("history plan query issued without required capability: %s", query)
		}
	}
}

type catalogCoordinatorQueryer struct {
	*orderedDetailQueryer
	releaseHistory chan struct{}
	releaseCPU     chan struct{}
	releaseASH     chan struct{}
}

func newCatalogCoordinatorQueryer() *catalogCoordinatorQueryer {
	return &catalogCoordinatorQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
		releaseHistory:       make(chan struct{}),
		releaseCPU:           make(chan struct{}),
		releaseASH:           make(chan struct{}),
	}
}

func (f *catalogCoordinatorQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	switch {
	case strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "statement_history"):
		return rowsOfStrings(
			"start_time", "unique_query_id", "query_plan", "cpu_time", "db_time",
		)
	case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"):
		select {
		case <-f.releaseHistory:
			return []dbconn.Row{{"Seq Scan on public.orders  (cost=0.00..20.00 rows=2 width=8)"}}
		case <-ctx.Done():
			return nil
		}
	case strings.Contains(query, "SELECT start_time, cpu_time, db_time"):
		select {
		case <-f.releaseCPU:
			return []dbconn.Row{{time.Now(), float64(4000), float64(8000)}}
		case <-ctx.Done():
			return nil
		}
	case strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "local_active_session'"):
		select {
		case <-f.releaseASH:
			return rowsOfStrings(
				"sample_time", "unique_query_id", "state", "event", "wait_status",
			)
		case <-ctx.Done():
			return nil
		}
	case strings.Contains(query, "FROM dbe_perf.local_active_session"):
		return []dbconn.Row{{time.Now(), "active", "none", "none"}}
	case query == "EXPLAIN select * from public.orders":
		return []dbconn.Row{{"Seq Scan on public.orders  (cost=0.00..10.00 rows=1 width=8)"}}
	default:
		return []dbconn.Row{}
	}
}

func waitForDetailPatch(
	t *testing.T,
	patches <-chan DetailPatch,
	predicate func(DetailPatch) bool,
) DetailPatch {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case patch := <-patches:
			if predicate(patch) {
				return patch
			}
		case <-timer.C:
			t.Fatal("timed out waiting for detail patch")
		}
	}
}

func TestDetailCatalogLoadingTracksPlanAndEvidenceGenerations(t *testing.T) {
	fake := newCatalogCoordinatorQueryer()
	loader := NewDetailLoader(fake)
	var catalogCalls atomic.Int32
	catalogStarted := make(chan int, 8)
	loader.catalogDiagnose = func(
		ctx context.Context,
		accesses []TableAccess,
		runtime RuntimeEvidence,
		plan PlanAnalysis,
		_ string,
	) catalogDiagnosisResult {
		call := int(catalogCalls.Add(1))
		catalogStarted <- call
		if call < 4 {
			<-ctx.Done()
			return catalogDiagnosisResult{Tables: []TableDiagnosis{{
				Access: TableAccess{Table: "old-" + strconv.Itoa(call)},
			}}}
		}
		return catalogDiagnosisResult{Tables: []TableDiagnosis{{
			Access: TableAccess{Table: "final"},
		}}}
	}

	patches := make(chan DetailPatch, 128)
	done := make(chan struct{})
	detail := NewLoadingDetail(DetailTarget{
		RequestID: 18, SQLID: 108, SQLText: "select * from public.orders",
	})
	go func() {
		loader.LoadStream(context.Background(), DetailTarget{
			RequestID: 18, SQLID: 108, SQLText: "select * from public.orders",
		}, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			patches <- p
			return true
		})
		close(done)
	}()

	waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageLoading &&
			p.PlanRevision == 1 && p.CatalogGeneration == 0
	})
	<-catalogStarted
	close(fake.releaseHistory)
	waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageLoading &&
			p.PlanRevision == 2 && p.CatalogGeneration == 0
	})
	<-catalogStarted
	close(fake.releaseCPU)
	waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageLoading &&
			p.PlanRevision == 2 && p.CatalogGeneration == 1
	})
	<-catalogStarted
	close(fake.releaseASH)
	waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageLoading &&
			p.PlanRevision == 2 && p.CatalogGeneration == 2
	})
	<-catalogStarted
	final := waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageReady && len(p.Tables) == 1
	})
	if final.Tables[0].Access.Table != "final" {
		t.Fatalf("stale catalog escaped: %+v", final)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detail pipeline did not finish")
	}
	if detail.CatalogGeneration != 2 ||
		detail.Stages[StageCatalog].State != StageReady ||
		len(detail.Tables) != 1 || detail.Tables[0].Access.Table != "final" {
		t.Fatalf("merged detail=%+v", detail)
	}
	for {
		select {
		case patch := <-patches:
			if len(patch.Tables) > 0 &&
				strings.HasPrefix(patch.Tables[0].Access.Table, "old-") {
				t.Fatalf("obsolete catalog patch was emitted: %+v", patch)
			}
		default:
			return
		}
	}
}

type rejectAfterCatalogQueryer struct {
	*orderedDetailQueryer
	catalogStarted chan struct{}
	once           sync.Once
}

func newRejectAfterCatalogQueryer() *rejectAfterCatalogQueryer {
	return &rejectAfterCatalogQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
		catalogStarted:       make(chan struct{}),
	}
}

func (f *rejectAfterCatalogQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	switch {
	case query == "EXPLAIN select * from public.orders":
		return []dbconn.Row{{"Seq Scan on public.orders  (cost=0.00..10.00 rows=1 width=8)"}}
	case strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "c.relname = 'pg_index'"):
		f.once.Do(func() { close(f.catalogStarted) })
		<-ctx.Done()
		return nil
	case strings.Contains(query, "SELECT query_plan FROM dbe_perf.statement_history"),
		strings.Contains(query, "ORDER BY query_start ASC"),
		strings.Contains(query, "c.relname = 'meta_plan_cache'"),
		strings.Contains(query, "pg_attribute"):
		select {
		case <-f.catalogStarted:
			return []dbconn.Row{}
		case <-ctx.Done():
			return nil
		}
	default:
		return []dbconn.Row{}
	}
}

func TestDetailEmitterRejectionCancelsStartedCatalog(t *testing.T) {
	fake := newRejectAfterCatalogQueryer()
	loader := NewDetailLoader(fake)
	var catalogLoading atomic.Bool
	done := make(chan struct{})
	go func() {
		loader.LoadStream(context.Background(), DetailTarget{
			RequestID: 19, SQLID: 109, SQLText: "select * from public.orders",
		}, func(p DetailPatch) bool {
			if p.Stage == StageCatalog && p.State == StageLoading && p.PlanRevision > 0 {
				catalogLoading.Store(true)
				return true
			}
			return !catalogLoading.Load()
		})
		close(done)
	}()
	select {
	case <-fake.catalogStarted:
	case <-time.After(time.Second):
		t.Fatal("catalog did not start")
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("emitter rejection did not cancel and join started catalog")
	}
}

func TestDetailCatalogNilIsErrorAndEmptyIsNonError(t *testing.T) {
	run := func(catalogRows []dbconn.Row) Detail {
		fake := newOrderedDetailQueryer(dbcompat.KindOpenGauss)
		fake.respond = func(query string) []dbconn.Row {
			switch {
			case query == "EXPLAIN select * from public.orders":
				return []dbconn.Row{{
					"Seq Scan on public.orders  (cost=0.00..10.00 rows=1 width=8)",
				}}
			case strings.Contains(query, "pg_attribute") &&
				strings.Contains(query, "c.relname = 'pg_index'"):
				return catalogRows
			default:
				return []dbconn.Row{}
			}
		}
		return NewDetailLoader(fake).Load(
			context.Background(), 110, "select * from public.orders",
		)
	}

	failed := run(nil)
	if failed.PlanQuality != PlanQualityExplain ||
		failed.Error != "" ||
		failed.Stages[StageCatalog].State != StageError {
		t.Fatalf("nil catalog result detail=%+v", failed)
	}

	empty := run([]dbconn.Row{})
	if empty.PlanQuality != PlanQualityExplain ||
		empty.Error != "" ||
		empty.Stages[StageCatalog].State == StageError {
		t.Fatalf("empty catalog result detail=%+v", empty)
	}
}

type blockingHistoryProbeQueryer struct {
	*orderedDetailQueryer
}

func (f *blockingHistoryProbeQueryer) QueryContext(
	ctx context.Context,
	query string,
) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if isStatementHistoryProbe(query) {
		<-ctx.Done()
		return nil
	}
	if query == "EXPLAIN select 1" {
		return []dbconn.Row{{"Result  (cost=0.00..1.00 rows=1 width=4)"}}
	}
	return []dbconn.Row{}
}

func TestDetailHistoryCapabilityProbeTimeoutStaysTimeout(t *testing.T) {
	fake := &blockingHistoryProbeQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
	}
	target := DetailTarget{RequestID: 20, SQLID: 111, SQLText: "select 1"}
	detail := NewLoadingDetail(target)
	NewDetailLoader(fake, 20*time.Millisecond).LoadStream(
		context.Background(), target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			return true
		},
	)
	if detail.PlanQuality != PlanQualityExplain ||
		detail.Stages[StageHistory].State != StageTimeout ||
		detail.Stages[StageCPU].State != StageTimeout {
		t.Fatalf("detail=%+v", detail)
	}
}

type blockingASHProbeQueryer struct {
	*orderedDetailQueryer
}

func (f *blockingASHProbeQueryer) QueryContext(
	ctx context.Context,
	query string,
) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "local_active_session'") {
		<-ctx.Done()
		return nil
	}
	if query == "EXPLAIN select 1" {
		return []dbconn.Row{{"Result  (cost=0.00..1.00 rows=1 width=4)"}}
	}
	return []dbconn.Row{}
}

func TestDetailASHCapabilityProbeTimeoutStaysTimeout(t *testing.T) {
	fake := &blockingASHProbeQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
	}
	target := DetailTarget{RequestID: 21, SQLID: 112, SQLText: "select 1"}
	detail := NewLoadingDetail(target)
	NewDetailLoader(fake, 20*time.Millisecond).LoadStream(
		context.Background(), target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			return true
		},
	)
	if detail.PlanQuality != PlanQualityExplain ||
		detail.Stages[StageASH].State != StageTimeout {
		t.Fatalf("detail=%+v", detail)
	}
}

type blockingCatalogProbeQueryer struct {
	*orderedDetailQueryer
}

func (f *blockingCatalogProbeQueryer) QueryContext(
	ctx context.Context,
	query string,
) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	if strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "c.relname = 'pg_index'") {
		<-ctx.Done()
		return nil
	}
	if query == "EXPLAIN select * from public.orders" {
		return []dbconn.Row{{
			"Seq Scan on public.orders  (cost=0.00..10.00 rows=1 width=8)",
		}}
	}
	return []dbconn.Row{}
}

func TestDetailCatalogCapabilityProbeTimeoutStaysTimeout(t *testing.T) {
	fake := &blockingCatalogProbeQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
	}
	detail := NewDetailLoader(fake, 20*time.Millisecond).Load(
		context.Background(), 113, "select * from public.orders",
	)
	if detail.PlanQuality != PlanQualityExplain ||
		detail.Error != "" ||
		detail.Stages[StageCatalog].State != StageTimeout {
		t.Fatalf("detail=%+v", detail)
	}
}

type multiTableCatalogQueryer struct {
	*orderedDetailQueryer
	timeoutSecond bool
}

func (f *multiTableCatalogQueryer) QueryContext(
	ctx context.Context,
	query string,
) []dbconn.Row {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.mu.Unlock()
	switch {
	case query == "EXPLAIN select * from public.first_table union all select * from public.second_table":
		return []dbconn.Row{{
			"Append  (cost=0.00..30.00 rows=2 width=8)\n" +
				"  ->  Seq Scan on public.first_table  (cost=0.00..10.00 rows=1 width=8)\n" +
				"  ->  Seq Scan on public.second_table  (cost=0.00..20.00 rows=1 width=8)",
		}}
	case strings.Contains(query, "pg_attribute") &&
		strings.Contains(query, "c.relname = 'pg_index'"):
		return rowsOfStrings("indisvalid", "indisready")
	case strings.Contains(query, "FROM pg_stat_user_indexes") &&
		strings.Contains(query, "s.relname = 'first_table'"):
		return []dbconn.Row{}
	case strings.Contains(query, "FROM pg_stat_user_indexes") &&
		strings.Contains(query, "s.relname = 'second_table'"):
		if f.timeoutSecond {
			<-ctx.Done()
		}
		return nil
	default:
		return []dbconn.Row{}
	}
}

func runMultiTableCatalogDetail(
	t *testing.T,
	timeoutSecond bool,
) Detail {
	t.Helper()
	fake := &multiTableCatalogQueryer{
		orderedDetailQueryer: newOrderedDetailQueryer(dbcompat.KindOpenGauss),
		timeoutSecond:        timeoutSecond,
	}
	timeout := time.Second
	if timeoutSecond {
		timeout = 20 * time.Millisecond
	}
	return NewDetailLoader(fake, timeout).Load(
		context.Background(),
		114,
		"select * from public.first_table union all select * from public.second_table",
	)
}

func TestDetailCatalogClearsPartialTablesWhenLaterTableFails(t *testing.T) {
	detail := runMultiTableCatalogDetail(t, false)
	if detail.PlanQuality != PlanQualityExplain ||
		detail.Error != "" ||
		detail.Stages[StageCatalog].State != StageError ||
		detail.Tables == nil || len(detail.Tables) != 0 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestDetailCatalogClearsPartialTablesWhenLaterTableTimesOut(t *testing.T) {
	detail := runMultiTableCatalogDetail(t, true)
	if detail.PlanQuality != PlanQualityExplain ||
		detail.Error != "" ||
		detail.Stages[StageCatalog].State != StageTimeout ||
		detail.Tables == nil || len(detail.Tables) != 0 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestDetailCatalogLoadingClearsPreviousGenerationTables(t *testing.T) {
	fake := newCatalogCoordinatorQueryer()
	loader := NewDetailLoader(fake)
	var calls atomic.Int32
	secondCatalogStarted := make(chan struct{})
	loader.catalogDiagnose = func(
		ctx context.Context,
		accesses []TableAccess,
		runtime RuntimeEvidence,
		plan PlanAnalysis,
		_ string,
	) catalogDiagnosisResult {
		if calls.Add(1) == 1 {
			return catalogDiagnosisResult{Tables: []TableDiagnosis{{
				Access: TableAccess{Table: "old-generation"},
			}}}
		}
		close(secondCatalogStarted)
		<-ctx.Done()
		return catalogDiagnosisResult{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := DetailTarget{
		RequestID: 22, SQLID: 115, SQLText: "select * from public.orders",
	}
	detail := NewLoadingDetail(target)
	patches := make(chan DetailPatch, 64)
	done := make(chan struct{})
	go func() {
		loader.LoadStream(ctx, target, func(p DetailPatch) bool {
			MergeDetailPatch(&detail, p)
			patches <- p
			return true
		})
		close(done)
	}()

	waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageReady &&
			len(p.Tables) == 1 &&
			p.Tables[0].Access.Table == "old-generation"
	})
	close(fake.releaseCPU)
	loading := waitForDetailPatch(t, patches, func(p DetailPatch) bool {
		return p.Stage == StageCatalog && p.State == StageLoading &&
			p.PlanRevision == 1 && p.CatalogGeneration == 1
	})
	if loading.Tables == nil {
		t.Fatalf("loading patch did not explicitly clear tables: %+v", loading)
	}
	if len(detail.Tables) != 0 ||
		detail.Stages[StageCatalog].State != StageLoading ||
		detail.CatalogGeneration != 1 {
		t.Fatalf("loading was not visible in merged detail: %+v", detail)
	}
	select {
	case <-secondCatalogStarted:
	case <-time.After(time.Second):
		t.Fatal("second catalog did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("detail pipeline did not cancel")
	}
}
