package gsbench

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type APSafety struct {
	CPUTargetPercent int
	MaxWorkers       int
	ScanRows         int
}

var defaultAPSafety = APSafety{CPUTargetPercent: 70, MaxWorkers: 8, ScanRows: 1_000_000}

const maximumCPUWorkersPerAdjustment = 8

func cpuControllerConfig(
	target float64,
	maxWorkers int,
	interval time.Duration,
) ControllerConfig {
	step := max(1, (maxWorkers-1+9)/10)
	return ControllerConfig{
		Target: target, Tolerance: 3, MinWorkers: 1,
		MaxWorkers:      maxWorkers,
		Step:            min(maximumCPUWorkersPerAdjustment, step),
		RequiredSamples: 3,
		Interval:        interval,
	}
}

func LoadAPSafety(rt *Runtime, prefix string, defaults APSafety) (APSafety, error) {
	policy := APSafety{
		CPUTargetPercent: runtimeInt(rt, prefix+".cpu_target_percent", defaults.CPUTargetPercent),
		MaxWorkers:       runtimeInt(rt, prefix+".max_workers", defaults.MaxWorkers),
		ScanRows:         runtimeInt(rt, prefix+".scan_rows", defaults.ScanRows),
	}
	if policy.CPUTargetPercent < 1 || policy.CPUTargetPercent > 100 {
		return APSafety{}, fmt.Errorf("%s.cpu_target_percent must be between 1 and 100", prefix)
	}
	if policy.MaxWorkers <= 0 || policy.ScanRows <= 0 {
		return APSafety{}, fmt.Errorf("%s worker and scan row limits must be positive", prefix)
	}
	if rt != nil && rt.Config.Safety.MaxWorkers > 0 {
		policy.MaxWorkers = min(policy.MaxWorkers, rt.Config.Safety.MaxWorkers)
	}
	return policy, nil
}

func APStatements(schema string, scanRows int) ([]string, error) {
	if !identifierRE.MatchString(schema) {
		return nil, fmt.Errorf("unsafe schema %q", schema)
	}
	if scanRows <= 0 {
		return nil, fmt.Errorf("scan_rows must be positive")
	}
	fact := fmt.Sprintf(
		"(SELECT id,sale_date,customer_id,product_id,store_id,amount,quantity FROM %s.fact_sales LIMIT %d)",
		schema, scanRows,
	)
	return []string{
		`SELECT p.category_id, count(*), sum(f.amount), avg(f.quantity) FROM ` + fact + ` f JOIN ` + schema + `.dim_product p ON p.id=f.product_id GROUP BY p.category_id ORDER BY sum(f.amount) DESC LIMIT 100`,
		`SELECT c.region_id, f.store_id, sum(f.amount), count(*) FROM ` + fact + ` f JOIN ` + schema + `.customers c ON c.id=f.customer_id GROUP BY c.region_id,f.store_id ORDER BY count(*) DESC LIMIT 200`,
		`SELECT f.sale_date, sum(f.amount), avg(f.amount), max(f.amount) FROM ` + fact + ` f GROUP BY f.sale_date ORDER BY sum(f.amount) DESC`,
	}, nil
}

type APScenario struct {
	workload  *sqlWorkload
	control   ControlResult
	loop      continuousControl
	available bool
	policy    APSafety
}

func NewAPScenario() *APScenario { return &APScenario{} }

func buildAPWorkload(ctx context.Context, rt *Runtime, name string, maximum, scanRows int) (*sqlWorkload, error) {
	statements, err := APStatements(rt.Config.Data.Schema, scanRows)
	if err != nil {
		return nil, err
	}
	return newSQLWorkloadWithoutOperationTimeout(ctx, rt, name, maximum, func(ctx context.Context, conn *sql.Conn, workerID int) error {
		rows, err := conn.QueryContext(ctx, statements[workerID%len(statements)])
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	}), nil
}

func (s *APScenario) Code() ScenarioCode { return 102 }
func (s *APScenario) Name() string       { return "ap_cpu" }
func (s *APScenario) Strategy() string   { return "ap_sql_feedback" }

