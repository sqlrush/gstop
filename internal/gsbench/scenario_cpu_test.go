package gsbench

import (
	"context"
	"strings"
	"testing"
	"time"
)

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

func TestAPStatementsContainBoundSingleThreadedAnalytics(t *testing.T) {
	statements, err := APStatements("gsbench", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	joinedAll := strings.ToUpper(strings.Join(statements, "\n"))
	if !strings.Contains(joinedAll, " JOIN ") {
		t.Fatalf("AP workload has no join: %s", joinedAll)
	}
	for _, statement := range statements {
		joined := strings.ToUpper(statement)
		for _, required := range []string{
			"FACT_SALES", "GROUP BY", "ORDER BY", "SUM(",
			"FROM GSBENCH.FACT_SALES LIMIT 1000000",
			"/*+ SET(QUERY_DOP 1) */",
		} {
			if !strings.Contains(joined, required) {
				t.Errorf("AP statement missing %q: %s", required, statement)
			}
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

func TestFixedWorkerVerificationUsesExactWorkersWithoutCPUFeedback(t *testing.T) {
	run := &fixedWorkerRun{
		duration: 30 * time.Second,
		lanes:    []fixedWorkerLane{{Name: "tp", Workers: 3}},
		final: map[string]WorkerSnapshot{
			"tp": {
				Target: 3, Started: 3, PeakActive: 3,
				Operations: 120, TotalLatency: 6 * time.Second,
			},
		},
		startedAt: time.Unix(100, 0),
		endedAt:   time.Unix(130, 0),
	}
	result := verifyFixedWorkerResult(
		"tp_cpu",
		run,
		map[string]string{"tp": "workers"},
	)
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("result=%+v", result)
	}
	metrics := map[string]Evidence{}
	for _, evidence := range result.Evidence {
		metrics[evidence.Metric] = evidence
		if strings.Contains(evidence.Metric, "cpu") || evidence.Metric == "effective_workers" {
			t.Fatalf("fixed worker result retained feedback evidence: %+v", evidence)
		}
	}
	if workers := metrics["workers"]; workers.Target != 3 || workers.Actual != 3 {
		t.Fatalf("workers evidence=%+v", workers)
	}
	if duration := metrics["duration_seconds"]; duration.Target != 30 || duration.Actual != 30 {
		t.Fatalf("duration evidence=%+v", duration)
	}
}

func TestFixedWorkerVerificationRejectsMissingOrFailedWorker(t *testing.T) {
	for _, snapshot := range []WorkerSnapshot{
		{Target: 2, Started: 1, PeakActive: 1, Operations: 10},
		{Target: 2, Started: 2, PeakActive: 2, Operations: 10, Errors: 1, FirstError: "query failed"},
	} {
		run := &fixedWorkerRun{
			duration:  time.Second,
			lanes:     []fixedWorkerLane{{Name: "ap", Workers: 2}},
			final:     map[string]WorkerSnapshot{"ap": snapshot},
			startedAt: time.Unix(100, 0), endedAt: time.Unix(101, 0),
		}
		result := verifyFixedWorkerResult(
			"ap_cpu",
			run,
			map[string]string{"ap": "workers"},
		)
		if result.Outcome != OutcomeFailed {
			t.Fatalf("snapshot=%+v result=%+v", snapshot, result)
		}
	}
}

func TestCPUScenariosDeclareFixedWorkerStrategiesAndOwnDuration(t *testing.T) {
	checks := []struct {
		scenario Scenario
		strategy string
	}{
		{scenario: NewTPScenario(), strategy: "tp_sql_fixed_workers"},
		{scenario: NewAPScenario(), strategy: "ap_sql_fixed_workers"},
		{scenario: NewMixedScenario(), strategy: "mixed_tp_ap_fixed_workers"},
	}
	for _, check := range checks {
		strategic, ok := check.scenario.(ScenarioStrategy)
		if !ok || strategic.Strategy() != check.strategy {
			t.Fatalf("scenario %s strategy=%q, want %q", check.scenario.Name(), strategic.Strategy(), check.strategy)
		}
		owner, ok := check.scenario.(workloadDurationOwner)
		if !ok || !owner.OwnsWorkloadDuration() {
			t.Fatalf("scenario %s does not own its fixed pressure duration", check.scenario.Name())
		}
	}
}
