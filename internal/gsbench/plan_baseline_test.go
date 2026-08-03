package gsbench

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixedPlanExplainer struct{ plan string }

func (e fixedPlanExplainer) Explain(context.Context, string) (string, error) { return e.plan, nil }

func TestPlanBaselinePlanVerificationRequiresExpectedToken(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPlanBaselinePlans(context.Background(), fixedPlanExplainer{plan: "Index Scan using plan_data_lookup_idx (cost=0.00..1.00)"}, definitions[:1]); err != nil {
		t.Fatal(err)
	}
	if err := verifyPlanBaselinePlans(context.Background(), fixedPlanExplainer{plan: "Seq Scan (cost=0.00..1.00)"}, definitions[1:2]); err == nil || !strings.Contains(err.Error(), "plan_index_unusable_idx") {
		t.Fatalf("expected missing baseline index token, got %v", err)
	}
}

func TestSelectPlanBaselineDefinitionsScopesStrictVerification(t *testing.T) {
	definitions, err := PlanScenarioDefinitions("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	selected := selectPlanBaselineDefinitions(
		definitions,
		[]ScenarioCode{605, 601, 605},
	)
	if len(selected) != 2 || selected[0].Code != 601 || selected[1].Code != 605 {
		t.Fatalf("selected=%+v", selected)
	}
}

func TestPlanBaselineRepairSQLIsScopedAndComplete(t *testing.T) {
	steps, err := PlanBaselineRepairSteps("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(steps, "\n")
	for _, token := range []string{
		`"gsbench".plan_data`, "plan_index_unusable_idx", "plan_index_drop_idx",
		"plan_index_shape_good_idx", "plan_index_shape_bad_idx",
		"SET STATISTICS -1", "RESET (n_distinct)", "ADD STATISTICS",
		`ANALYZE "gsbench".plan_data`,
	} {
		if !strings.Contains(joined, token) {
			t.Errorf("repair SQL missing %s", token)
		}
	}
	if strings.Contains(joined, "pg_catalog.pg_statistic SET") ||
		strings.Contains(joined, "pg_index SET") {
		t.Fatalf("repair directly updates catalogs: %s", joined)
	}
}

func TestPlanBaselineRepairRefreshesWholeTableCardinality(t *testing.T) {
	steps, err := PlanBaselineRepairSteps("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	want := `ANALYZE "gsbench".plan_data`
	fullAt := -1
	lastTargetedAt := -1
	for index, step := range steps {
		if step == want {
			fullAt = index
		}
		if strings.HasPrefix(step, `ANALYZE "gsbench".plan_data(`) ||
			strings.HasPrefix(step, `ANALYZE "gsbench".plan_data ((`) {
			lastTargetedAt = index
		}
	}
	if fullAt < 0 {
		t.Fatalf("repair steps do not contain full-table %q: %v", want, steps)
	}
	if fullAt <= lastTargetedAt {
		t.Fatalf(
			"full-table ANALYZE at %d must follow targeted ANALYZE at %d: %v",
			fullAt,
			lastTargetedAt,
			steps,
		)
	}
}

func TestPlanBaselineRepairStepsUseEveryCompleteCanonicalIndex(t *testing.T) {
	steps, err := PlanBaselineRepairSteps("gsbench")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS plan_data_lookup_idx ON gsbench.plan_data (lookup_key,dist_key)",
		"CREATE INDEX IF NOT EXISTS plan_stats_target_idx ON gsbench.plan_data (stats_target_key,dist_key,id)",
		"CREATE INDEX IF NOT EXISTS plan_stats_ndistinct_idx ON gsbench.plan_data (stats_ndistinct_key,dist_key,id)",
		"CREATE INDEX IF NOT EXISTS plan_stats_corr_idx ON gsbench.plan_data (stats_corr_a,stats_corr_b,dist_key,id)",
		"CREATE INDEX IF NOT EXISTS plan_index_unusable_idx ON gsbench.plan_data (index_unusable_key,dist_key,id)",
		"CREATE INDEX IF NOT EXISTS plan_index_drop_idx ON gsbench.plan_data (index_drop_key,dist_key,id)",
		"CREATE INDEX IF NOT EXISTS plan_index_shape_good_idx ON gsbench.plan_data (index_shape_lead,index_shape_tail,dist_key,id)",
	}
	for _, expected := range want {
		found := false
		for _, step := range steps {
			if compactPlanDDL(step) == compactPlanDDL(expected) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("repair steps missing %q: %v", expected, steps)
		}
	}
}

type planBaselineCatalogState struct {
	mu          sync.Mutex
	definitions map[string]string
	executed    []planBaselineExecutedSQL
	nextConnID  int
}

type planBaselineExecutedSQL struct {
	connID    int
	statement string
}

func (s *planBaselineCatalogState) newConnection() driver.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextConnID++
	return &planBaselineCatalogConn{state: s, id: s.nextConnID}
}

