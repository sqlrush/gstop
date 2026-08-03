package healthdash

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
	"gstop/internal/sqlshape"
)

func TestIntegrationActiveElapsedSQL(t *testing.T) {
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	expectedText := os.Getenv("GSTOP_INTEGRATION_ACTIVE_SQL_ID")
	if path == "" || expectedText == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG and GSTOP_INTEGRATION_ACTIVE_SQL_ID")
	}
	expectedID, err := strconv.ParseInt(expectedText, 10, 64)
	if err != nil || expectedID <= 0 {
		t.Fatalf("invalid expected SQL ID %q: %v", expectedText, err)
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-active-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()
	collector := NewCollector(cfg.With("main.dynamic_mem_enable", false), db, logger, nil, nil)
	collector.lastSlowRefresh = collector.now()
	collector.RefreshFast()
	for _, metric := range collector.Snapshot().ActiveElapsedSQL {
		if metric.SQLID == expectedID && metric.ActiveSessions > 0 &&
			metric.RepresentativeElapsedUS > 0 && !metric.RepresentativeQueryStart.IsZero() {
			t.Logf("active elapsed metric: %+v", metric)
			return
		}
	}
	t.Fatalf("SQL_ID %d absent from current active TOP5: %+v",
		expectedID, collector.Snapshot().ActiveElapsedSQL)
}

// TestIntegrationHealthQueries is opt-in because it uses the operator's live
// database configuration. It validates the openGauss/GaussDB SQL dialect of
// the fast lock/replication probes and the slow PGXC_NODE capability probe.
func TestIntegrationHealthQueries(t *testing.T) {
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()
	if rows := db.Query("SELECT 1;"); len(rows) != 1 {
		t.Fatalf("live integration preflight failed: %+v", rows)
	}

	collector := NewCollector(cfg.With("main.dynamic_mem_enable", false), db, logger, nil, nil)
	collector.lastSlowRefresh = collector.now()
	collector.RefreshFast()
	snapshot := collector.Snapshot()
	if snapshot.LockError != "" {
		t.Fatalf("lock health query is incompatible: %s", snapshot.LockError)
	}
	if snapshot.ReplicationError != "" {
		t.Fatalf("replication health queries are incompatible: %s", snapshot.ReplicationError)
	}
	if rows := db.Query(clusterTopologyQuery); rows == nil {
		t.Fatal("PGXC_NODE capability query failed; standalone must return an empty successful result")
	}
}

// TestIntegrationDetailDiagnostics validates the complete read-only detail
// path against one already-retained statement. It never executes the selected
// statement and records every query issued by DetailLoader for a safety audit.
func TestIntegrationDetailDiagnostics(t *testing.T) {
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-detail-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()

	candidates := db.Query(`SELECT unique_query_id, query, query_plan
FROM dbe_perf.statement_history
WHERE start_time >= current_timestamp - interval '10 minutes'
  AND unique_query_id IS NOT NULL
  AND query IS NOT NULL
  AND query_plan IS NOT NULL
ORDER BY CASE
           WHEN query_plan LIKE '%Seq Scan on %'
             OR query_plan LIKE '%Index Scan% on %'
             OR query_plan LIKE '%CStore Scan on %' THEN 0
           ELSE 1
         END,
         start_time DESC
LIMIT 100;`)
	var sqlID int64
	var query string
	var expectTables bool
	for _, row := range candidates {
		id, ok := row.Int(0)
		if !ok || id <= 0 || strings.TrimSpace(row.Str(1)) == "" {
			continue
		}
		if sqlID == 0 {
			sqlID, query = id, row.Str(1)
		}
		analysis := AnalyzePlan(planLines([]dbconn.Row{{row.Str(2)}}))
		if len(ExtractTableAccesses(analysis)) > 0 {
			sqlID, query, expectTables = id, row.Str(1), true
			break
		}
	}
	if sqlID == 0 {
		t.Skip("no retained statement with a plan in the latest 10 minutes")
	}

	recorder := &recordingLiveQueryer{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	detail := NewDetailLoader(recorder).Load(ctx, sqlID, query)
	if ctx.Err() != nil {
		t.Fatalf("detail load timed out: %v", ctx.Err())
	}
	if detail.PlanSource == "" || len(detail.PlanLines) == 0 {
		t.Fatalf("no plan for retained SQL_ID %d: %s", sqlID, detail.Error)
	}
	if detail.Plan.Hotspot == nil {
		t.Fatalf("plan for SQL_ID %d had no parseable cost hotspot: notices=%+v", sqlID, detail.Notices)
	}
	if !detail.Runtime.CPU.Available && !hasNoticeArea(detail.Notices, "cpu") {
		t.Fatalf("CPU evidence absence was not explicit: %+v", detail.Notices)
	}
	if !detail.Runtime.ASH.Available && !hasNoticeArea(detail.Notices, "ash") {
		t.Fatalf("ASH evidence absence was not explicit: %+v", detail.Notices)
	}
	if expectTables && len(detail.Tables) == 0 {
		t.Fatalf("table-scan plan produced no table diagnostics: notices=%+v", detail.Notices)
	}
	resolvedTables := 0
	classifiedStats := 0
	classifiedIndexes := 0
	for _, table := range detail.Tables {
		if table.Access.Schema != "" {
			resolvedTables++
		}
		if table.Statistics.State != FreshnessVerify {
			classifiedStats++
		}
		if table.Index.Assessment != IndexVerify {
			classifiedIndexes++
		}
	}
	for _, issued := range recorder.snapshot() {
		upper := strings.ToUpper(strings.TrimSpace(issued))
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.Contains(upper, "ALTER ") ||
			strings.Contains(upper, "DROP ") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe detail query issued: %s", issued)
		}
	}
	t.Logf("SQL_ID %d source=%s hotspot=%s CPU=%t ASH=%t tables=%d resolved=%d stats=%d indexes=%d",
		sqlID, detail.PlanSource, detail.Plan.Hotspot.NodeType,
		detail.Runtime.CPU.Available, detail.Runtime.ASH.Available, len(detail.Tables),
		resolvedTables, classifiedStats, classifiedIndexes)
}

