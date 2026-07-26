package gsbench

import (
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
		Run:    RunConfig{Scenarios: []string{"thread_pool"}},
		Safety: SafetyConfig{AllowInstanceParameterChange: true, AllowDatabaseRestart: true, RestartCommand: "restart-db"},
	}
	got := selectThreadStrategyForRun(Capabilities{Admin: true}, cfg)
	if got != "enable_with_restart" {
		t.Fatalf("strategy=%q", got)
	}
	cfg.Run.Scenarios = []string{"thread_pool", "tp_cpu"}
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
