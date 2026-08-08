package gsbench

import (
	"context"
	"errors"
	"testing"
)

func TestThreadTargetDeadlineRemainsFailureWithoutWrappingDeadline(
	t *testing.T,
) {
	err := threadTargetControlError(ControlResult{
		Err: context.DeadlineExceeded, Actual: 89, Workers: 10,
	}, 90)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline target error=%v", err)
	}
}

func TestFrozenThreadWorkersMustRemainEstablished(t *testing.T) {
	if err := validateFrozenWorkerSnapshot(WorkerSnapshot{
		Target: 10, Active: 10,
	}, 10); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []WorkerSnapshot{
		{Target: 10, Active: 9},
		{Target: 9, Active: 9},
	} {
		if err := validateFrozenWorkerSnapshot(snapshot, 10); err == nil {
			t.Fatalf("accepted lost frozen worker: %+v", snapshot)
		}
	}
}