type planBaselineCatalogConnector struct {
	state *planBaselineCatalogState
}

func (c planBaselineCatalogConnector) Connect(context.Context) (driver.Conn, error) {
	return c.state.newConnection(), nil
}

func (c planBaselineCatalogConnector) Driver() driver.Driver {
	return planBaselineCatalogDriver{state: c.state}
}

type planBaselineCatalogDriver struct {
	state *planBaselineCatalogState
}

func (d planBaselineCatalogDriver) Open(string) (driver.Conn, error) {
	return d.state.newConnection(), nil
}

type planBaselineCatalogConn struct {
	state *planBaselineCatalogState
	id    int
}

func (*planBaselineCatalogConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (*planBaselineCatalogConn) Close() error { return nil }

func (*planBaselineCatalogConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *planBaselineCatalogConn) ExecContext(
	_ context.Context,
	statement string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	c.state.mu.Lock()
	c.state.executed = append(c.state.executed, planBaselineExecutedSQL{
		connID: c.id, statement: statement,
	})
	c.state.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func (c *planBaselineCatalogConn) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	row := func(value driver.Value) driver.Rows {
		return &planBaselineCatalogRows{
			columns: []string{"value"},
			values:  [][]driver.Value{{value}},
		}
	}
	trimmed := strings.TrimSpace(query)
	switch {
	case strings.Contains(query, "FROM pg_tables"):
		return row(int64(1)), nil
	case strings.Contains(query, "pg_get_indexdef"):
		name := planBaselineNamedString(args, 1)
		c.state.mu.Lock()
		definition, ok := c.state.definitions[name]
		c.state.mu.Unlock()
		if !ok {
			return &planBaselineCatalogRows{columns: []string{"value"}}, nil
		}
		return row(definition), nil
	case strings.Contains(query, "FROM pg_indexes"):
		name := planBaselineNamedString(args, 1)
		if name == "" && strings.Contains(query, "plan_index_shape_bad_idx") {
			return row(int64(0)), nil
		}
		c.state.mu.Lock()
		_, ok := c.state.definitions[name]
		c.state.mu.Unlock()
		if ok {
			return row(int64(1)), nil
		}
		return row(int64(0)), nil
	case strings.Contains(query, "FROM pg_index"):
		return row(int64(1)), nil
	case strings.Contains(query, "FROM pg_attribute"):
		return row(int64(1)), nil
	case strings.Contains(query, "FROM pg_statistic_ext"):
		return row(int64(1)), nil
	case strings.HasPrefix(trimmed, "EXPLAIN "):
		return row(planBaselineTokenForExplain(trimmed)), nil
	default:
		return nil, fmt.Errorf("unexpected plan baseline query %q", query)
	}
}

func planBaselineNamedString(args []driver.NamedValue, index int) string {
	if index < 0 || index >= len(args) {
		return ""
	}
	value, _ := args[index].Value.(string)
	return value
}

func planBaselineTokenForExplain(query string) string {
	switch {
	case strings.Contains(query, "lookup_key"):
		return "Index Scan using plan_data_lookup_idx"
	case strings.Contains(query, "stats_target_key"):
		return "Seq Scan"
	case strings.Contains(query, "index_unusable_key"):
		return "Index Scan using plan_index_unusable_idx"
	case strings.Contains(query, "stats_ndistinct_key"):
		return "Index Scan using plan_stats_ndistinct_idx"
	case strings.Contains(query, "stats_corr_a"):
		return "Index Scan using plan_stats_corr_idx"
	case strings.Contains(query, "index_drop_key"):
		return "Index Scan using plan_index_drop_idx"
	case strings.Contains(query, "index_shape_lead"):
		return "Index Scan using plan_index_shape_good_idx"
	default:
		return "Seq Scan"
	}
}

type planBaselineCatalogRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *planBaselineCatalogRows) Columns() []string { return r.columns }
func (*planBaselineCatalogRows) Close() error        { return nil }
func (r *planBaselineCatalogRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func planBaselineDefinitionsForTest() map[string]string {
	return map[string]string{
		"plan_data_lookup_idx":      "CREATE UNIQUE INDEX plan_data_lookup_idx ON gsbench.plan_data (lookup_key,dist_key)",
		"plan_stats_target_idx":     "CREATE INDEX plan_stats_target_idx ON gsbench.plan_data (stats_target_key,dist_key,id)",
		"plan_stats_ndistinct_idx":  "CREATE INDEX plan_stats_ndistinct_idx ON gsbench.plan_data (stats_ndistinct_key,dist_key,id)",
		"plan_stats_corr_idx":       "CREATE INDEX plan_stats_corr_idx ON gsbench.plan_data (stats_corr_a,stats_corr_b,dist_key,id)",
		"plan_index_unusable_idx":   "CREATE INDEX plan_index_unusable_idx ON gsbench.plan_data (index_unusable_key,dist_key,id)",
		"plan_index_drop_idx":       "CREATE INDEX plan_index_drop_idx ON gsbench.plan_data (index_drop_key,dist_key,id)",
		"plan_index_shape_good_idx": "CREATE INDEX plan_index_shape_good_idx ON gsbench.plan_data (index_shape_lead,index_shape_tail,dist_key,id)",
	}
}

func newPlanBaselineCatalogDatabase(
	t *testing.T,
	state *planBaselineCatalogState,
) *Database {
	t.Helper()
	pool := sql.OpenDB(planBaselineCatalogConnector{state: state})
	ctx, cancel := context.WithCancel(context.Background())
	db := &Database{
		cfg: BenchConfig{Safety: SafetyConfig{QueryTimeout: time.Second}},
		ctx: ctx, cancel: cancel, pool: pool, tagged: map[*TaggedConn]struct{}{},
	}
	t.Cleanup(func() {
		cancel()
		if err := pool.Close(); err != nil {
			t.Errorf("close plan baseline test database: %v", err)
		}
	})
	return db
}

func TestRepairPlanBaselineRecreatesSameNameIndexWithWrongShape(t *testing.T) {
	definitions := planBaselineDefinitionsForTest()
	definitions["plan_index_drop_idx"] =
		"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data (index_drop_key)"
	state := &planBaselineCatalogState{definitions: definitions}
	db := newPlanBaselineCatalogDatabase(t, state)

	if _, err := RepairPlanBaseline(context.Background(), db, "gsbench"); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	executed := append([]planBaselineExecutedSQL(nil), state.executed...)
	state.mu.Unlock()
	var dropAt, createAt = -1, -1
	for index, event := range executed {
		switch compactPlanDDL(event.statement) {
		case compactPlanDDL("DROP INDEX gsbench.plan_index_drop_idx"):
			dropAt = index
		case compactPlanDDL(
			"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data (index_drop_key,dist_key,id)",
		):
			createAt = index
		}
	}
	if dropAt < 0 || createAt <= dropAt {
		t.Fatalf("wrong-shape repair SQL=%v", executed)
	}

	wantSession := []string{
		"SET default_statistics_target=-2",
		"ANALYZE gsbench.plan_data ((stats_corr_a,stats_corr_b))",
		"RESET default_statistics_target",
	}
	for start := 0; start+len(wantSession) <= len(executed); start++ {
		matched := true
		connID := executed[start].connID
		for offset, want := range wantSession {
			event := executed[start+offset]
			if event.connID != connID ||
				compactPlanDDL(event.statement) != compactPlanDDL(want) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("extended statistics analyze did not use one session: %v", executed)
}

func TestVerifyPlanBaselineRejectsSameNameIndexWithWrongShape(t *testing.T) {
	definitions := planBaselineDefinitionsForTest()
	definitions["plan_index_drop_idx"] =
		"CREATE INDEX plan_index_drop_idx ON gsbench.plan_data (index_drop_key)"
	state := &planBaselineCatalogState{definitions: definitions}
	db := newPlanBaselineCatalogDatabase(t, state)

	err := VerifyPlanBaseline(context.Background(), db, "gsbench")
	if err == nil || !strings.Contains(err.Error(), "plan_index_drop_idx") ||
		!strings.Contains(err.Error(), "definition") {
		t.Fatalf("VerifyPlanBaseline() error=%v", err)
	}
}

func TestPlanBaselineRepairRejectsUnsafeSchema(t *testing.T) {
	if _, err := PlanBaselineRepairSteps("gsbench;drop schema public"); err == nil {
		t.Fatal("expected unsafe schema error")
	}
}
