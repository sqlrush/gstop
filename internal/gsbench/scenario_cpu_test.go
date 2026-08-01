package gsbench

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sequenceCPUSampler struct{ calls atomic.Int64 }

func (s *sequenceCPUSampler) SampleCPU(context.Context) (float64, bool) {
	call := s.calls.Add(1)
	if call <= 3 || call >= 5 {
		return 50, true
	}
	return 10, true
}

func TestTPStatementsUseIndexedReadsWritesAndInserts(t *testing.T) {
	statements := TPStatements("gsbench", 42, 9001, 12.34)
	joined := strings.ToUpper(strings.Join(statements, "\n"))
	for _, required := range []string{
		"SELECT", "UPDATE GSBENCH.ACCOUNTS", "INSERT INTO GSBENCH.ORDERS",
		"WHERE DIST_KEY=43 AND ID=42",
		"ORDERS(DIST_KEY,ID,CUSTOMER_ID,STATUS,AMOUNT,CREATED_AT)",
		"VALUES(43,9001,42,0,12.34",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("TP statements missing %q: %s", required, joined)
		}
	}
	for _, marker := range []string{"$1", "$2", "$3", "?"} {
		if strings.Contains(joined, marker) {
			t.Fatalf("TP statements contain bind marker %q: %s", marker, joined)
		}
	}
}

func TestAPStatementsContainScanJoinAggregateAndSort(t *testing.T) {
	statements, err := APStatements("gsbench", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.ToUpper(strings.Join(statements, "\n"))
	for _, required := range []string{"FACT_SALES", " JOIN ", "GROUP BY", "ORDER BY", "SUM("} {
		if !strings.Contains(joined, required) {
			t.Errorf("AP statements missing %q: %s", required, joined)
		}
	}
}

func TestAPStatementsBoundFactInput(t *testing.T) {
	statements, err := APStatements("gsbench", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if !strings.Contains(statement, "FROM gsbench.fact_sales LIMIT 1000000") {
			t.Fatalf("unbounded AP SQL: %s", statement)
		}
	}
}

func TestAPStatementsRejectUnsafeInput(t *testing.T) {
	if _, err := APStatements("bad-name", 1_000_000); err == nil {
		t.Fatal("expected unsafe schema error")
	}
	if _, err := APStatements("gsbench", 0); err == nil {
		t.Fatal("expected scan row error")
	}
}

func TestAPWorkloadDisablesOperationTimeoutOnly(t *testing.T) {
	normal := newSQLWorkload(context.Background(), nil, "normal", 1, nil)
	if normal.disableOperationTimeout {
		t.Fatal("ordinary workload unexpectedly disables query timeout")
	}
	ap := newSQLWorkloadWithoutOperationTimeout(context.Background(), nil, "ap", 1, nil)
	if !ap.disableOperationTimeout {
		t.Fatal("AP workload unexpectedly uses query timeout")
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

func TestMixedWorkerTargetsCapAPAndShiftRemainderToTP(t *testing.T) {
	tp, ap := MixedWorkerTargetsCapped(20, 50, 4)
	if tp != 16 || ap != 4 {
		t.Fatalf("tp=%d ap=%d", tp, ap)
	}
	tp, ap = MixedWorkerTargetsCapped(40, 80, 4)
	if tp != 36 || ap != 4 {
		t.Fatalf("tp=%d ap=%d", tp, ap)
	}
}

func TestDefaultAPSafetyCaps(t *testing.T) {
	if defaultAPSafety.MaxWorkers != 8 ||
		defaultAPSafety.CPUTargetPercent != 70 ||
		defaultAPSafety.ScanRows != 1_000_000 {
		t.Fatalf("AP defaults=%+v", defaultAPSafety)
	}
	if defaultMixedMaximum != 20 || defaultMixedAPMaximum != 4 {
		t.Fatalf("mixed defaults total=%d ap=%d", defaultMixedMaximum, defaultMixedAPMaximum)
	}
}

func TestCPUControllerCapsEachWorkerRampAdjustment(t *testing.T) {
	config := cpuControllerConfig(95, 640, 2*time.Second)
	if config.Step != maximumCPUWorkersPerAdjustment {
		t.Fatalf("cpu controller step=%d, want cap=%d", config.Step, maximumCPUWorkersPerAdjustment)
	}
	if config.MaxWorkers != 640 || config.Target != 95 {
		t.Fatalf("cpu controller config=%+v", config)
	}
}

func TestCPUVerificationRequiresMeasuredTargetForSuccess(t *testing.T) {
	result := verifyCPUResult("tp_cpu", 95, true, ControlResult{Reached: true, Measured: true, Actual: 95, Workers: 8}, WorkerSnapshot{Operations: 100})
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	result = verifyCPUResult("tp_cpu", 95, false, ControlResult{Ceiling: true, Workers: 8}, WorkerSnapshot{Operations: 100})
	if result.Outcome != OutcomeDegraded {
		t.Fatalf("fallback result=%+v", result)
	}
	result = verifyCPUResult("tp_cpu", 95, true, ControlResult{Ceiling: true, Measured: true, Actual: 50, ReachableMax: 60, Workers: 8}, WorkerSnapshot{})
	if result.Outcome != OutcomeFailed || !strings.Contains(result.Message, "ceiling 60.0%") {
		t.Fatalf("failure result=%+v", result)
	}
}

func TestCPURuntimeEvidenceRequiresSuccessfulSample(t *testing.T) {
	evidence := cpuRuntimeEvidence(95, true, ControlResult{})
	if len(evidence) == 0 || evidence[0].Available {
		t.Fatalf("unsampled CPU evidence reported available: %+v", evidence)
	}
	evidence = cpuRuntimeEvidence(95, true, ControlResult{
		Measured: true,
		Actual:   94,
	})
	if !evidence[0].Available || evidence[0].Actual != 94 {
		t.Fatalf("measured CPU evidence unavailable: %+v", evidence)
	}
}

func TestCPUWorkloadScenarioContinuesRegulatingThroughHold(t *testing.T) {
	workload := &sqlWorkload{group: NewWorkerGroup(context.Background(), 8, func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	})}
	scenario := &cpuWorkloadScenario{
		code: 101, name: "tp_cpu", workload: workload, available: true,
	}
	runtime := &Runtime{
		Config: BenchConfig{
			Run:    RunConfig{Duration: 5 * time.Millisecond, RampInterval: time.Nanosecond},
			Safety: SafetyConfig{CPUTargetPercent: 50, MaxWorkers: 8},
		},
		CPU: &sequenceCPUSampler{},
	}
	if err := scenario.Ramp(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if err := scenario.Hold(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if scenario.workload.Target() <= 1 {
		t.Fatalf("worker target froze after first entering the band: control=%+v target=%d", scenario.control, scenario.workload.Target())
	}
	if err := scenario.Stop(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
}
