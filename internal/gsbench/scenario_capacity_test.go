package gsbench

import (
	"math"
	"strings"
	"testing"
)

func TestConnectionTargetHonorsInstanceAndSafetyCeilings(t *testing.T) {
	if got := connectionTarget(200, 95, 500); got != 190 {
		t.Fatalf("target=%d", got)
	}
	if got := connectionTarget(1000, 95, 500); got != 500 {
		t.Fatalf("safety-capped target=%d", got)
	}
	if got := connectionTarget(101, 95, 500); got != 96 {
		t.Fatalf("fractional target was not rounded up: %d", got)
	}
}

func TestConnectionBudgetSubtractsReservedAndExistingSessions(t *testing.T) {
	budget, err := calculateConnectionBudget(101, 3, 7, 95, 100)
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

	limited, err := calculateConnectionBudget(1000, 10, 100, 95, 500)
	if err != nil {
		t.Fatal(err)
	}
	if !limited.Limited || limited.WorkloadTarget != 500 || limited.ReachableTotal != 600 {
		t.Fatalf("limited budget=%+v", limited)
	}
	if math.Abs(limited.CeilingPercent-600.0/990.0*100) > 0.001 {
		t.Fatalf("ceiling percent=%f budget=%+v", limited.CeilingPercent, limited)
	}
}

func TestConnectionTopUpUsesObservedLiveSessions(t *testing.T) {
	if got := connectionTopUp(94, 91, 84, 87); got != 3 {
		t.Fatalf("top-up=%d", got)
	}
	if got := connectionTopUp(94, 91, 87, 87); got != 0 {
		t.Fatalf("top-up exceeded workload safety limit: %d", got)
	}
	if got := connectionTopUp(98, 94, 86, 87); got != 1 {
		t.Fatalf("closed tagged session was not replenished: %d", got)
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
	if got := threadUtilizationCeiling(1000, 640); math.Abs(got-64) > 0.001 {
		t.Fatalf("thread utilization ceiling=%f", got)
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
