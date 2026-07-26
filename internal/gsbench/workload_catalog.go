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
	if code, ok := planChangeCodeForName(scenario); ok {
		definitions, err := PlanScenarioDefinitions(schema)
		if err != nil {
			return nil, err
		}
		for _, definition := range definitions {
			if definition.Code == code {
				return append([]string(nil), definition.Candidates...), nil
			}
		}
	}
	if definition, err := DefaultScenarioCatalog().Resolve(scenario); err == nil && definition.Code >= 621 && definition.Code <= 626 {
		statement, statementErr := hardParseLiteralSQL(definition.Code, schema, 42)
		if statementErr != nil {
			return nil, statementErr
		}
		return []string{statement}, nil
	}
	if definition, err := DefaultScenarioCatalog().Resolve(scenario); err == nil {
		if lifecycle, lifecycleErr := memoryLifecycleFor(definition.Code); lifecycleErr == nil {
			statements := make([]string, 0, len(lifecycle.AllocationCodes))
			for _, code := range lifecycle.AllocationCodes {
				workload, workloadErr := ResourceWorkloadFor(code, schema, runtime.Environment)
				if workloadErr != nil {
					return nil, workloadErr
				}
				statements = append(statements, workload.Statement)
			}
			return statements, nil
		}
		if workload, workloadErr := ResourceWorkloadFor(definition.Code, schema, runtime.Environment); workloadErr == nil {
			return []string{workload.Statement}, nil
		}
	}
	return nil, fmt.Errorf("unknown scenario %q", scenario)
}
