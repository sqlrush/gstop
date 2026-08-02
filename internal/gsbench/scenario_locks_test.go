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
