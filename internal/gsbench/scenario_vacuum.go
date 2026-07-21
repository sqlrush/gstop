package gsbench

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

func VacuumCommand(schema, mode string, allowFull bool) (string, error) {
	if !identifierRE.MatchString(schema) {
		return "", fmt.Errorf("unsafe schema %q", schema)
	}
	switch strings.ToLower(mode) {
	case "vacuum":
		return "VACUUM " + schema + ".vacuum_targets", nil
	case "analyze":
		return "VACUUM ANALYZE " + schema + ".vacuum_targets", nil
	case "full":
		if !allowFull {
			return "", fmt.Errorf("VACUUM FULL requires allow_vacuum_full=true")
		}
		return "VACUUM FULL " + schema + ".vacuum_targets", nil
	default:
		return "", fmt.Errorf("unknown vacuum mode %q", mode)
	}
}

func EvaluateVacuumRegression(baseline, foreground time.Duration, minimum float64, started bool) Result {
	result := Result{Scenario: "vacuum_pressure", Outcome: OutcomeFailed}
	if !started || baseline <= 0 {
		result.Message = "vacuum did not start or baseline is unavailable"
		return result
	}
	factor := float64(foreground) / float64(baseline)
	result.Evidence = []Evidence{{Metric: "foreground_slowdown", Target: minimum, Actual: factor, Available: true}}
	if factor >= minimum {
		result.Outcome = OutcomeSuccess
		result.Message = fmt.Sprintf("foreground median regressed %.2fx during vacuum", factor)
	} else {
		result.Message = fmt.Sprintf("foreground regression %.2fx is below %.2fx", factor, minimum)
	}
	return result
}

type VacuumScenario struct {
	baseline   time.Duration
	foreground []time.Duration
	command    string
	conn       *TaggedConn
	cancel     context.CancelFunc
	done       chan error
	started    bool
	minimum    float64
}

func NewVacuumScenario() *VacuumScenario { return &VacuumScenario{} }
func (s *VacuumScenario) Name() string   { return "vacuum_pressure" }
func (s *VacuumScenario) Prepare(ctx context.Context, rt *Runtime) error {
	mode := runtimeString(rt, "scenario.vacuum_pressure.mode", "vacuum")
	cmd, err := VacuumCommand(rt.Config.Data.Schema, mode, runtimeBool(rt, "scenario.vacuum_pressure.allow_vacuum_full", false))
	if err != nil {
		return err
	}
	s.command = cmd
	s.minimum = runtimeFloat(rt, "scenario.vacuum_pressure.minimum_slowdown", 1.5)
	s.baseline, err = measureVacuumForeground(ctx, rt.Database, rt.Config.Data.Schema)
	if err != nil {
		return err
	}
	if rt.Journal == nil {
		return fmt.Errorf("mutation journal is unavailable")
	}
	mutation := Mutation{RunID: rt.RunID, Scenario: s.Name(), Kind: "vacuum_churn", Target: rt.Config.Data.Schema + ".vacuum_targets", ForwardSQL: "UPDATE " + rt.Config.Data.Schema + ".vacuum_targets SET version=version+1,payload=payload||'x',updated_at=current_timestamp", InverseSQL: "UPDATE " + rt.Config.Data.Schema + ".vacuum_targets SET version=0,payload=repeat('v',900),updated_at=current_timestamp", VerifySQL: "SELECT count(*) FROM " + rt.Config.Data.Schema + ".vacuum_targets WHERE version<>0", VerifyValue: "0"}
	return rt.Journal.Apply(ctx, mutation)
}
func (s *VacuumScenario) Ramp(ctx context.Context, rt *Runtime) error {
	conn, err := rt.Database.OpenTagged(ctx, rt.RunID, s.Name(), "vacuum")
	if err != nil {
		return err
	}
	s.conn = conn
	vacCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan error, 1)
	s.started = true
	go func() { _, err := conn.Conn.ExecContext(vacCtx, s.command); s.done <- err }()
	return nil
}
func (s *VacuumScenario) Hold(ctx context.Context, rt *Runtime) error {
	deadline := time.NewTimer(rt.Config.Run.Duration)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		default:
			elapsed, err := measureVacuumForeground(ctx, rt.Database, rt.Config.Data.Schema)
			if err != nil {
				return err
			}
			s.foreground = append(s.foreground, elapsed)
		}
	}
}
func (s *VacuumScenario) Verify(context.Context, *Runtime) (Result, error) {
	if len(s.foreground) == 0 {
		return EvaluateVacuumRegression(s.baseline, 0, s.minimum, s.started), nil
	}
	sort.Slice(s.foreground, func(i, j int) bool { return s.foreground[i] < s.foreground[j] })
	return EvaluateVacuumRegression(s.baseline, s.foreground[len(s.foreground)/2], s.minimum, s.started), nil
}
func (s *VacuumScenario) Stop(ctx context.Context, _ *Runtime) error {
	cancelled := false
	if s.cancel != nil {
		s.cancel()
		cancelled = true
	}
	if s.done != nil {
		select {
		case err := <-s.done:
			if err != nil && !cancelled && ctx.Err() == nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}
func (s *VacuumScenario) Restore(ctx context.Context, rt *Runtime) error {
	if rt.Journal == nil {
		return nil
	}
	return rt.Journal.RestoreScenario(ctx, rt.RunID, s.Name())
}
func measureVacuumForeground(ctx context.Context, db *Database, schema string) (time.Duration, error) {
	started := time.Now()
	var value string
	err := db.Scan(ctx, "SELECT sum(length(payload)) FROM "+schema+".vacuum_targets", nil, &value)
	return time.Since(started), err
}