func (s *APScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt.Database == nil {
		return sql.ErrConnDone
	}
	policy, err := LoadAPSafety(rt, "scenario.ap_cpu", defaultAPSafety)
	if err != nil {
		return err
	}
	workload, err := buildAPWorkload(ctx, rt, s.Name(), policy.MaxWorkers, policy.ScanRows)
	if err != nil {
		return err
	}
	s.policy = policy
	s.workload = workload
	s.available = rt.CPU != nil && rt.Capabilities.DatabaseCPU
	if rt.Log != nil {
		rt.Log.Info("scenario=%s cpu_target=%d max_workers=%d scan_rows=%d query_timeout=disabled",
			s.Name(), policy.CPUTargetPercent, policy.MaxWorkers, policy.ScanRows)
	}
	return nil
}

func (s *APScenario) Ramp(ctx context.Context, rt *Runtime) error {
	controller := Controller{
		Config: cpuControllerConfig(
			float64(s.policy.CPUTargetPercent),
			s.policy.MaxWorkers,
			rt.Config.Run.RampInterval,
		),
		Actuator: s.workload,
		Sample: func(ctx context.Context) Sample {
			snapshot := s.workload.Snapshot()
			if !s.available {
				return sampleCPU(ctx, nil, snapshot)
			}
			return sampleCPU(ctx, rt.CPU, snapshot)
		},
	}
	s.loop.Start(ctx, controller)
	return nil
}

func (s *APScenario) Hold(ctx context.Context, rt *Runtime) error {
	var err error
	s.control, err = s.loop.Wait(ctx, rt.Config.Run.Duration)
	return err
}

func (s *APScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCPUResult(s.Name(), float64(s.policy.CPUTargetPercent), s.available, s.control, s.workload.Snapshot()), nil
}
func (s *APScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.workload == nil {
		return WorkerSnapshot{}
	}
	return s.workload.Snapshot()
}
func (s *APScenario) RuntimeEvidence() []Evidence {
	return cpuRuntimeEvidence(
		float64(s.policy.CPUTargetPercent),
		s.available,
		s.control,
	)
}

func (s *APScenario) Stop(ctx context.Context, _ *Runtime) error {
	s.control = s.loop.Stop()
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}

func (s *APScenario) Restore(context.Context, *Runtime) error { return nil }

type cpuWorkloadScenario struct {
	code      ScenarioCode
	name      string
	build     func(context.Context, *Runtime, string) *sqlWorkload
	workload  *sqlWorkload
	control   ControlResult
	loop      continuousControl
	available bool
	target    float64
}

func (s *cpuWorkloadScenario) Code() ScenarioCode { return s.code }
func (s *cpuWorkloadScenario) Name() string       { return s.name }
func (s *cpuWorkloadScenario) Strategy() string   { return "tp_sql_feedback" }
func (s *cpuWorkloadScenario) Prepare(ctx context.Context, rt *Runtime) error {
	if rt.Database == nil {
		return sql.ErrConnDone
	}
	s.workload = s.build(ctx, rt, s.name)
	s.available = rt.CPU != nil && rt.Capabilities.DatabaseCPU
	return nil
}
func (s *cpuWorkloadScenario) Ramp(ctx context.Context, rt *Runtime) error {
	s.target = float64(rt.Config.Safety.CPUTargetPercent)
	controller := Controller{
		Config:   cpuControllerConfig(s.target, rt.Config.Safety.MaxWorkers, rt.Config.Run.RampInterval),
		Actuator: s.workload,
		Sample: func(ctx context.Context) Sample {
			snapshot := s.workload.Snapshot()
			if !s.available {
				return sampleCPU(ctx, nil, snapshot)
			}
			return sampleCPU(ctx, rt.CPU, snapshot)
		},
	}
	s.loop.Start(ctx, controller)
	return nil
}
func (s *cpuWorkloadScenario) Hold(ctx context.Context, rt *Runtime) error {
	var err error
	s.control, err = s.loop.Wait(ctx, rt.Config.Run.Duration)
	return err
}
func (s *cpuWorkloadScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCPUResult(s.name, s.target, s.available, s.control, s.workload.Snapshot()), nil
}
func (s *cpuWorkloadScenario) ExecutionSnapshot() WorkerSnapshot {
	if s.workload == nil {
		return WorkerSnapshot{}
	}
	return s.workload.Snapshot()
}
func (s *cpuWorkloadScenario) RuntimeEvidence() []Evidence {
	return cpuRuntimeEvidence(s.target, s.available, s.control)
}
func (s *cpuWorkloadScenario) Stop(ctx context.Context, _ *Runtime) error {
	s.control = s.loop.Stop()
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}
func (s *cpuWorkloadScenario) Restore(context.Context, *Runtime) error { return nil }
