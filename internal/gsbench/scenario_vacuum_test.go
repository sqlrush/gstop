package gsbench

import (
	"testing"
	"time"
)

func TestVacuumCommandModesAndFullOptIn(t *testing.T) {
	if got, err := VacuumCommand("gsbench", "vacuum", false); err != nil || got != "VACUUM gsbench.vacuum_targets" {
		t.Fatalf("command=%q err=%v", got, err)
	}
	if got, err := VacuumCommand("gsbench", "analyze", false); err != nil || got != "VACUUM ANALYZE gsbench.vacuum_targets" {
		t.Fatalf("command=%q err=%v", got, err)
	}
	if _, err := VacuumCommand("gsbench", "full", false); err == nil {
		t.Fatal("VACUUM FULL should require opt-in")
	}
	if got, err := VacuumCommand("gsbench", "full", true); err != nil || got != "VACUUM FULL gsbench.vacuum_targets" {
		t.Fatalf("command=%q err=%v", got, err)
	}
}

func TestVacuumRegressionUsesForegroundMedian(t *testing.T) {
	result := EvaluateVacuumRegression(10*time.Millisecond, 16*time.Millisecond, 1.5, true)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	result = EvaluateVacuumRegression(10*time.Millisecond, 11*time.Millisecond, 1.5, true)
	if result.Outcome != OutcomeFailed {
		t.Fatalf("small regression result=%+v", result)
	}
}
