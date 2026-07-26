package gsbench

import "testing"

func TestNewLockScenarioUsesDefinitionIdentity(t *testing.T) {
	scenario := NewLockScenario(LockDefinition{Code: 502, Name: "lock_table_exclusive"})
	if scenario.Code() != 502 || scenario.Name() != "lock_table_exclusive" {
		t.Fatalf("scenario=%d/%s", scenario.Code(), scenario.Name())
	}
}
