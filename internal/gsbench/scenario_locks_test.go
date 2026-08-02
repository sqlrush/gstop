package gsbench

import "testing"

func TestNewLockScenarioUsesDefinitionIdentity(t *testing.T) {
	scenario := NewLockScenario(LockDefinition{Code: 502, Name: "lock_table_exclusive"})
	if scenario.Code() != 502 || scenario.Name() != "lock_table_exclusive" {
		t.Fatalf("scenario=%d/%s", scenario.Code(), scenario.Name())
	}
}

func TestLockScenarioCompilesRuntimeSessionTopology(t *testing.T) {
	scenario := &LockScenario{
		definition: LockDefinition{Code: 501, Name: "lock_row_chain"},
	}
	runtime := &Runtime{
		RunID: "run-1",
		Config: BenchConfig{
			Data: DataConfig{Schema: "gsbench"},
			LockWorkloads: LockWorkloadConfig{
				RowChainSessions: 4,
				RowChainDepth:    2,
			},
		},
	}
	if err := scenario.configureDefinition(runtime); err != nil {
		t.Fatal(err)
	}
	if scenario.definition.RequestedSessions != 4 ||
		scenario.definition.RequestedChainDepth != 2 ||
		len(scenario.definition.Waiters) != 3 {
		t.Fatalf("definition=%+v", scenario.definition)
	}
}

func TestLockScenarioReportsRuntimeEvidenceWhenValidationIsDisabled(t *testing.T) {
	definition, err := configureLockDefinition(
		businessLockDefinitionForTest(t, 502),
		LockWorkloadConfig{TableExclusiveSessions: 3},
		"gsbench",
		"run-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewLockEngine(definition)
	engine.evidence = []LockEvidence{
		{
			Granted: false, LockType: "relation",
			Object:     "lock_table_targets",
			HolderMode: "AccessExclusive", WaiterMode: "AccessShare",
			BlockerTag: "gsbench/run-1/lock_table_exclusive/blocker",
			WaiterTag:  "gsbench/run-1/lock_table_exclusive/waiter-1",
		},
		{
			Granted: false, LockType: "relation",
			Object:     "lock_table_targets",
			HolderMode: "AccessExclusive", WaiterMode: "AccessShare",
			BlockerTag: "gsbench/run-1/lock_table_exclusive/blocker",
			WaiterTag:  "gsbench/run-1/lock_table_exclusive/waiter-2",
		},
	}
	scenario := &LockScenario{definition: definition, engine: engine}
	metrics := map[string]Evidence{}
	for _, item := range scenario.RuntimeEvidence() {
		metrics[item.Metric] = item
	}
	if metrics["lock_sessions"].Target != 3 ||
		metrics["lock_sessions"].Actual != 3 ||
		metrics["lock_waiters"].Target != 2 ||
		metrics["lock_waiters"].Actual != 2 {
		t.Fatalf("metrics=%+v", metrics)
	}
}