// TestIntegrationDetailCatalogDiagnostics uses EXPLAIN (never ANALYZE) against
// one visible user table to validate GaussDB/openGauss index and statistics
// catalog compatibility without reading table data or manufacturing history.
func TestIntegrationDetailCatalogDiagnostics(t *testing.T) {
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-detail-catalog-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()

	tables := db.Query(`SELECT schemaname, relname
FROM pg_catalog.pg_stat_user_tables candidate
WHERE (
  SELECT count(*)
  FROM pg_catalog.pg_stat_user_tables same_name
  WHERE same_name.relname = candidate.relname
) = 1
ORDER BY schemaname, relname
LIMIT 1;`)
	if len(tables) == 0 {
		t.Skip("no uniquely named visible user table")
	}
	schema, table := tables[0].Str(0), tables[0].Str(1)
	query := "SELECT * FROM " + quoteIdentifier(schema) + "." + quoteIdentifier(table) + " LIMIT 1"

	recorder := &recordingLiveQueryer{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	detail := NewDetailLoader(recorder).Load(ctx, 9223372036854770000, query)
	if ctx.Err() != nil {
		t.Fatalf("detail load timed out: %v", ctx.Err())
	}
	if detail.PlanSource != PlanSourceExplain || detail.Plan.Hotspot == nil {
		t.Fatalf("safe EXPLAIN did not produce a parseable plan: source=%s error=%s notices=%+v",
			detail.PlanSource, detail.Error, detail.Notices)
	}
	if len(detail.Tables) == 0 {
		t.Fatalf("safe EXPLAIN produced no table diagnostics: notices=%+v", detail.Notices)
	}
	var matched *TableDiagnosis
	for i := range detail.Tables {
		if strings.EqualFold(detail.Tables[i].Access.Schema, schema) &&
			strings.EqualFold(detail.Tables[i].Access.Table, table) {
			matched = &detail.Tables[i]
			break
		}
	}
	if matched == nil {
		t.Fatalf("catalog did not resolve the EXPLAIN relation; diagnoses=%d notices=%+v",
			len(detail.Tables), detail.Notices)
	}
	if matched.Index.Assessment == IndexVerify {
		t.Fatalf("index catalog remained unclassified: %+v", matched.Index)
	}
	if matched.Statistics.State == FreshnessVerify &&
		matched.Statistics.LastAnalyze.IsZero() &&
		matched.Statistics.LastDataChanged.IsZero() {
		t.Fatalf("statistics timestamps remained unavailable: %+v", matched.Statistics)
	}
	for _, issued := range recorder.snapshot() {
		upper := strings.ToUpper(strings.TrimSpace(issued))
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.Contains(upper, "ALTER ") ||
			strings.Contains(upper, "DROP ") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe detail query issued: %s", issued)
		}
	}
	t.Logf("catalog detail source=%s hotspot=%s index=%s statistics=%s",
		detail.PlanSource, detail.Plan.Hotspot.NodeType,
		matched.Index.Assessment, matched.Statistics.State)
}

