package gsbench

import (
	"math"
	"strings"
	"testing"
)

func TestConnectionTargetUsesPhysicalCapacity(t *testing.T) {
	if got := connectionTarget(200, 95); got != 190 {
		t.Fatalf("target=%d", got)
	}
	if got := connectionTarget(1000, 95); got != 950 {
		t.Fatalf("physical target=%d", got)
	}
	if got := connectionTarget(101, 95); got != 96 {
		t.Fatalf("fractional target was not rounded up: %d", got)
	}
}

func TestConnectionBudgetIgnoresArtificialSafetyCap(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 20, 90)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsableCapacity != 100 || budget.DesiredTotal != 90 ||
		budget.WorkloadTarget != 70 || budget.ReachableTotal != 90 ||
		budget.Limited {
		t.Fatalf("budget=%+v", budget)
	}
}

func TestConnectionBudgetSubtractsReservedAndExistingSessions(t *testing.T) {
	budget, err := calculateConnectionBudget(101, 3, 7, 95)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsableCapacity != 98 || budget.DesiredTotal != 94 ||
		budget.WorkloadTarget != 87 || budget.ReachableTotal != 94 {
		t.Fatalf("budget=%+v", budget)
	}
	if budget.Limited {
		t.Fatalf("unnecessarily limited budget=%+v", budget)
	}

	physical, err := calculateConnectionBudget(1000, 10, 100, 95)
	if err != nil {
		t.Fatal(err)
	}
	if physical.Limited || physical.WorkloadTarget != 841 || physical.ReachableTotal != 941 {
		t.Fatalf("physical budget=%+v", physical)
	}
	if math.Abs(physical.CeilingPercent-941.0/990.0*100) > 0.001 {
		t.Fatalf("ceiling percent=%f budget=%+v", physical.CeilingPercent, physical)
	}
}

func TestConnectionBudgetInjectsOnlyBaselineDelta(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 80, 90)
	if err != nil {
		t.Fatal(err)
	}
	if budget.UsableCapacity != 100 || budget.DesiredTotal != 90 ||
		budget.WorkloadTarget != 10 || budget.BaselinePercent != 80 {
		t.Fatalf("budget=%+v", budget)
	}
}

func TestConnectionBudgetRejectsTargetAtOrBelowBaseline(t *testing.T) {
	for _, target := range []int{79, 80} {
		if _, err := calculateConnectionBudget(
			103, 3, 80, target,
		); err == nil {
			t.Fatalf("target %d accepted at 80%% baseline", target)
		}
	}
}

