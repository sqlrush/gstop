package dbconn

import (
	"context"
	"errors"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
)

func TestOperationContextUsesConfiguredTimeout(t *testing.T) {
	db := New(config.FromMap(map[string]any{
		"main": map[string]any{"collect_timeout": 0.05},
	}), logging.New("db-cancel-test", ""))

	ctx, cancel := db.operationContext()
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("operation context did not honor collect_timeout")
	}
}

func TestCancelImmediatelyCancelsOperationContext(t *testing.T) {
	db := New(config.FromMap(map[string]any{
		"main": map[string]any{"collect_timeout": int64(30)},
	}), logging.New("db-cancel-test", ""))
	ctx, cancel := db.operationContext()
	defer cancel()

	db.Cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("context error = %v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("DB.Cancel did not cancel an in-flight operation")
	}
}