// TestIntegrationDetail601Fault is an opt-in acceptance probe for the exact
// multi-schema regression exercised by gsbench scenario 601. The caller must
// first establish the 601 fault phase; this test remains strictly read-only.
func TestIntegrationDetail601Fault(t *testing.T) {
	if os.Getenv("GSTOP_INTEGRATION_601_FAULT") != "1" {
		t.Skip("set GSTOP_INTEGRATION_601_FAULT=1 during a scenario 601 fault")
	}
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-detail-601-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()

	const schema = "gsbench_e2e_20260801_100g"
	const sqlID int64 = 3877360001
	const sqlText = `SELECT id,payload FROM "` + schema +
		`".plan_data WHERE lookup_key=500000`
	sameNameRows := db.Query(`SELECT count(DISTINCT n.nspname)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = 'plan_data'
  AND c.relkind IN ('r','p','f','m')
  AND n.nspname NOT IN ('pg_catalog','information_schema');`)
	sameNameSchemas, sameNameOK := int64(0), false
	if len(sameNameRows) > 0 {
		sameNameSchemas, sameNameOK = sameNameRows[0].Int(0)
	}
	if !sameNameOK || sameNameSchemas < 2 {
		t.Fatalf("601 acceptance requires duplicate plan_data schemas, got %d", sameNameSchemas)
	}
	recorder := &recordingLiveQueryer{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	detail := NewDetailLoader(recorder, 30*time.Second).Load(ctx, sqlID, sqlText)
	if ctx.Err() != nil || !detail.Complete {
		t.Fatalf("detail incomplete: ctx=%v detail=%+v", ctx.Err(), detail)
	}
	if detail.CatalogSource != PlanSourceExplain {
		t.Fatalf("catalog source=%q detail=%+v", detail.CatalogSource, detail)
	}

	var matched *TableDiagnosis
	for index := range detail.Tables {
		table := &detail.Tables[index]
		if table.Access.Schema == schema && table.Access.Table == "plan_data" {
			matched = table
			break
		}
	}
	if matched == nil || matched.Access.ScanType != "Seq Scan" {
		t.Fatalf("fault table diagnosis missing: tables=%+v notices=%+v", detail.Tables, detail.Notices)
	}
	if matched.Index.Assessment != IndexUnreasonable {
		t.Fatalf("fault index assessment=%+v", matched.Index)
	}
	if !strings.Contains(
		strings.Join(matched.Index.Reasons, "\n"),
		"plan_data_lookup_idx 当前不存在",
	) {
		t.Fatalf("missing dropped-index evidence: %+v", matched.Index)
	}
	ddl := matched.Index.SuggestedDDL
	if !strings.Contains(ddl, `ON "`+schema+`"."plan_data"`) ||
		!strings.Contains(ddl, `("lookup_key")`) {
		t.Fatalf("fault SuggestedDDL=%q", ddl)
	}
	for _, notice := range detail.Notices {
		if notice.Area == "catalog" && strings.Contains(notice.Message, "无法唯一解析") {
			t.Fatalf("multi-schema ambiguity remained: %+v", detail.Notices)
		}
	}

	view := NewView(240)
	view.RenderDetail(detail)
	rendered := strings.Join(view.Lines(), "\n")
	t.Log("\n" + rendered)
	for _, want := range []string{
		"SCHEMA: " + schema,
		"Table: " + schema + ".plan_data | access=Seq Scan",
		"Display-only suggestion:",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered detail missing %q:\n%s", want, rendered)
		}
	}
	for _, issued := range recorder.snapshot() {
		upper := strings.ToUpper(strings.TrimSpace(issued))
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.Contains(upper, "ALTER ") ||
			strings.Contains(upper, "DROP ") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe detail query issued: %s", issued)
		}
	}
}

