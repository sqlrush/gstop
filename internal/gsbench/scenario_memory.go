package gsbench

import (
	"context"
	"database/sql"
)

func DynamicMemoryStatements(schema string) []string {
	return []string{
		`SELECT f1.product_id, sum(f1.amount), count(*) FROM ` + schema + `.fact_sales f1 JOIN ` + schema + `.fact_sales f2 ON f1.customer_id=f2.customer_id WHERE mod(f1.id,16)=0 GROUP BY f1.product_id ORDER BY sum(f1.amount) DESC`,
		`SELECT f.store_id, p.category_id, sum(f.amount), avg(f.quantity) FROM ` + schema + `.fact_sales f JOIN ` + schema + `.dim_product p ON p.id=f.product_id GROUP BY f.store_id,p.category_id ORDER BY sum(f.amount) DESC`,
	}
}

type MemoryScenario struct {
	workload *sqlWorkload
	control  ControlResult
	target   float64
	real     bool
}

func NewMemoryScenario() *MemoryScenario { return &MemoryScenario{} }
func (s *MemoryScenario) Name() string   { return "dynamic_memory" }
func (s *MemoryScenario) Prepare(ctx context.Context, rt *Runtime) error {
	s.target = float64(runtimeInt(rt, "scenario.dynamic_memory.target_percent", 90))
	s.real = rt.Capabilities.DynamicMemoryView
	statements := DynamicMemoryStatements(rt.Config.Data.Schema)
	s.workload = newSQLWorkload(ctx, rt, s.Name(), rt.Config.Safety.MaxWorkers, func(ctx context.Context, conn *sql.Conn, workerID int) error {
		if _, err := conn.ExecContext(ctx, "SET work_mem='256MB'"); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, statements[workerID%len(statements)])
		if err != nil {
			return err
		}
		defer rows.Close()
		return consumeRows(rows)
	})
	return nil
}
func (s *MemoryScenario) sample(ctx context.Context, rt *Runtime) Sample {
	if !s.real {
		return Sample{Errors: s.workload.Snapshot().Errors}
	}
	rows, err := rt.Database.Query(ctx, `SELECT memorytype,memorymbytes FROM pg_catalog.pv_total_memory_detail() WHERE memorytype IN ('max_dynamic_memory','dynamic_used_memory')`)
	if err != nil {
		return Sample{Errors: s.workload.Snapshot().Errors}
	}
	defer rows.Close()
	var maximum, used float64
	for rows.Next() {
		var kind string
		var value float64
		if rows.Scan(&kind, &value) != nil {
			continue
		}
		if kind == "max_dynamic_memory" {
			maximum = value
		}
		if kind == "dynamic_used_memory" {
			used = value
		}
	}
	if maximum <= 0 {
		return Sample{Errors: s.workload.Snapshot().Errors}
	}
	return Sample{Available: true, Value: used / maximum * 100, Errors: s.workload.Snapshot().Errors}
}
func (s *MemoryScenario) Ramp(ctx context.Context, rt *Runtime) error {
	c := Controller{Config: ControllerConfig{Target: s.target, Tolerance: 3, MinWorkers: 1, MaxWorkers: rt.Config.Safety.MaxWorkers, Step: 1, RequiredSamples: 3, Interval: rt.Config.Run.RampInterval}, Actuator: s.workload, Sample: func(ctx context.Context) Sample { return s.sample(ctx, rt) }}
	s.control = c.Run(ctx)
	return s.control.Err
}
func (s *MemoryScenario) Hold(ctx context.Context, rt *Runtime) error {
	return waitContext(ctx, rt.Config.Run.Duration)
}
func (s *MemoryScenario) Verify(context.Context, *Runtime) (Result, error) {
	return verifyCapacityResult(s.Name(), s.target, s.control.Actual, s.real, s.workload.Snapshot().Operations), nil
}
func (s *MemoryScenario) Stop(ctx context.Context, _ *Runtime) error {
	if s.workload == nil {
		return nil
	}
	return s.workload.Stop(ctx)
}
func (s *MemoryScenario) Restore(context.Context, *Runtime) error { return nil }
