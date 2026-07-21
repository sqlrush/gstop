package healthdash

import (
	"strings"
	"testing"

	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
)

func TestDetailUsesHistoricalRealPlanFirst(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	var historyQuery string
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if strings.Contains(query, "dbe_perf.statement_history") {
			historyQuery = query
			return []dbconn.Row{{"Plan A\n  -> Scan orders"}}
		}
		t.Fatalf("unexpected lower-priority query after history plan: %s", query)
		return nil
	}

	detail := NewDetailLoader(fake).Load(77, "select * from orders")

	if detail.PlanSource != PlanSourceHistory || len(detail.PlanLines) != 2 {
		t.Fatalf("detail = %+v", detail)
	}
	if !strings.Contains(historyQuery, "start_time >= current_timestamp - interval '10 minutes'") {
		t.Fatalf("history lookup is not time-bounded for the start_time index: %s", historyQuery)
	}
}

func TestDetailFallsBackToGaussDBRunningPlan(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindGaussDB}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case strings.Contains(query, "dbe_perf.statement_history"):
			return []dbconn.Row{}
		case strings.Contains(query, "pg_stat_activity"):
			return []dbconn.Row{{int64(123), "select running"}}
		case strings.Contains(query, "gs_get_explain(123)"):
			return []dbconn.Row{{"Runtime Plan"}}
		default:
			t.Fatalf("unexpected query: %s", query)
			return nil
		}
	}

	detail := NewDetailLoader(fake).Load(88, "")

	if detail.SQLText != "select running" || detail.PlanSource != PlanSourceRuntime || len(detail.PlanLines) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDetailFallsBackToReadOnlyExplainEstimate(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch {
		case strings.Contains(query, "dbe_perf.statement_history"):
			return []dbconn.Row{}
		case strings.Contains(query, "pg_stat_activity"):
			return []dbconn.Row{{int64(321), "select estimated"}}
		case query == "EXPLAIN select estimated":
			return []dbconn.Row{{"Estimate Plan"}}
		default:
			t.Fatalf("unexpected query: %s", query)
			return nil
		}
	}

	detail := NewDetailLoader(fake).Load(99, "")

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
		if strings.Contains(query, "dbe_perf.statement_history") || strings.Contains(query, "pg_stat_activity") {
			return []dbconn.Row{}
		}
		if strings.HasPrefix(query, "EXPLAIN ") {
			return nil
		}
		t.Fatalf("statement was executed without EXPLAIN: %s", query)
		return nil
	}

	detail := NewDetailLoader(fake).Load(100, "select * from t where id = $1")

	if detail.Error == "" || detail.PlanSource != "" || len(detail.PlanLines) != 0 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestDetailRejectsMultipleStatementsBeforeExplain(t *testing.T) {
	fake := &fakeQueryer{kind: dbcompat.KindOpenGauss}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if strings.Contains(query, "dbe_perf.statement_history") || strings.Contains(query, "pg_stat_activity") {
			return []dbconn.Row{}
		}
		t.Fatalf("multiple statements reached database: %s", query)
		return nil
	}

	detail := NewDetailLoader(fake).Load(101, "select 1; delete from important")

	if !strings.Contains(detail.Error, "多语句") {
		t.Fatalf("detail error = %q", detail.Error)
	}
}
