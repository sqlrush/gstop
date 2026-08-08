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
	ScenarioCode ScenarioCode
	Signature    string
	SQLText      string
	PlanText     string
}

type workloadPlanStore interface {
	Explain(context.Context, string) (string, error)
	Cache(context.Context, ScenarioCode, string, string, string) error
}

type databaseWorkloadPlanStore struct {
	runtime *Runtime
}

type planCacheMutation struct {
	deleteSQL    string
	insertSQL    string
	scenarioCode ScenarioCode
}

func planCacheMutationFor(
	schema string,
	scenarioCode ScenarioCode,
) (planCacheMutation, error) {
	quotedSchema, ok := quoteDatasetSchema(schema)
	if !ok {
		return planCacheMutation{}, fmt.Errorf(
			"unsafe dataset schema %q",
			schema,
		)
	}
	if _, err := DefaultScenarioCatalog().LookupCode(scenarioCode); err != nil {
		return planCacheMutation{}, err
	}
	table := quotedSchema + ".meta_plan_cache"
	return planCacheMutation{
		deleteSQL: "DELETE FROM " + table +
			" WHERE signature=$1 AND scenario_code=$2",
		insertSQL: "INSERT INTO " + table +
			"(signature,scenario_code,sql_text,plan_text) VALUES($1,$2,$3,$4)",
		scenarioCode: scenarioCode,
	}, nil
}

func (s databaseWorkloadPlanStore) Explain(ctx context.Context, sqlText string) (string, error) {
	return explainLiteral(ctx, s.runtime.Database, sqlText)
}

func (s databaseWorkloadPlanStore) Cache(
	ctx context.Context,
	scenarioCode ScenarioCode,
	signature, sqlText, planText string,
) error {
	mutation, err := planCacheMutationFor(
		s.runtime.Config.Data.Schema,
		scenarioCode,
	)
	if err != nil {
		return err
	}
	return replaceCachedWorkloadPlan(
		ctx,
		databaseDatasetTransactionBeginner{db: s.runtime.Database},
		mutation,
		signature,
		sqlText,
		planText,
	)
}

func replaceCachedWorkloadPlan(
	ctx context.Context,
	beginner datasetTransactionBeginner,
	mutation planCacheMutation,
	signature, sqlText, planText string,
) error {
	tx, err := beginner.BeginDatasetTransaction(ctx)
	if err != nil {
		return fmt.Errorf("begin plan cache transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(
		ctx,
		mutation.deleteSQL,
		signature,
		int(mutation.scenarioCode),
	); err != nil {
		return fmt.Errorf("delete existing plan: %w", err)
	}
	if _, err := tx.ExecContext(
		ctx,
		mutation.insertSQL,
		signature,
		int(mutation.scenarioCode),
		sqlText,
		planText,
	); err != nil {
		return fmt.Errorf("insert plan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit plan cache transaction: %w", err)
	}
	committed = true
	return nil
}

// InspectWorkloadPlans checks that every workload statement is literal and
// explainable without mutating the gsbench plan cache or database metadata.
func InspectWorkloadPlans(
	ctx context.Context,
	runtime *Runtime,
	scenario string,
	sqls []string,
) error {
	if runtime == nil || runtime.Database == nil {
		return fmt.Errorf("scenario %s plan inspection database is unavailable", scenario)
	}
	catalog := runtime.Catalog
	if catalog == nil {
		catalog = DefaultScenarioCatalog()
	}
	definition, err := catalog.Resolve(scenario)
	if err != nil {
		return fmt.Errorf("resolve plan-inspection scenario %q: %w", scenario, err)
	}
	return inspectWorkloadPlans(
		ctx,
		databaseWorkloadPlanStore{runtime: runtime},
		definition.Code,
		sqls,
	)
}

func inspectWorkloadPlans(
	ctx context.Context,
	store workloadPlanStore,
	scenarioCode ScenarioCode,
	sqls []string,
) error {
	for _, sqlText := range uniqueSQLTexts(sqls) {
		if workloadBindMarkerRE.MatchString(sqlText) {
			return fmt.Errorf(
				"scenario %03d workload SQL contains bind marker: %s",
				scenarioCode,
				sqlText,
			)
		}
		planText, err := store.Explain(ctx, sqlText)
		if err != nil {
			return fmt.Errorf(
				"scenario %03d explain workload: %w",
				scenarioCode,
				err,
			)
		}
		if !looksLikeWorkloadPlan(planText) {
			return fmt.Errorf(
				"scenario %03d returned empty or unparseable plan",
				scenarioCode,
			)
		}
	}
	return nil
}

// EnsureWorkloadPlans is retained for compatibility with explicit callers
// that intentionally persist plan snapshots. Scenario preflight uses the
// read-only InspectWorkloadPlans path.
func EnsureWorkloadPlans(
	ctx context.Context,
	runtime *Runtime,
	scenario string,
	sqls []string,
) error {
	if runtime == nil || runtime.Database == nil {
		return fmt.Errorf("scenario %s plan preflight database is unavailable", scenario)
	}
	catalog := runtime.Catalog
	if catalog == nil {
		catalog = DefaultScenarioCatalog()
	}
	definition, err := catalog.Resolve(scenario)
	if err != nil {
		return fmt.Errorf("resolve plan-cache scenario %q: %w", scenario, err)
	}
	return ensureWorkloadPlans(
		ctx,
		databaseWorkloadPlanStore{runtime: runtime},
		definition.Code,
		sqls,
	)
}

func ensureWorkloadPlans(
	ctx context.Context,
	store workloadPlanStore,
	scenarioCode ScenarioCode,
	sqls []string,
) error {
	for _, sqlText := range uniqueSQLTexts(sqls) {
		if workloadBindMarkerRE.MatchString(sqlText) {
			return fmt.Errorf(
				"scenario %03d workload SQL contains bind marker: %s",
				scenarioCode,
				sqlText,
			)
		}
		planText, err := store.Explain(ctx, sqlText)
		if err != nil {
			return fmt.Errorf(
				"scenario %03d explain workload: %w",
				scenarioCode,
				err,
			)
		}
		if !looksLikeWorkloadPlan(planText) {
			return fmt.Errorf(
				"scenario %03d returned empty or unparseable plan",
				scenarioCode,
			)
		}
		signature := sqlshape.Signature(sqlText)
		if err := store.Cache(
			ctx,
			scenarioCode,
			signature,
			sqlText,
			planText,
		); err != nil {
			return fmt.Errorf(
				"scenario %03d cache workload plan: %w",
				scenarioCode,
				err,
			)
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
