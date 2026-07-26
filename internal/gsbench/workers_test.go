package gsbench

import (
	"context"
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
