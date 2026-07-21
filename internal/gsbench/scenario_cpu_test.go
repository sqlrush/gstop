package gsbench

import (
	"strings"
	"testing"
)

func TestTPStatementsUseIndexedReadsWritesAndInserts(t *testing.T) {
	statements := TPStatements("gsbench")
	joined := strings.ToUpper(strings.Join(statements, "\n"))
	for _, required := range []string{"SELECT", "UPDATE GSBENCH.ACCOUNTS", "INSERT INTO GSBENCH.ORDERS", "WHERE ID=$1"} {
		if !strings.Contains(joined, required) {
			t.Errorf("TP statements missing %q: %s", required, joined)
		}
	}
}

func TestAPStatementsContainScanJoinAggregateAndSort(t *testing.T) {
	joined := strings.ToUpper(strings.Join(APStatements("gsbench"), "\n"))
	for _, required := range []string{"FACT_SALES", " JOIN ", "GROUP BY", "ORDER BY", "SUM("} {
		if !strings.Contains(joined, required) {
			t.Errorf("AP statements missing %q: %s", required, joined)
		}
	}
}

func TestMixedWorkerTargetsPreserveRatio(t *testing.T) {
	tp, ap := MixedWorkerTargets(10, 80)
	if tp != 8 || ap != 2 {
		t.Fatalf("tp=%d ap=%d", tp, ap)
	}
	tp, ap = MixedWorkerTargets(1, 80)
	if tp+ap != 1 {
		t.Fatalf("single worker split tp=%d ap=%d", tp, ap)
	}
}

func TestCPUVerificationRequiresMeasuredTargetForSuccess(t *testing.T) {
	result := verifyCPUResult("tp_cpu", 95, true, ControlResult{Reached: true, Actual: 95, Workers: 8}, WorkerSnapshot{Operations: 100})
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	result = verifyCPUResult("tp_cpu", 95, false, ControlResult{Ceiling: true, Workers: 8}, WorkerSnapshot{Operations: 100})
	if result.Outcome != OutcomeDegraded {
		t.Fatalf("fallback result=%+v", result)
	}
	result = verifyCPUResult("tp_cpu", 95, true, ControlResult{Ceiling: true, Actual: 50, Workers: 8}, WorkerSnapshot{})
	if result.Outcome != OutcomeFailed {
		t.Fatalf("failure result=%+v", result)
	}
}
