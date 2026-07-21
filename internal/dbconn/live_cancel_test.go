package dbconn

import (
	"os"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
)

// TestIntegrationCancelLiveQuery is opt-in because it needs the operator's
// configured openGauss/GaussDB instance. pg_sleep makes cancellation timing
// deterministic without making live infrastructure a unit-test dependency.
func TestIntegrationCancelLiveQuery(t *testing.T) {
	cfg := liveIntegrationConfig(t)
	db := New(cfg, logging.New("db-live-cancel-test", ""))
	defer db.Close()
	requireLiveConnection(t, db)

	done := make(chan []Row, 1)
	go func() {
		done <- db.Query("SELECT pg_sleep(30);")
	}()
	time.Sleep(500 * time.Millisecond)
	cancelAt := time.Now()
	db.Cancel()

	select {
	case rows := <-done:
		if rows != nil {
			t.Fatalf("formerly-hanging query completed before cancellation: %+v", rows)
		}
		if elapsed := time.Since(cancelAt); elapsed > time.Second {
			t.Fatalf("live cancellation took %v, want under 1s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live database query did not stop after DB.Cancel")
	}
}

func TestIntegrationTimeoutLiveQuery(t *testing.T) {
	cfg := liveIntegrationConfig(t)
	db := New(cfg, logging.New("db-live-timeout-test", ""))
	defer db.Close()
	requireLiveConnection(t, db)

	start := time.Now()
	rows := db.Query("SELECT pg_sleep(30);")
	elapsed := time.Since(start)
	if rows != nil {
		t.Fatalf("formerly-hanging query completed instead of timing out: %+v", rows)
	}
	if elapsed < 4*time.Second || elapsed > 7*time.Second {
		t.Fatalf("live query timeout = %v, want approximately collect_timeout=5s", elapsed)
	}
}

func liveIntegrationConfig(t *testing.T) *config.Config {
	t.Helper()
	path := os.Getenv("GSTOP_INTEGRATION_CONFIG")
	if path == "" {
		t.Skip("set GSTOP_INTEGRATION_CONFIG to run against a live instance")
	}
	cfg, err := config.Load(path, config.Args{})
	if err != nil {
		t.Fatalf("load integration config: %v", err)
	}
	return cfg
}

func requireLiveConnection(t *testing.T, db *DB) {
	t.Helper()
	rows := db.Query("SELECT 1;")
	if len(rows) != 1 || rows[0].Str(0) != "1" {
		t.Fatalf("live integration preflight failed: %+v", rows)
	}
}
