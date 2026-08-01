package gsbench

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerGroupCancellationIsBounded(t *testing.T) {
	started := make(chan struct{}, 4)
	group := NewWorkerGroup(context.Background(), 4, func(ctx context.Context, _ int) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})
	if err := group.SetTarget(4); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		<-started
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := group.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if got := group.Snapshot().Active; got != 0 {
		t.Fatalf("active workers = %d", got)
	}
}

func TestWorkerGroupHonorsMaximum(t *testing.T) {
	group := NewWorkerGroup(context.Background(), 2, func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err := group.SetTarget(3); err == nil {
		t.Fatal("expected maximum worker error")
	}
	_ = group.Stop(context.Background())
}

func TestWorkerGroupExposesFirstExecutionError(t *testing.T) {
	sentinel := errors.New("sentinel workload error")
	group := NewWorkerGroup(context.Background(), 1, func(context.Context, int) error {
		return sentinel
	})
	if err := group.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for group.Snapshot().Errors == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	snapshot := group.Snapshot()
	if snapshot.Errors == 0 {
		t.Fatal("worker did not record the execution error")
	}
	if snapshot.FirstError != sentinel.Error() {
		t.Fatalf("first error=%q", snapshot.FirstError)
	}
	if err := group.ExecutionError(); err == nil || !strings.Contains(err.Error(), sentinel.Error()) {
		t.Fatalf("execution error=%v", err)
	}
	_ = group.Stop(context.Background())
}

func TestWorkerGroupBoundsErrorTextAndBacksOffRetries(t *testing.T) {
	var calls atomic.Int64
	group := NewWorkerGroup(context.Background(), 1, func(context.Context, int) error {
		calls.Add(1)
		return errors.New(strings.Repeat("x", 1024))
	})
	if err := group.SetTarget(1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(35 * time.Millisecond)
	if got := group.Snapshot().FirstError; len(got) == 0 || len(got) > 256 {
		t.Fatalf("bounded first error length=%d", len(got))
	}
	if got := calls.Load(); got > 5 {
		t.Fatalf("deterministic worker error retried without backoff: calls=%d", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := group.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerDrivenScenariosExposeExecutionSnapshots(t *testing.T) {
	factories := DefaultScenarioFactories()
	for _, code := range []ScenarioCode{
		101, 102, 103,
		201, 202, 203, 204, 205, 207, 208,
		301, 302, 303, 304, 321, 322, 331, 332, 333,
		401, 402, 403, 404,
		621, 622, 623,
	} {
		definition := DefaultScenarioCatalog().MustCode(code)
		scenario, err := factories[code](definition, Environment{})
		if err != nil {
			t.Fatalf("scenario %03d: %v", code, err)
		}
		if _, ok := scenario.(executionReporter); !ok {
			t.Errorf("worker-driven scenario %03d does not expose execution errors", code)
		}
	}
}
