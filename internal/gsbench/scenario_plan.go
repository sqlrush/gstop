package gsbench

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"time"
)

type PlanObservation struct {
	PlanHash          string
	PlanText          string
	ResultFingerprint string
	Median            time.Duration
}

func EvaluatePlanRegression(baseline, changed PlanObservation, minimumSlowdown float64) Result {
	result := Result{Scenario: "plan_regression", Outcome: OutcomeFailed}
	if baseline.PlanHash == changed.PlanHash {
		result.Message = "execution plan did not change"
		return result
	}
	if baseline.ResultFingerprint != changed.ResultFingerprint {
		result.Message = "changed plan returned a different result fingerprint"
		return result
	}
	if baseline.Median <= 0 || float64(changed.Median)/float64(baseline.Median) < minimumSlowdown {
		result.Message = fmt.Sprintf("plan changed but slowdown was %.2fx, below %.2fx", float64(changed.Median)/float64(baseline.Median), minimumSlowdown)
		return result
	}
	result.Outcome = OutcomeSuccess
	result.Message = fmt.Sprintf("plan changed and median elapsed time regressed %.2fx", float64(changed.Median)/float64(baseline.Median))
	result.Evidence = []Evidence{{Metric: "slowdown_factor", Target: minimumSlowdown, Actual: float64(changed.Median) / float64(baseline.Median), Available: true}}
	return result
}

func PlanMutation(runID, schema, trigger string) (Mutation, error) {
	if !identifierRE.MatchString(schema) {
		return Mutation{}, fmt.Errorf("unsafe schema %q", schema)
	}
	base := Mutation{RunID: runID, Scenario: "plan_regression", Kind: trigger}
	switch trigger {
	case "index_unusable":
		base.Target = schema + ".plan_data_lookup_idx"
		base.ForwardSQL = "ALTER INDEX " + base.Target + " UNUSABLE"
		base.InverseSQL = "ALTER INDEX " + base.Target + " REBUILD"
		base.VerifySQL = `SELECT b.indisusable FROM pg_stat_user_indexes a JOIN pg_index b ON a.indexrelid=b.indexrelid WHERE a.schemaname='` + schema + `' AND a.indexrelname='plan_data_lookup_idx'`
		base.VerifyValue = "true"
	case "stats_skew":
		base.Target = schema + ".plan_data.skew_key"
		base.ForwardSQL = "UPDATE " + schema + ".plan_data SET skew_key=1; ANALYZE " + schema + ".plan_data"
		base.InverseSQL = "UPDATE " + schema + ".plan_data SET skew_key=CASE WHEN mod(id,100)<95 THEN 1 ELSE mod(id,1000) END; ANALYZE " + schema + ".plan_data"
		base.VerifySQL = "SELECT count(*) FROM " + schema + ".plan_data WHERE skew_key<>CASE WHEN mod(id,100)<95 THEN 1 ELSE mod(id,1000) END"
		base.VerifyValue = "0"
	case "hard_parse":
		base.Target = schema + ".plan_data.lookup_key statistics"
		base.ForwardSQL = "ALTER TABLE " + schema + ".plan_data ALTER COLUMN lookup_key SET STATISTICS 1; ANALYZE " + schema + ".plan_data"
		base.InverseSQL = "ALTER TABLE " + schema + ".plan_data ALTER COLUMN lookup_key SET STATISTICS -1; ANALYZE " + schema + ".plan_data"
		base.VerifySQL = `SELECT attstattarget FROM pg_attribute a JOIN pg_class c ON a.attrelid=c.oid JOIN pg_namespace n ON c.relnamespace=n.oid WHERE n.nspname='` + schema + `' AND c.relname='plan_data' AND a.attname='lookup_key'`
		base.VerifyValue = "-1"
	default:
		return Mutation{}, fmt.Errorf("unknown plan trigger %q", trigger)
	}
	return base, nil
}

type PlanScenario struct {
	baseline PlanObservation
	changed  PlanObservation
	minimum  float64
	trigger  string
}

func NewPlanScenario() *PlanScenario { return &PlanScenario{} }
func (s *PlanScenario) Name() string { return "plan_regression" }
func (s *PlanScenario) Prepare(ctx context.Context, rt *Runtime) error {
	s.minimum = runtimeFloat(rt, "scenario.plan_regression.minimum_slowdown", 2.0)
	s.trigger = runtimeString(rt, "scenario.plan_regression.trigger", "index_unusable")
	observation, err := observePlan(ctx, rt.Database, rt.Config.Data.Schema)
	if err != nil {
		return err
	}
	s.baseline = observation
	return nil
}
func (s *PlanScenario) Ramp(ctx context.Context, rt *Runtime) error {
	if rt.Journal == nil {
		return fmt.Errorf("mutation journal is unavailable")
	}
	mutation, err := PlanMutation(rt.RunID, rt.Config.Data.Schema, s.trigger)
	if err != nil {
		return err
	}
	return rt.Journal.Apply(ctx, mutation)
}
func (s *PlanScenario) Hold(ctx context.Context, rt *Runtime) error {
	observation, err := observePlan(ctx, rt.Database, rt.Config.Data.Schema)
	if err != nil {
		return err
	}
	s.changed = observation
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *PlanScenario) Verify(context.Context, *Runtime) (Result, error) {
	return EvaluatePlanRegression(s.baseline, s.changed, s.minimum), nil
}
func (s *PlanScenario) Stop(context.Context, *Runtime) error { return nil }
func (s *PlanScenario) Restore(ctx context.Context, rt *Runtime) error {
	if rt.Journal == nil {
		return nil
	}
	return rt.Journal.RestoreScenario(ctx, rt.RunID, s.Name())
}

func observePlan(ctx context.Context, db *Database, schema string) (PlanObservation, error) {
	query := "SELECT count(*),sum(id) FROM " + schema + ".plan_data WHERE lookup_key BETWEEN 1000 AND 50000"
	rows, err := db.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		return PlanObservation{}, err
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return PlanObservation{}, err
		}
		lines = append(lines, line)
	}
	if err := rows.Close(); err != nil {
		return PlanObservation{}, err
	}
	planText := strings.Join(lines, "\n")
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(planText)))
	var durations []time.Duration
	var fingerprint string
	for range 3 {
		started := time.Now()
		var count, sum string
		if err := db.Scan(ctx, query, nil, &count, &sum); err != nil {
			return PlanObservation{}, err
		}
		durations = append(durations, time.Since(started))
		fingerprint = count + ":" + sum
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return PlanObservation{PlanHash: hash, PlanText: planText, ResultFingerprint: fingerprint, Median: durations[len(durations)/2]}, nil
}
