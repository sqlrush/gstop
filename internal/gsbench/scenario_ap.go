package gsbench

import (
	"context"
	"database/sql"
)

func APStatements(schema string) []string {
	return []string{
		`SELECT p.category_id, count(*), sum(f.amount), avg(f.quantity) FROM ` + schema + `.fact_sales f JOIN ` + schema + `.dim_product p ON p.id=f.product_id GROUP BY p.category_id ORDER BY sum(f.amount) DESC LIMIT 100`,
		`SELECT c.region_id, f.store_id, sum(f.amount), count(*) FROM ` + schema + `.fact_sales f JOIN ` + schema + `.customers c ON c.id=f.customer_id GROUP BY c.region_id,f.store_id ORDER BY count(*) DESC LIMIT 200`,
		`SELECT f.sale_date, sum(f.amount), avg(f.amount), max(f.amount) FROM ` + schema + `.fact_sales f GROUP BY f.sale_date ORDER BY sum(f.amount) DESC`,
	}
}

type APScenario struct{ *cpuWorkloadScenario }

func NewAPScenario() *APScenario {
	return &APScenario{cpuWorkloadScenario: &cpuWorkloadScenario{name: "ap_cpu", build: buildAPWorkload}}
}

func buildAPWorkload(ctx context.Context, rt *Runtime, name string) *sqlWorkload {
	statements := APStatements(rt.Config.Data.Schema)
	return newSQLWorkload(ctx, rt, name, rt.Config.Safety.MaxWorkers, func(ctx context.Context, conn *sql.Conn, workerID int) error {
		rows, err := conn.QueryContext(ctx, statements[workerID%len(statements)])
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	})
}

type cpuWorkloadScenario struct {
	name      string
	build     func(context.Context, *Runtime, string) *sqlWorkload
	workload  *sqlWorkload
	control   ControlResult
	available bool
	target    float64
}

func (s *cpuWorkloadScenario) Name() string { return s.name }
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
		Config:   ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: rt.Config.Safety.MaxWorkers, Step: 1, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval},
		Actuator: s.workload,
		Sample: func(ctx context.Context) Sample {
			if s.available {
				if value, ok := rt.CPU.SampleCPU(ctx); ok {
					return Sample{Value: value, Available: true, Errors: s.workload.Snapshot().Errors}
				}
			}
			return Sample{Available: false, Errors: s.workload.Snapshot().Errors}
		},
	}
	s.control = controller.Run(ctx)
	return s.control.Err
}
func (s *cpuWorkloadScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *cpuWorkloadScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCPUResult(s.name, s.target, s.available, s.control, s.workload.Snapshot()), nil
}
func (s *cpuWorkloadScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}
func (s *cpuWorkloadScenario) Restore(context.Context, *Runtime) error { return nil }
