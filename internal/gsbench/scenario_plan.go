package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

func explainLiteral(ctx context.Context, db *Database, sqlText string) (string, error) {
	rows, err := db.Query(ctx, "EXPLAIN "+sqlText)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	return scanExplainRows(rows.Rows)
}

func scanExplainRows(rows *sql.Rows) (string, error) {
	columns, err := rows.Columns()
	if err != nil {
		return "", err
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for index := range values {
		destinations[index] = &values[index]
	}

	var lines []string
	for rows.Next() {
		if err := rows.Scan(destinations...); err != nil {
			return "", err
		}
		fields := make([]string, len(values))
		for index, value := range values {
			switch value := value.(type) {
			case nil:
				fields[index] = "NULL"
			case []byte:
				fields[index] = string(value)
			default:
				fields[index] = fmt.Sprint(value)
			}
		}
		lines = append(lines, strings.Join(fields, "\t"))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func ObserveLiteralPlan(ctx context.Context, db *Database, sqlText string, samples int) (PlanObservation, error) {
	planText, err := explainLiteral(ctx, db, sqlText)
	if err != nil {
		return PlanObservation{}, err
	}
	if samples < 1 {
		samples = 1
	}
	var durations []time.Duration
	var fingerprint string
	for i := 0; i < samples+1; i++ {
		started := time.Now()
		var count, sum string
		if err := db.Scan(ctx, sqlText, nil, &count, &sum); err != nil {
			return PlanObservation{}, err
		}
		if i > 0 {
			durations = append(durations, time.Since(started))
		}
		fingerprint = count + ":" + sum
	}
	return NewPlanObservation(sqlText, planText, fingerprint, durations), nil
}

func literalPlanOp(sqlText string) SQLWorkerOp {
	return func(ctx context.Context, conn *sql.Conn, _ int) error {
		rows, err := conn.QueryContext(ctx, sqlText)
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	}
}

type PlanCoordinator struct {
	mu sync.Mutex
}

func (c *PlanCoordinator) Exclusive(fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fn()
}

type PlanChangeScenario struct {
	def         PlanScenarioDefinition
	coordinator *PlanCoordinator
	baselines   []PlanObservation
	baseline    PlanObservation
	changed     PlanObservation
	minimum     float64
	samples     int
	workers     int
	workload    *sqlWorkload
}

func NewPlanChangeScenario(def PlanScenarioDefinition, coordinator *PlanCoordinator) *PlanChangeScenario {
	if coordinator == nil {
		coordinator = &PlanCoordinator{}
	}
	return &PlanChangeScenario{def: def, coordinator: coordinator}
}

func (s *PlanChangeScenario) Code() ScenarioCode { return s.def.Code }
func (s *PlanChangeScenario) Name() string       { return s.def.Name }

func minimumPlanDataRows(string) int64 {
	return planDataMinRows
}

func (s *PlanChangeScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if err := validatePlanCapability(s.Name(), rt.Capabilities); err != nil {
		return err
	}
	s.minimum = runtimeFloat(rt, "scenario."+s.Name()+".minimum_slowdown",
		runtimeFloat(rt, "scenario.plan_change.minimum_slowdown",
			runtimeFloat(rt, "scenario.plan_regression.minimum_slowdown", 2)))
	s.samples = runtimeInt(rt, "scenario."+s.Name()+".samples",
		runtimeInt(rt, "scenario.plan_change.samples", 5))
	s.workers = runtimeInt(rt, "scenario."+s.Name()+".workers",
		runtimeInt(rt, "scenario.plan_change.workers", 1))
	if s.workers < 1 {
		s.workers = 1
	}
	if maximum := rt.Config.Safety.MaxWorkers; maximum > 0 && s.workers > maximum {
		s.workers = maximum
	}

	minimumRows := minimumPlanDataRows(rt.Config.Run.Profile)
	quotedSchema, ok := quoteDatasetSchema(rt.Config.Data.Schema)
	if !ok {
		return fmt.Errorf("unsafe schema %q", rt.Config.Data.Schema)
	}
	var rows int64
	if err := rt.Database.Scan(ctx,
		"SELECT high_water FROM "+quotedSchema+
			".meta_batches WHERE table_name='plan_data'",
		nil, &rows); err != nil {
		return fmt.Errorf("read plan_data high-water: %w", err)
	}
	if rows < minimumRows {
		return fmt.Errorf(
			"plan_data has %d rows; need at least %d for profile %s; run gsbench init",
			rows, minimumRows, rt.Config.Run.Profile,
		)
	}

	if len(s.def.Candidates) == 0 {
		definitions, err := PlanScenarioDefinitions(rt.Config.Data.Schema)
		if err != nil {
			return err
		}
		for _, definition := range definitions {
			if definition.Code == s.def.Code && definition.Name == s.def.Name {
				s.def = definition
				break
			}
		}
		if len(s.def.Candidates) == 0 {
			return fmt.Errorf("plan scenario definition %03d is unavailable", s.def.Code)
		}
	}
	s.baselines = nil
	for _, sqlText := range s.def.Candidates {
		observation, err := ObserveLiteralPlan(ctx, rt.Database, sqlText, s.samples)
		if err != nil {
			return fmt.Errorf("observe baseline candidate: %w", err)
		}
		s.baselines = append(s.baselines, observation)
	}
	return nil
}

func (s *PlanChangeScenario) Ramp(ctx context.Context, rt *Runtime) error {
	if rt.Journal == nil {
		return fmt.Errorf("mutation journal is unavailable")
	}
	return s.coordinator.Exclusive(func() error {
		mutations, err := PlanMutationSet(rt.RunID, rt.Config.Data.Schema, s.Name())
		if err != nil {
			return err
		}
		for _, mutation := range mutations {
			if err := rt.Journal.Apply(ctx, mutation); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PlanChangeScenario) Hold(ctx context.Context, rt *Runtime) error {
	changedPlans := make(map[string]string, len(s.baselines))
	for _, baseline := range s.baselines {
		planText, err := explainLiteral(ctx, rt.Database, baseline.SQL)
		if err != nil {
			return err
		}
		changedPlans[baseline.SQL] = planText
	}
	baseline, _, err := SelectChangedCandidate(s.baselines, changedPlans)
	if err != nil {
		return err
	}
	s.baseline = baseline
	if rt.Config.Run.ValidationEnabled && rt.PlanPreflight != nil {
		if err := rt.PlanPreflight(ctx, s.Name(), []string{baseline.SQL}); err != nil {
			return fmt.Errorf("refresh changed workload plan: %w", err)
		}
	}
	s.changed, err = ObserveLiteralPlan(ctx, rt.Database, baseline.SQL, s.samples)
	if err != nil {
		return err
	}
	if rt.Log != nil {
		rt.Log.Info("scenario=%s literal_sql=%s", s.Name(), baseline.SQL)
		rt.Log.Info("scenario=%s baseline_signature=%q changed_signature=%q",
			s.Name(), baseline.StructureSignature, s.changed.StructureSignature)
		rt.Log.Info("scenario=%s baseline_plan=%s", s.Name(), baseline.PlanText)
		rt.Log.Info("scenario=%s changed_plan=%s", s.Name(), s.changed.PlanText)
	}
	s.workload = newSQLWorkload(ctx, rt, s.Name(), s.workers, literalPlanOp(baseline.SQL))
	if err := s.workload.SetTarget(s.workers); err != nil {
		return err
	}
	return waitContext(ctx, rt.Config.Run.Duration)
}

func (s *PlanChangeScenario) Verify(context.Context, *Runtime) (Result, error) {
	return EvaluatePlanChange(s.Name(), s.baseline, s.changed, s.minimum), nil
}

func (s *PlanChangeScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}

func (s *PlanChangeScenario) Restore(context.Context, *Runtime) error {
	return nil
}
