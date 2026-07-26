package gsbench

import (
	"context"
	"errors"
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
	scenario, signature, sqlText, planText string,
) error {
	if s.cacheErr != nil {
		return s.cacheErr
	}
	s.cached = append(s.cached, cachedWorkloadPlan{
		Scenario: scenario, Signature: signature, SQLText: sqlText, PlanText: planText,
	})
	return nil
}

func TestEnsureWorkloadPlansCachesEachUniqueSQLShape(t *testing.T) {
	first := "SELECT * FROM gsbench.accounts WHERE id=42"
	second := "SELECT * FROM gsbench.accounts WHERE id=43"
	store := &fakeWorkloadPlanStore{plans: map[string]string{
		first:  "Index Scan  (cost=0.00..1.00 rows=1 width=8)",
		second: "Index Scan  (cost=0.00..1.00 rows=1 width=8)",
	}}
	if err := ensureWorkloadPlans(
		context.Background(), store, "tp_cpu", []string{first, first, second},
	); err != nil {
		t.Fatal(err)
	}
	if len(store.explained) != 2 || len(store.cached) != 2 {
		t.Fatalf("explained=%v cached=%+v", store.explained, store.cached)
	}
	for _, cached := range store.cached {
		if cached.Signature != sqlshape.Signature(cached.SQLText) ||
			cached.Scenario != "tp_cpu" {
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
				context.Background(), store, "connection_pool", []string{"SELECT 1"},
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
			context.Background(), store, "tp_cpu", []string{sqlText},
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
		context.Background(), explainStore, "tp_cpu", []string{"SELECT 1"},
	); err == nil || !strings.Contains(err.Error(), "explain denied") {
		t.Fatalf("explain err=%v", err)
	}

	cacheStore := &fakeWorkloadPlanStore{
		plans:    map[string]string{"SELECT 1": "Result  (cost=0.00..1.00 rows=1 width=4)"},
		cacheErr: errors.New("cache denied"),
	}
	if err := ensureWorkloadPlans(
		context.Background(), cacheStore, "tp_cpu", []string{"SELECT 1"},
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
