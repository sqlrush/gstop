package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gstop/internal/sqlshape"
)

type fakeWorkloadPlanStore struct {
	plans      map[string]string
	explainErr error
	cacheErr   error
	explained  []string
	cached     []cachedWorkloadPlan
}

func (s *fakeWorkloadPlanStore) Explain(_ context.Context, sqlText string) (string, error) {
	s.explained = append(s.explained, sqlText)
	if s.explainErr != nil {
		return "", s.explainErr
	}
	return s.plans[sqlText], nil
}

func (s *fakeWorkloadPlanStore) Cache(
	_ context.Context,
	scenarioCode ScenarioCode,
	signature, sqlText, planText string,
) error {
	if s.cacheErr != nil {
		return s.cacheErr
	}
	s.cached = append(s.cached, cachedWorkloadPlan{
		ScenarioCode: scenarioCode,
		Signature:    signature,
		SQLText:      sqlText,
		PlanText:     planText,
	})
	return nil
}

type recordingWorkloadPlanCacheTransaction struct {
	queries    []string
	args       [][]any
	committed  bool
	rolledBack bool
}

func (t *recordingWorkloadPlanCacheTransaction) ExecContext(
	_ context.Context,
	query string,
	args ...any,
) (sql.Result, error) {
	t.queries = append(t.queries, query)
	t.args = append(t.args, append([]any(nil), args...))
	return nil, nil
}

func (t *recordingWorkloadPlanCacheTransaction) Commit() error {
	t.committed = true
	return nil
}

func (t *recordingWorkloadPlanCacheTransaction) Rollback() error {
	t.rolledBack = true
	return nil
}

type recordingWorkloadPlanCacheBeginner struct {
	tx *recordingWorkloadPlanCacheTransaction
}

func (b recordingWorkloadPlanCacheBeginner) BeginDatasetTransaction(
	context.Context,
) (datasetSQLTransaction, error) {
	return b.tx, nil
}

func TestReplaceCachedWorkloadPlanUsesScenarioCodeSchemaContract(t *testing.T) {
	mutation, err := planCacheMutationFor("Bench", ScenarioCode(601))
	if err != nil {
		t.Fatal(err)
	}
	tx := &recordingWorkloadPlanCacheTransaction{}
	if err := replaceCachedWorkloadPlan(
		context.Background(),
		recordingWorkloadPlanCacheBeginner{tx: tx},
		mutation,
		"signature-1",
		"SELECT 1",
		"Result  (cost=0.00..1.00 rows=1 width=4)",
	); err != nil {
		t.Fatal(err)
	}
	wantQueries := []string{
		`DELETE FROM "Bench".meta_plan_cache WHERE signature=$1 AND scenario_code=$2`,
		`INSERT INTO "Bench".meta_plan_cache(signature,scenario_code,sql_text,plan_text) VALUES($1,$2,$3,$4)`,
	}
	if !reflect.DeepEqual(tx.queries, wantQueries) {
		t.Fatalf("cache queries=%q want=%q", tx.queries, wantQueries)
	}
	if len(tx.args) != 2 {
		t.Fatalf("cache args=%v", tx.args)
	}
	for index, args := range tx.args {
		if len(args) < 2 {
			t.Fatalf("cache call %d args=%v", index+1, args)
		}
		code, ok := args[1].(int)
		if !ok || code != 601 {
			t.Fatalf(
				"cache call %d scenario argument=%#v (%T), want int(601)",
				index+1,
				args[1],
				args[1],
			)
		}
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf(
			"cache transaction committed=%v rolled_back=%v",
			tx.committed,
			tx.rolledBack,
		)
	}
}

