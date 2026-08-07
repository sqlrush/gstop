package gsbench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeChurnConnection struct {
	executed bool
	closed   bool
}

func (c *fakeChurnConnection) ExecContext(_ context.Context, statement string, _ ...any) error {
	if statement != "SELECT 1" {
		return errUnexpectedChurnStatement{statement: statement}
	}
	c.executed = true
	return nil
}

func (c *fakeChurnConnection) Close() error {
	c.closed = true
	return nil
}

type errUnexpectedChurnStatement struct{ statement string }

func (e errUnexpectedChurnStatement) Error() string { return e.statement }

func TestConnectionChurnOpensUsesAndClosesEveryOperation(t *testing.T) {
	var connections []*fakeChurnConnection
	op := newConnectionChurnOperation("SELECT 1", func(context.Context, int64) (churnConnection, error) {
		connection := &fakeChurnConnection{}
		connections = append(connections, connection)
		return connection, nil
	})
	for range 3 {
		if err := op.Run(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	metrics := op.Metrics()
	if metrics.Created != 3 || metrics.Closed != 3 || metrics.Operations != 3 {
		t.Fatalf("metrics=%+v", metrics)
	}
	for index, connection := range connections {
		if !connection.executed || !connection.closed {
			t.Fatalf("connection %d executed=%v closed=%v", index, connection.executed, connection.closed)
		}
	}
}

type failingCloseChurnConnection struct{ fakeChurnConnection }

func (c *failingCloseChurnConnection) Close() error { return errors.New("close failed") }

func TestConnectionChurnReportsCloseFailure(t *testing.T) {
	op := newConnectionChurnOperation("SELECT 1", func(context.Context, int64) (churnConnection, error) {
		return &failingCloseChurnConnection{}, nil
	})
	if err := op.Run(context.Background(), 0); err == nil {
		t.Fatal("close failure was not returned")
	}
	if op.Metrics().Failures != 1 {
		t.Fatalf("metrics=%+v", op.Metrics())
	}
}

func TestPressureTargetsIgnoreLegacySafetyCap(t *testing.T) {
	target, err := resourcePressureTarget(404, 4, 8)
	if err != nil || target != 5 {
		t.Fatalf("thread target=%d err=%v", target, err)
	}
	if target, err := resourcePressureTarget(404, 4, 4); err != nil || target != 5 {
		t.Fatalf("thread queue target=%d error=%v", target, err)
	}
	if _, err := resourcePressureTarget(405, 2, 6); err == nil {
		t.Fatal("deferred pooler scenario still has a reachable pressure target")
	}
}

func TestThreadQueueRequiresObservedPendingWorkForSuccess(t *testing.T) {
	group := &WorkerGroup{target: 5}
	scenario := &resourcePressureScenario{
		resourceScenario: &resourceScenario{code: 404, name: "threadpool_queue", workers: &sqlWorkload{group: group}},
		target:           5,
		status:           ThreadPoolStatus{Actual: 4, Idle: 0},
		sessionCeiling:   5,
		established:      5,
		peakPending:      1,
	}
	result, err := scenario.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeSuccess {
		t.Fatalf("pending queue result=%+v", result)
	}
	if !evidenceMetricAvailable(result.Evidence, "thread_pool_actual_workers") || !evidenceMetricAvailable(result.Evidence, "thread_pool_pending_sessions") || !evidenceMetricAvailable(result.Evidence, "thread_queue_session_ceiling") {
		t.Fatalf("missing real queue evidence: %+v", result.Evidence)
	}

	scenario.peakPending = 0
	result, err = scenario.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeFailed {
		t.Fatalf("no-pending queue result=%+v", result)
	}
}

func TestThreadQueueHoldReturnsWorkerErrorsImmediately(t *testing.T) {
	group := &WorkerGroup{}
	group.errors.Store(1)
	group.firstError = "queue worker failed"
	scenario := &resourcePressureScenario{resourceScenario: &resourceScenario{workers: &sqlWorkload{group: group}}}
	started := time.Now()
	err := scenario.Hold(context.Background(), &Runtime{Config: BenchConfig{Run: RunConfig{Duration: 100 * time.Millisecond}}})
	if err == nil || !strings.Contains(err.Error(), "queue worker failed") {
		t.Fatalf("hold error=%v", err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("hold waited %s after a known worker error", elapsed)
	}
}

func evidenceMetricAvailable(evidence []Evidence, metric string) bool {
	for _, item := range evidence {
		if item.Metric == metric && item.Available {
			return true
		}
	}
	return false
}

func TestTotalMemoryAndRetentionHaveDifferentLifecyclePlans(t *testing.T) {
	total, err := memoryLifecycleFor(207)
	if err != nil {
		t.Fatal(err)
	}
	if !total.Composite || total.RetainSessions || len(total.AllocationCodes) != 4 {
		t.Fatalf("total-memory lifecycle=%+v", total)
	}
	retention, err := memoryLifecycleFor(208)
	if err != nil {
		t.Fatal(err)
	}
	if retention.Composite || !retention.RetainSessions || len(retention.AllocationCodes) != 2 {
		t.Fatalf("retention lifecycle=%+v", retention)
	}
	if retention.AllocationCodes[0] != 204 || retention.AllocationCodes[1] != 205 {
		t.Fatalf("retention allocations=%v", retention.AllocationCodes)
	}
}

func TestMemorySpecialScenarioPlansCoverTheirLifecycleMechanisms(t *testing.T) {
	runtime := &Runtime{Config: BenchConfig{Data: DataConfig{Schema: "gsbench"}}}
	total, err := ScenarioWorkloadStatements(runtime, "memory_total_pressure")
	if err != nil {
		t.Fatal(err)
	}
	if len(total) != 4 {
		t.Fatalf("total-memory plans=%d want=4", len(total))
	}
	joined := strings.Join(total, "\n")
	for _, want := range []string{"ORDER BY payload", "JOIN", "customer_id=$1", "mod(id,97)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("total-memory plans missing %q:\n%s", want, joined)
		}
	}
	retention, err := ScenarioWorkloadStatements(runtime, "memory_retention")
	if err != nil {
		t.Fatal(err)
	}
	if len(retention) != 2 {
		t.Fatalf("retention plans=%d want=2", len(retention))
	}
}