func TestConnectionBudgetUsesAllPhysicalHeadroomAtOneHundredPercent(t *testing.T) {
	budget, err := calculateConnectionBudget(103, 3, 20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if budget.WorkloadTarget != 80 || budget.ReachableTotal != 100 {
		t.Fatalf("budget=%+v", budget)
	}
}

func TestConnectionStateCountsPreserveTotal(t *testing.T) {
	idle, idleTxn, active := connectionStateCounts(100, 60, 20)
	if idle != 60 || idleTxn != 20 || active != 20 {
		t.Fatalf("idle=%d idleTxn=%d active=%d", idle, idleTxn, active)
	}
}

func TestThreadStrategyRequiresRealEnabledPoolForSuccess(t *testing.T) {
	if got := selectThreadStrategy(Capabilities{ThreadPoolEnabled: true, ThreadPoolView: true}); got != "real" {
		t.Fatalf("strategy=%q", got)
	}
	if got := selectThreadStrategy(Capabilities{}); got != "active_backend_fallback" {
		t.Fatalf("strategy=%q", got)
	}
}

func TestThreadStrategyAllowsExplicitAdminRestartPath(t *testing.T) {
	cfg := BenchConfig{
		Run:    RunConfig{ScenarioCodes: []ScenarioCode{402}},
		Safety: SafetyConfig{AllowInstanceParameterChange: true, AllowDatabaseRestart: true, RestartCommand: "restart-db"},
	}
	got := selectThreadStrategyForRun(Capabilities{Admin: true}, cfg)
	if got != "enable_with_restart" {
		t.Fatalf("strategy=%q", got)
	}
	cfg.Run.ScenarioCodes = []ScenarioCode{402, 101}
	if got := selectThreadStrategyForRun(Capabilities{Admin: true}, cfg); got != "active_backend_fallback" {
		t.Fatalf("combined strategy=%q", got)
	}
}

func TestParseThreadPoolWorkers(t *testing.T) {
	actual, idle, ok := ParseThreadPoolWorkers([]string{
		"group 0: actual: 8 idle: 2 pending: 4",
		"group 1: actual: 4 idle: 1 pending: 0",
	})
	if !ok || actual != 12 || idle != 3 {
		t.Fatalf("actual=%d idle=%d ok=%v", actual, idle, ok)
	}
}

func TestParseThreadPoolStatusIncludesPendingSessions(t *testing.T) {
	status, ok := ParseThreadPoolStatus([]string{
		"group 0: actual: 8 idle: 2 pending: 4",
		"group 1: actual: 4 idle: 1 pending: 0",
	})
	if !ok || status.Actual != 12 || status.Idle != 3 || status.Pending != 4 {
		t.Fatalf("status=%+v ok=%v", status, ok)
	}
}

func TestThreadCapacityUsesWorkerTopologyAndSessionHeadroom(t *testing.T) {
	if got := threadSessionCapacity(1000, 10, 100, 640, 800); got != 640 {
		t.Fatalf("session capacity=%d", got)
	}
	if got := threadSessionCapacity(500, 10, 480, 640, 800); got != 10 {
		t.Fatalf("headroom-limited session capacity=%d", got)
	}
}

func TestThreadPressurePhysicalHeadroomIgnoresConfiguredCaps(t *testing.T) {
	if got := physicalSessionHeadroom(1000, 10, 100); got != 890 {
		t.Fatalf("physical headroom=%d want=890", got)
	}
	if got := physicalSessionHeadroom(500, 10, 490); got != 0 {
		t.Fatalf("exhausted headroom=%d want=0", got)
	}
}

func TestThreadPressureCapacityLeavesCapAwareCapacityForOtherScenarios(t *testing.T) {
	facts := connectionCapacityFacts{InstanceMax: 1000, Reserved: 10, Existing: 100}
	if got := threadPressureCapacity(facts); got != 890 {
		t.Fatalf("402 capacity=%d want=890", got)
	}
	if got := threadSessionCapacity(1000, 10, 100, 1, 1); got != 1 {
		t.Fatalf("cap-aware capacity=%d want=1", got)
	}
}

func TestThreadPoolPercentAndCeilingIncludeExistingBusyWorkers(t *testing.T) {
	status := ThreadPoolStatus{Actual: 100, Idle: 20}
	if got := threadPoolPercent(status); got != 80 {
		t.Fatalf("baseline=%v", got)
	}
	if got := threadUtilizationCeilingFromBaseline(status, 10); got != 90 {
		t.Fatalf("ceiling=%v", got)
	}
}

func TestThreadTargetMustExceedBaseline(t *testing.T) {
	status := ThreadPoolStatus{Actual: 100, Idle: 20}
	for _, target := range []int{79, 80} {
		if err := validateThreadTarget(status, target, 20); err == nil {
			t.Fatalf("target %d accepted at 80%% baseline", target)
		}
	}
	if err := validateThreadTarget(status, 90, 5); err == nil {
		t.Fatal("unreachable 90% target was accepted")
	}
}

func TestThreadTargetRequiresRealThreadPoolEvidence(t *testing.T) {
	if err := requireRealThreadPoolEvidence(true); err != nil {
		t.Fatal(err)
	}
	if err := requireRealThreadPoolEvidence(false); err == nil {
		t.Fatal("active-backend fallback was accepted for a percentage target")
	}
}

func TestDynamicMemoryStatementsUseHashSortAndAggregate(t *testing.T) {
	joined := strings.ToUpper(strings.Join(DynamicMemoryStatements("gsbench"), "\n"))
	for _, required := range []string{"JOIN", "GROUP BY", "ORDER BY", "SUM("} {
		if !strings.Contains(joined, required) {
			t.Errorf("memory statements missing %q: %s", required, joined)
		}
	}
}

func TestCapacityVerificationDoesNotPromoteFallback(t *testing.T) {
	if got := verifyCapacityResult("thread_pool", 95, 96, true, 10).Outcome; got != OutcomeSuccess {
		t.Fatalf("real outcome=%s", got)
	}
	if got := verifyCapacityResult("thread_pool", 95, 96, false, 10).Outcome; got != OutcomeDegraded {
		t.Fatalf("fallback outcome=%s", got)
	}
	if got := verifyCapacityResult("thread_pool", 95, 40, true, 10).Outcome; got != OutcomeFailed {
		t.Fatalf("missed-target outcome=%s", got)
	}
}
