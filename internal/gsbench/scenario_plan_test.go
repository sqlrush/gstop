package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"testing"
	"time"
)

type explainRowsTestConnector struct {
	rows driver.Rows
}

func (c *explainRowsTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &explainRowsTestConn{rows: c.rows}, nil
}

func (*explainRowsTestConnector) Driver() driver.Driver {
	return explainRowsTestDriver{}
}

type explainRowsTestDriver struct{}

func (explainRowsTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type explainRowsTestConn struct {
	rows driver.Rows
}

func (*explainRowsTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*explainRowsTestConn) Close() error { return nil }

func (*explainRowsTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin is not supported")
}

func (c *explainRowsTestConn) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	return c.rows, nil
}

type explainRowsTestRows struct {
	columns     []string
	values      [][]driver.Value
	index       int
	terminalErr error
}

func (r *explainRowsTestRows) Columns() []string { return r.columns }
func (*explainRowsTestRows) Close() error        { return nil }

func (r *explainRowsTestRows) Next(dest []driver.Value) error {
	if r.index < len(r.values) {
		copy(dest, r.values[r.index])
		r.index++
		return nil
	}
	if r.terminalErr != nil {
		err := r.terminalErr
		r.terminalErr = nil
		return err
	}
	return io.EOF
}

func openExplainRowsForTest(
	t *testing.T,
	rows driver.Rows,
) *sql.Rows {
	t.Helper()
	pool := sql.OpenDB(&explainRowsTestConnector{rows: rows})
	t.Cleanup(func() { _ = pool.Close() })
	result, err := pool.QueryContext(context.Background(), "EXPLAIN SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	return result
}

func TestScanExplainRowsSupportsSingleColumn(t *testing.T) {
	rows := openExplainRowsForTest(t, &explainRowsTestRows{
		columns: []string{"QUERY PLAN"},
		values: [][]driver.Value{
			{"Seq Scan on plan_data  (cost=0.00..10.00 rows=1 width=8)"},
			{"  Filter: (id = 1)"},
		},
	})

	got, err := scanExplainRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	want := "Seq Scan on plan_data  (cost=0.00..10.00 rows=1 width=8)\n  Filter: (id = 1)"
	if got != want {
		t.Fatalf("plan=%q want=%q", got, want)
	}
}

func TestScanExplainRowsFormatsFiveColumnsNullAndBytes(t *testing.T) {
	rows := openExplainRowsForTest(t, &explainRowsTestRows{
		columns: []string{"id", "operation", "estimate", "detail", "node"},
		values: [][]driver.Value{
			{int64(1), []byte("Seq Scan"), nil, "cost=1", []byte("dn1")},
			{int64(2), "Filter", []byte("rows=3"), nil, "done"},
		},
	})

	got, err := scanExplainRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\tSeq Scan\tNULL\tcost=1\tdn1\n2\tFilter\trows=3\tNULL\tdone"
	if got != want {
		t.Fatalf("plan=%q want=%q", got, want)
	}
}

func TestScanExplainRowsPropagatesRowsError(t *testing.T) {
	wantErr := errors.New("old gauss row read failed")
	rows := openExplainRowsForTest(t, &explainRowsTestRows{
		columns:     []string{"QUERY PLAN"},
		values:      [][]driver.Value{{"Seq Scan"}},
		terminalErr: wantErr,
	})

	_, err := scanExplainRows(rows)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want=%v", err, wantErr)
	}
}

func TestPlanRegressionRequiresChangedPlanSameResultAndSlowdown(t *testing.T) {
	base := PlanObservation{StructureSignature: "Index Scan", ResultFingerprint: "42:900", Median: 10 * time.Millisecond}
	worse := PlanObservation{StructureSignature: "Seq Scan", ResultFingerprint: "42:900", Median: 25 * time.Millisecond}
	result := EvaluatePlanChange("planchange_index_unusable", base, worse, 2.0)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	worse.StructureSignature = "Index Scan"
	if got := EvaluatePlanChange("planchange_index_unusable", base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("unchanged plan outcome=%s", got)
	}
	worse.StructureSignature = "Seq Scan"
	worse.ResultFingerprint = "different"
	if got := EvaluatePlanChange("planchange_index_unusable", base, worse, 2.0).Outcome; got != OutcomeFailed {
		t.Fatalf("wrong-result outcome=%s", got)
	}
}

func TestLiteralPlanWorkerUsesExactSQLWithoutArguments(t *testing.T) {
	query := "SELECT count(*),sum(id) FROM gsbench.plan_data WHERE index_drop_key BETWEEN 100000 AND 110000"
	if regexp.MustCompile(`\$[0-9]+|\?`).MatchString(query) {
		t.Fatalf("query contains bind placeholder: %s", query)
	}
	if op := literalPlanOp(query); op == nil {
		t.Fatal("literal plan operation is nil")
	}
}

func TestPlanScenarioUsesItsCanonicalName(t *testing.T) {
	def := PlanScenarioDefinition{Code: 605, Name: "planchange_index_drop", Candidates: []string{"SELECT 1"}}
	scenario := NewPlanChangeScenario(def, &PlanCoordinator{})
	if scenario.Name() != "planchange_index_drop" || scenario.Code() != 605 {
		t.Fatalf("name=%s", scenario.Name())
	}
}

func TestPlanScenarioMinimumRowsMatchesGeneratedDatasetFloor(t *testing.T) {
	for _, profile := range []string{"quick", "stress"} {
		if got := minimumPlanDataRows(profile); got != planDataMinRows {
			t.Fatalf(
				"profile=%s minimum rows=%d want=%d",
				profile,
				got,
				planDataMinRows,
			)
		}
	}
}
