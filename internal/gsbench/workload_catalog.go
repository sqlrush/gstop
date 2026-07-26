package gsbench

import "fmt"

// ScenarioWorkloadStatements returns one complete literal SQL statement for
// every plannable workload shape a scenario can execute.
func ScenarioWorkloadStatements(runtime *Runtime, scenario string) ([]string, error) {
	if runtime == nil {
		return nil, fmt.Errorf("runtime is unavailable")
	}
	schema := runtime.Config.Data.Schema
	switch scenario {
	case "tp_cpu":
		return TPStatements(schema, 42, 9001, 1000), nil
	case "ap_cpu":
		scanRows := runtimeInt(runtime, "scenario.ap_cpu.scan_rows", defaultAPSafety.ScanRows)
		return APStatements(schema, scanRows)
	case "mixed_cpu":
		scanRows := runtimeInt(runtime, "scenario.mixed_cpu.scan_rows", 1_000_000)
		ap, err := APStatements(schema, scanRows)
		if err != nil {
			return nil, err
		}
		return append(TPStatements(schema, 42, 9001, 1000), ap...), nil
	case "connection_pool":
		return []string{"SELECT 1", "SELECT pg_sleep(1)"}, nil
	case "thread_pool":
		return []string{"SELECT pg_sleep(1)"}, nil
	case "vacuum_pressure":
		return []string{
			vacuumForegroundSQL(schema),
			"UPDATE " + schema + ".vacuum_targets SET version=version+1,payload=payload||'x',updated_at=current_timestamp",
		}, nil
	}
	return nil, fmt.Errorf("unknown scenario %q", scenario)
}
