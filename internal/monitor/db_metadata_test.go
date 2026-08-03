package monitor

import (
	"context"
	"sync"
	"testing"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/model"
)

func TestDBMonitorSuccessfulMetadataCommitIsAtomic(t *testing.T) {
	cfg := config.FromMap(map[string]any{})
	logger := logging.New("db-metadata-test", "")
	info := model.NewDBInfo()
	a := model.DBInfoSnapshot{Version: "version-a", User: "user-a", Role: "primary"}
	b := model.DBInfoSnapshot{Version: "version-b", User: "user-b", Role: "standby"}
	info.SetSnapshot(a)
	current := a
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: cfg, Logger: logger, Health: health.New(cfg),
		}),
		items:     []string{"GaussDB", "USER", "time", "d", "PRI"},
		methods:   []string{"version", "user", "", "", ""},
		osMethods: []string{"", "", "", "", "primary node probe"},
		values:    make([]string, 5),
		dbInfo:    info,
	}
	m.queryContext = func(_ context.Context, query string) []dbconn.Row {
		switch query {
		case "version":
			return []dbconn.Row{{current.Version}}
		case "user":
			return []dbconn.Row{{current.User}}
		default:
			t.Fatalf("unexpected query %q", query)
			return nil
		}
	}
	m.runContext = func(_ context.Context, command string, _ bool) (string, bool) {
		switch command {
		case "primary node probe":
			return "10.0.0.1", true
		case "ifconfig":
			if current.Role == "primary" {
				return "eth0: inet 10.0.0.1", true
			}
			return "eth0: inet 10.0.0.2", true
		default:
			t.Fatalf("unexpected command %q", command)
			return "", false
		}
	}

	const iterations = 50_000
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				got := info.Snapshot()
				if got != a && got != b {
					t.Errorf("mixed DB monitor metadata: %+v", got)
					return
				}
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		if i%2 == 0 {
			current = b
		} else {
			current = a
		}
		m.version, m.user, m.primaryNode = "", "", ""
		if err := m.RefreshWithContext(context.Background()); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
}
