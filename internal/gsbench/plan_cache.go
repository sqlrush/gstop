package gsbench

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"gstop/internal/sqlshape"
)

var workloadBindMarkerRE = regexp.MustCompile(`\$\d+|\?|(^|[^:]):[A-Za-z_][A-Za-z0-9_]*`)

type cachedWorkloadPlan struct {
	Scenario  string
	Signature string
	SQLText   string
	PlanText  string
}

type workloadPlanStore interface {
	Explain(context.Context, string) (string, error)
	Cache(context.Context, string, string, string, string) error
}

type databaseWorkloadPlanStore struct {
	runtime *Runtime
}

func (s databaseWorkloadPlanStore) Explain(ctx context.Context, sqlText string) (string, error) {
	return explainLiteral(ctx, s.runtime.Database, sqlText)
}

func (s databaseWorkloadPlanStore) Cache(
	ctx context.Context,
	scenario, signature, sqlText, planText string,
) error {
	schema := s.runtime.Config.Data.Schema
	if _, err := s.runtime.Database.Exec(
		ctx,
		"DELETE FROM "+schema+".meta_plan_cache WHERE signature=$1 AND scenario=$2",
		signature,
		scenario,
	); err != nil {
		return fmt.Errorf("delete existing plan: %w", err)
	}
	if _, err := s.runtime.Database.Exec(
		ctx,
		"INSERT INTO "+schema+
			".meta_plan_cache(signature,scenario,sql_text,plan_text) VALUES($1,$2,$3,$4)",
		signature,
		scenario,
		sqlText,
		planText,
	); err != nil {
		return fmt.Errorf("insert plan: %w", err)
	}
	return nil
}

// EnsureWorkloadPlans proves every workload statement is literal, explainable,
// and durably cached before a scenario starts executing it.
func EnsureWorkloadPlans(
	ctx context.Context,
	runtime *Runtime,
	scenario string,
	sqls []string,
) error {
	if runtime == nil || runtime.Database == nil {
		return fmt.Errorf("scenario %s plan preflight database is unavailable", scenario)
	}
	return ensureWorkloadPlans(
		ctx,
		databaseWorkloadPlanStore{runtime: runtime},
		scenario,
		sqls,
	)
}

func ensureWorkloadPlans(
	ctx context.Context,
	store workloadPlanStore,
	scenario string,
	sqls []string,
) error {
	for _, sqlText := range uniqueSQLTexts(sqls) {
		if workloadBindMarkerRE.MatchString(sqlText) {
			return fmt.Errorf("scenario %s workload SQL contains bind marker: %s", scenario, sqlText)
		}
		planText, err := store.Explain(ctx, sqlText)
		if err != nil {
			return fmt.Errorf("scenario %s explain workload: %w", scenario, err)
		}
		if !looksLikeWorkloadPlan(planText) {
			return fmt.Errorf("scenario %s returned empty or unparseable plan", scenario)
		}
		signature := sqlshape.Signature(sqlText)
		if err := store.Cache(ctx, scenario, signature, sqlText, planText); err != nil {
			return fmt.Errorf("scenario %s cache workload plan: %w", scenario, err)
		}
	}
	return nil
}

func uniqueSQLTexts(sqls []string) []string {
	seen := make(map[string]bool, len(sqls))
	out := make([]string, 0, len(sqls))
	for _, sqlText := range sqls {
		sqlText = strings.TrimSpace(sqlText)
		if sqlText == "" || seen[sqlText] {
			continue
		}
		seen[sqlText] = true
		out = append(out, sqlText)
	}
	return out
}

func looksLikeWorkloadPlan(planText string) bool {
	return strings.Contains(strings.ToLower(planText), "cost=")
}