func TestEnsureWorkloadPlansCachesEachUniqueSQLShape(t *testing.T) {
	first := "SELECT * FROM gsbench.accounts WHERE id=42"
	second := "SELECT * FROM gsbench.accounts WHERE id=43"
	store := &fakeWorkloadPlanStore{plans: map[string]string{
		first:  "Index Scan  (cost=0.00..1.00 rows=1 width=8)",
		second: "Index Scan  (cost=0.00..1.00 rows=1 width=8)",
	}}
	if err := ensureWorkloadPlans(
		context.Background(), store, ScenarioCode(101), []string{first, first, second},
	); err != nil {
		t.Fatal(err)
	}
	if len(store.explained) != 2 || len(store.cached) != 2 {
		t.Fatalf("explained=%v cached=%+v", store.explained, store.cached)
	}
	for _, cached := range store.cached {
		if cached.Signature != sqlshape.Signature(cached.SQLText) ||
			cached.ScenarioCode != 101 {
			t.Fatalf("cached=%+v", cached)
		}
	}
}

func TestEnsureWorkloadPlansRejectsEmptyOrUnparseablePlan(t *testing.T) {
	for name, plan := range map[string]string{
		"empty": "", "unparseable": "NOTICE: optimizer unavailable",
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeWorkloadPlanStore{plans: map[string]string{"SELECT 1": plan}}
			err := ensureWorkloadPlans(
				context.Background(), store, ScenarioCode(403), []string{"SELECT 1"},
			)
			if err == nil || !strings.Contains(err.Error(), "empty or unparseable plan") {
				t.Fatalf("err=%v", err)
			}
			if len(store.cached) != 0 {
				t.Fatalf("invalid plan cached: %+v", store.cached)
			}
		})
	}
}

func TestEnsureWorkloadPlansRejectsBindMarkersBeforeExplain(t *testing.T) {
	for _, sqlText := range []string{
		"SELECT * FROM t WHERE id=$1",
		"SELECT * FROM t WHERE id=?",
		"SELECT * FROM t WHERE id=:id",
	} {
		store := &fakeWorkloadPlanStore{}
		err := ensureWorkloadPlans(
			context.Background(), store, ScenarioCode(101), []string{sqlText},
		)
		if err == nil || !strings.Contains(err.Error(), "bind marker") {
			t.Fatalf("sql=%s err=%v", sqlText, err)
		}
		if len(store.explained) != 0 {
			t.Fatalf("bound SQL explained: %v", store.explained)
		}
	}
}

func TestEnsureWorkloadPlansPropagatesExplainAndCacheFailures(t *testing.T) {
	explainStore := &fakeWorkloadPlanStore{explainErr: errors.New("explain denied")}
	if err := ensureWorkloadPlans(
		context.Background(), explainStore, ScenarioCode(101), []string{"SELECT 1"},
	); err == nil || !strings.Contains(err.Error(), "explain denied") {
		t.Fatalf("explain err=%v", err)
	}

	cacheStore := &fakeWorkloadPlanStore{
		plans:    map[string]string{"SELECT 1": "Result  (cost=0.00..1.00 rows=1 width=4)"},
		cacheErr: errors.New("cache denied"),
	}
	if err := ensureWorkloadPlans(
		context.Background(), cacheStore, ScenarioCode(101), []string{"SELECT 1"},
	); err == nil || !strings.Contains(err.Error(), "cache denied") {
		t.Fatalf("cache err=%v", err)
	}
}

func TestScenarioWorkloadStatementsCoverEveryRegisteredScenario(t *testing.T) {
	runtime := &Runtime{Config: BenchConfig{
		Data:   DataConfig{Schema: "gsbench"},
		Safety: SafetyConfig{MaxWorkers: 640},
	}}
	// These are the catalog entries backed by workload SQL in this stage. Later
	// scenario-family tasks extend this list as they add their factories.
	for _, code := range []ScenarioCode{101, 102, 103, 401, 402, 801} {
		definition := DefaultScenarioCatalog().MustCode(code)
		t.Run(definition.Name, func(t *testing.T) {
			sqls, err := ScenarioWorkloadStatements(runtime, definition.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(sqls) == 0 {
				t.Fatal("scenario has no declared plannable workload SQL")
			}
			for _, sqlText := range sqls {
				if workloadBindMarkerRE.MatchString(sqlText) {
					t.Fatalf("workload SQL contains bind marker: %s", sqlText)
				}
			}
		})
	}
}