// TestIntegrationDetailMaintenanceAdvice confirms that a real retained
// ANALYZE/VACUUM plan remains visible without being turned into index DDL.
func TestIntegrationDetailMaintenanceAdvice(t *testing.T) {
	if os.Getenv("GSTOP_INTEGRATION_MAINTENANCE") != "1" {
		t.Skip("set GSTOP_INTEGRATION_MAINTENANCE=1 after a retained maintenance statement")
	}
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	logger := logging.New("health-detail-maintenance-live-test", "")
	db := dbconn.New(cfg, logger)
	defer db.Close()

	rows := db.Query(`SELECT unique_query_id, query
FROM dbe_perf.statement_history
WHERE start_time >= current_timestamp - interval '30 minutes'
  AND unique_query_id IS NOT NULL
  AND query IS NOT NULL
  AND query_plan IS NOT NULL
  AND BTRIM(query_plan) <> ''
  AND (UPPER(LTRIM(query)) LIKE 'ANALYZE %'
       OR UPPER(LTRIM(query)) LIKE 'VACUUM %')
ORDER BY start_time DESC
LIMIT 20;`)
	var sqlID int64
	var sqlText string
	for _, row := range rows {
		candidateID, ok := row.Int(0)
		candidateSQL := strings.TrimSpace(row.Str(1))
		keyword, keywordErr := sqlshape.LeadingKeyword(candidateSQL)
		if ok && candidateID > 0 && keywordErr == nil &&
			(keyword == "ANALYZE" || keyword == "VACUUM") {
			sqlID, sqlText = candidateID, candidateSQL
			break
		}
	}
	if sqlID == 0 {
		t.Skip("no retained ANALYZE/VACUUM plan in the latest 30 minutes")
	}

	recorder := &recordingLiveQueryer{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	detail := NewDetailLoader(recorder, 30*time.Second).Load(ctx, sqlID, sqlText)
	if ctx.Err() != nil || !detail.Complete || len(detail.Tables) == 0 {
		t.Fatalf("maintenance detail incomplete: ctx=%v detail=%+v", ctx.Err(), detail)
	}
	for _, table := range detail.Tables {
		if table.Index.Assessment != IndexNotApplicable ||
			table.Index.SuggestedDDL != "" || len(table.Index.SuggestedColumns) != 0 {
			t.Fatalf("maintenance index advice=%+v", table.Index)
		}
	}
	view := NewView(240)
	view.RenderDetail(detail)
	rendered := strings.Join(view.Lines(), "\n")
	t.Log("\n" + rendered)
	if !strings.Contains(rendered, "索引策略: 不适用") ||
		strings.Contains(rendered, "Display-only suggestion:") ||
		strings.Contains(rendered, "CREATE INDEX") {
		t.Fatalf("maintenance detail rendered unsafe advice:\n%s", rendered)
	}
	for _, issued := range recorder.snapshot() {
		upper := strings.ToUpper(strings.TrimSpace(issued))
		if strings.Contains(upper, "EXPLAIN ANALYZE") ||
			strings.Contains(upper, "CREATE INDEX") ||
			strings.Contains(upper, "ALTER ") ||
			strings.Contains(upper, "DROP ") ||
			strings.HasPrefix(upper, "ANALYZE ") {
			t.Fatalf("unsafe detail query issued: %s", issued)
		}
	}
}

type recordingLiveQueryer struct {
	db      *dbconn.DB
	mu      sync.Mutex
	queries []string
}

func (r *recordingLiveQueryer) Query(query string) []dbconn.Row {
	r.record(query)
	return r.db.Query(query)
}

func (r *recordingLiveQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	r.record(query)
	return r.db.QueryContext(ctx, query)
}

func (r *recordingLiveQueryer) ExecuteOnUserDB(query string) map[string][]dbconn.Row {
	r.record(query)
	return r.db.ExecuteOnUserDB(query)
}

func (r *recordingLiveQueryer) Kind() dbcompat.Kind { return r.db.Kind() }

func (r *recordingLiveQueryer) record(query string) {
	r.mu.Lock()
	r.queries = append(r.queries, query)
	r.mu.Unlock()
}

func (r *recordingLiveQueryer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

func hasNoticeArea(notices []DiagnosticNotice, area string) bool {
	for _, notice := range notices {
		if notice.Area == area {
			return true
		}
	}
	return false
}
