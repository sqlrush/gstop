package monitor

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/oscmd"
)

func TestInstanceFailedRoundRetainsValuesAndRestoresSamplingBaseline(t *testing.T) {
	baselineTime := time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local)
	m := &InstanceMonitor{
		items:    []string{"time", "TPS", "QPS", "XLOG(kB/s)"},
		values:   []string{"old-time", "12", "34", "56"},
		lastTime: baselineTime, interval: 2, prevTPS: 100, prevQPS: 200, prevXlog: 300,
	}
	saved := m.captureSampleState()
	m.lastTime = baselineTime.Add(time.Second)
	m.interval, m.prevTPS, m.prevQPS, m.prevXlog = 1, 101, 201, 301
	m.failRefresh(saved)
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"old-time", "12", "34", "56"}) {
		t.Fatalf("failed round values=%v", got)
	}
	if m.lastTime != baselineTime || m.interval != 2 ||
		m.prevTPS != 100 || m.prevQPS != 200 || m.prevXlog != 300 {
		t.Fatalf("failed round changed sampling baseline: %+v", m.captureSampleState())
	}
}

func TestInstanceRefreshWithContextLateFailureRestoresPublishedSnapshotAndBaseline(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	baselineTime := time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local)
	oldValues := []string{"old-time", "12"}
	m := &InstanceMonitor{
		base:     newBase(instanceName, instanceHeight, Deps{Logger: logger}),
		items:    []string{"time", "TPS"},
		methods:  []string{"time-query", "tps-query"},
		values:   oldValues,
		lastTime: baselineTime,
		interval: 2,
		prevTPS:  100,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			if query == "time-query" {
				return []dbconn.Row{{baselineTime.Add(time.Second)}}
			}
			return nil
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("late instance failure returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, oldValues) {
		t.Fatalf("failed instance values=%v", got)
	}
	if m.lastTime != baselineTime || m.interval != 2 || m.prevTPS != 100 {
		t.Fatalf("failed instance baseline=%+v", m.captureSampleState())
	}
}

func TestInstanceRefreshRejectsCrossAssociatedSessionRow(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	baselineTime := time.Date(2026, 8, 1, 19, 12, 9, 0, time.Local)
	oldValues := []string{
		"0", "16", "8", "8", "0", "8", "1.6%(16/1000)",
	}
	tests := []struct {
		name string
		row  dbconn.Row
	}{
		{
			name: "wait event result",
			row: dbconn.Row{
				int64(10494930208), "BufHashTableSearch",
				int64(1940789603), int64(1968176646), int64(1),
			},
		},
		{
			name: "xlog result",
			row:  dbconn.Row{"18B/6800830"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &InstanceMonitor{
				base: newBase(instanceName, instanceHeight, Deps{
					Cfg: config.FromMap(map[string]any{}), Logger: logger,
				}),
				items: []string{
					"time", "SN", "AN", "ASC", "ASI", "IDL", "CONNECTION(c/m)",
				},
				methods: []string{
					"time-query", "session-query", "", "", "", "", "",
				},
				values: oldValues, maxConnections: 1000,
				lastTime: baselineTime, interval: 1,
				queryContext: func(_ context.Context, query string) []dbconn.Row {
					switch query {
					case "time-query":
						return []dbconn.Row{{baselineTime.Add(time.Second)}}
					case "session-query":
						return []dbconn.Row{test.row}
					default:
						t.Fatalf("unexpected query %q", query)
						return nil
					}
				},
			}

			if err := m.RefreshWithContext(context.Background()); err == nil {
				t.Fatal("cross-associated session result returned nil error")
			}
			if got := m.MonitorValues(); !reflect.DeepEqual(got, oldValues) {
				t.Fatalf("cross-associated session result published values=%v", got)
			}
			if m.lastTime != baselineTime || m.interval != 1 {
				t.Fatalf("cross-associated session result changed baseline=%+v", m.captureSampleState())
			}
		})
	}
}

func TestInstanceRefreshRejectsCrossAssociatedXlogRow(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	baselineTime := time.Date(2026, 8, 1, 19, 12, 8, 0, time.Local)
	m := &InstanceMonitor{
		base: newBase(instanceName, instanceHeight, Deps{
			Cfg: config.FromMap(map[string]any{}), Logger: logger,
		}),
		items:    []string{"time", "XLOG(kB/s)"},
		methods:  []string{"time-query", "xlog-query"},
		values:   []string{"0", "128"},
		lastTime: baselineTime,
		interval: 1,
		prevXlog: 4096,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			switch query {
			case "time-query":
				return []dbconn.Row{{baselineTime.Add(time.Second)}}
			case "xlog-query":
				// This is the real cross-associated TPS/QPS-shaped value seen in v3.
				return []dbconn.Row{{int64(3571632)}}
			default:
				t.Fatalf("unexpected query %q", query)
				return nil
			}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("cross-associated xlog result returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"0", "128"}) {
		t.Fatalf("cross-associated xlog result published values=%v", got)
	}
	if m.lastTime != baselineTime || m.interval != 1 || m.prevXlog != 4096 {
		t.Fatalf("cross-associated xlog result changed baseline=%+v", m.captureSampleState())
	}
}

func TestInstanceRefreshRejectsCrossAssociatedScalarShapes(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	baselineTime := time.Date(2026, 8, 1, 19, 12, 8, 0, time.Local)
	sessionRow := dbconn.Row{
		int64(281454774376096), "omm", "gstop", int64(281454774376096),
		int64(4099012020), "select 1", "SELECT", nil, int64(1000),
		"active", "ON CPU", "none", nil, nil, nil, "postgres", nil, nil,
		int64(1000), "none", nil,
	}
	tests := []struct {
		name string
		item string
		row  dbconn.Row
	}{
		{name: "timestamp receives counter", item: "time", row: dbconn.Row{int64(3571632)}},
		{name: "counter receives xlog", item: "TPS", row: dbconn.Row{"18B/6800830"}},
		{name: "percentile receives event", item: "P80(ms)", row: dbconn.Row{
			int64(10494930208), "BufHashTableSearch",
			int64(1940789603), int64(1968176646), float64(1), "LWLock",
		}},
		{name: "threadpool receives session", item: "THREADPOOL", row: sessionRow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldValues := []string{"old"}
			m := &InstanceMonitor{
				base: newBase(instanceName, instanceHeight, Deps{
					Cfg: config.FromMap(map[string]any{}), Logger: logger,
				}),
				items: []string{test.item}, methods: []string{"query"}, values: oldValues,
				lastTime: baselineTime, interval: 1,
				prevTPS: 100, prevQPS: 200, prevXlog: 4096,
				queryContext: func(_ context.Context, query string) []dbconn.Row {
					if query != "query" {
						t.Fatalf("unexpected query %q", query)
					}
					return []dbconn.Row{test.row}
				},
			}

			if err := m.RefreshWithContext(context.Background()); err == nil {
				t.Fatal("cross-associated scalar result returned nil error")
			}
			if got := m.MonitorValues(); !reflect.DeepEqual(got, oldValues) {
				t.Fatalf("cross-associated scalar result published values=%v", got)
			}
			if got := m.captureSampleState(); got != (instanceSampleState{
				lastTime: baselineTime, interval: 1,
				prevTPS: 100, prevQPS: 200, prevXlog: 4096,
			}) {
				t.Fatalf("cross-associated scalar result changed baseline=%+v", got)
			}
		})
	}
}

func TestDBMonitorFailedRoundRetainsPreviousValues(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{"collect_timeout": 30.0}})
	logger := logging.New("monitor-failure-test", "")
	db := dbconn.New(cfg, logger)
	db.Cancel()
	deps := Deps{
		Cfg: cfg, DB: db, OS: oscmd.New(logger, time.Second),
		Logger: logger, Health: health.New(cfg),
	}
	m := &DBMonitor{
		base:      newBase(dbName, dbHeight, deps),
		items:     []string{"FAILED"},
		methods:   []string{"select 1"},
		osMethods: []string{""},
		values:    []string{"old"},
	}
	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("failed DB refresh returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("failed DB values=%v", got)
	}
}

func TestDBMonitorLateFailureDoesNotLeakMetadataOrPcacheThrottle(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_enable":     true,
		"dynamic_mem_cpu_thresh": int64(50),
		"dynamic_mem_interval":   int64(60),
	}})
	logger := logging.New("monitor-failure-test", "")
	memHealth := health.New(cfg)
	info := model.NewDBInfo()
	info.SetVersion("old-version")
	info.SetUser("old-user")
	info.SetRole("old-role")
	oldBusy := busyResult{cpuTime: 1, dbTime: 2, ts: time.Unix(3, 0), valid: true}
	oldLastBusy := []busyResult{{cpuTime: 4, valid: true}}
	oldValues := []string{"old-v", "old-u", "old-t", "old-d", "old-p", "old-c", "old-f"}
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: cfg, Logger: logger, Health: memHealth,
		}),
		items:     []string{"GaussDB", "USER", "time", "d", "PRI", "MB pcache", "FAILED"},
		methods:   []string{"version", "user", "", "", "", "pcache", "fail"},
		osMethods: []string{"", "", "", "", "primary node probe", "", ""},
		values:    oldValues,
		counter:   7,
		pcache:    "old-pcache",
		busy:      oldBusy,
		lastBusy:  oldLastBusy,
		dbInfo:    info,
	}
	var metadataLeaked bool
	var pcacheEligibleAtFailure bool
	m.queryContext = func(_ context.Context, query string) []dbconn.Row {
		switch query {
		case "version":
			return []dbconn.Row{{"new-version"}}
		case "user":
			return []dbconn.Row{{"new-user"}}
		case "pcache":
			return []dbconn.Row{{float64(128)}}
		case "fail":
			metadataLeaked = info.Version() != "old-version" ||
				info.User() != "old-user" || info.Role() != "old-role"
			pcacheEligibleAtFailure = memHealth.CanRefreshMemory("pcache")
			return nil
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
			return "eth0: inet 10.0.0.1 netmask 255.255.255.0", true
		default:
			t.Fatalf("unexpected command %q", command)
			return "", false
		}
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("late DB failure returned nil error")
	}
	if metadataLeaked {
		t.Fatal("DBInfo exposed partial metadata before the failed cycle returned")
	}
	if !pcacheEligibleAtFailure || !memHealth.CanRefreshMemory("pcache") {
		t.Fatal("failed DB cycle consumed pcache refresh eligibility")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, oldValues) {
		t.Fatalf("failed DB values=%v, want %v", got, oldValues)
	}
	if m.counter != 7 || m.version != "" || m.user != "" ||
		m.pcache != "old-pcache" || m.primaryNode != "" ||
		m.busy != oldBusy || !reflect.DeepEqual(m.lastBusy, oldLastBusy) {
		t.Fatalf("failed DB cycle leaked cache/baseline state: %+v", m.captureRefreshState())
	}
	if info.Version() != "old-version" || info.User() != "old-user" || info.Role() != "old-role" {
		t.Fatalf("failed DB metadata=%q/%q/%q", info.Version(), info.User(), info.Role())
	}
}

func TestDBMonitorRejectsCrossAssociatedBusyRow(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	oldBusy := busyResult{
		cpuTime: 100, dbTime: 200,
		ts: time.Date(2026, 8, 1, 19, 12, 9, 0, time.Local), valid: true,
	}
	oldLastBusy := []busyResult{{
		cpuTime: 90, dbTime: 180,
		ts: oldBusy.ts.Add(-time.Second), valid: true,
	}}
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: config.FromMap(map[string]any{}), Logger: logger,
		}),
		items:     []string{"db%"},
		methods:   []string{"busy-query"},
		osMethods: []string{"nproc"},
		values:    []string{"12.5"},
		busy:      oldBusy,
		lastBusy:  oldLastBusy,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			if query != "busy-query" {
				t.Fatalf("unexpected query %q", query)
			}
			// This is the real wait-event-shaped row associated with db% in v3.
			return []dbconn.Row{{
				int64(10494930208), "BufHashTableSearch",
				int64(1940789603), int64(1968176646), float64(1), "LWLock",
			}}
		},
		runContext: func(_ context.Context, command string, _ bool) (string, bool) {
			if command != "nproc" {
				t.Fatalf("unexpected command %q", command)
			}
			return "4", true
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("cross-associated busy result returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"12.5"}) {
		t.Fatalf("cross-associated busy result published values=%v", got)
	}
	if m.busy != oldBusy || !reflect.DeepEqual(m.lastBusy, oldLastBusy) {
		t.Fatalf("cross-associated busy result changed baseline=%+v", m.captureRefreshState())
	}
}

func TestDBMonitorRejectsCrossAssociatedScalarRow(t *testing.T) {
	cfg := config.FromMap(map[string]any{
		"main": map[string]any{"dynamic_mem_enable": true},
	})
	logger := logging.New("monitor-failure-test", "")
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: cfg, Logger: logger, Health: health.New(cfg),
		}),
		items:     []string{"MB dyn"},
		methods:   []string{"dynamic-memory-query"},
		osMethods: []string{""},
		values:    []string{"256"},
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			if query != "dynamic-memory-query" {
				t.Fatalf("unexpected query %q", query)
			}
			return []dbconn.Row{{int64(17), int64(4), int64(2), int64(1), int64(13)}}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("cross-associated scalar result returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"256"}) {
		t.Fatalf("cross-associated scalar result published values=%v", got)
	}
}

func TestDBMonitorPrimaryDetectionFailureRejectsCycle(t *testing.T) {
	cfg := config.FromMap(map[string]any{})
	logger := logging.New("monitor-failure-test", "")
	info := model.NewDBInfo()
	info.SetVersion("old-version")
	info.SetUser("old-user")
	info.SetRole("old-role")
	oldValues := []string{"old-v", "old-u", "old-t", "old-d", "old-primary"}
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: cfg, Logger: logger, Health: health.New(cfg),
		}),
		items:     []string{"GaussDB", "USER", "time", "d", "PRI"},
		methods:   make([]string, 5),
		osMethods: []string{"", "", "", "", "primary node probe"},
		values:    oldValues,
		version:   "old-version",
		user:      "old-user",
		dbInfo:    info,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			t.Fatalf("unexpected query %q", query)
			return nil
		},
	}
	m.runContext = func(_ context.Context, command string, _ bool) (string, bool) {
		if command == "primary node probe" {
			return "10.0.0.1", true
		}
		if command != "ifconfig" {
			t.Fatalf("unsafe primary detection command %q", command)
		}
		return "", false
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("failed primary detector returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, oldValues) {
		t.Fatalf("primary detector failure published values=%v", got)
	}
	if info.Role() != "old-role" {
		t.Fatalf("primary detector failure published role=%q", info.Role())
	}
}

func TestDBMonitorPrimaryDetectionNoMatchPublishesStandbyWithoutNodeInterpolation(t *testing.T) {
	cfg := config.FromMap(map[string]any{})
	logger := logging.New("monitor-failure-test", "")
	info := model.NewDBInfo()
	info.SetVersion("old-version")
	info.SetUser("old-user")
	info.SetRole("old-role")
	node := "10.0.0.1; echo injected"
	m := &DBMonitor{
		base: newBase(dbName, dbHeight, Deps{
			Cfg: cfg, Logger: logger, Health: health.New(cfg),
		}),
		items:     []string{"GaussDB", "USER", "time", "d", "PRI"},
		methods:   make([]string, 5),
		osMethods: []string{"", "", "", "", "primary node probe"},
		values:    []string{"old-v", "old-u", "old-t", "old-d", "old-primary"},
		version:   "old-version",
		user:      "old-user",
		dbInfo:    info,
	}
	var commands []string
	m.runContext = func(_ context.Context, command string, _ bool) (string, bool) {
		commands = append(commands, command)
		switch len(commands) {
		case 1:
			return node, true
		case 2:
			return "eth0: inet 10.0.0.2 netmask 255.255.255.0", true
		default:
			t.Fatalf("unexpected command %q", command)
			return "", false
		}
	}

	if err := m.RefreshWithContext(context.Background()); err != nil {
		t.Fatalf("successful no-match returned error: %v", err)
	}
	if !reflect.DeepEqual(commands, []string{"primary node probe", "ifconfig"}) {
		t.Fatalf("primary detection commands=%q", commands)
	}
	if strings.Contains(commands[1], node) {
		t.Fatalf("primary node was interpolated into shell command %q", commands[1])
	}
	if info.Role() != "standby" {
		t.Fatalf("successful no-match role=%q", info.Role())
	}
}

func TestEventFailedRoundRetainsPublishedLinesAndSamplingBaseline(t *testing.T) {
	old := []eventLine{{cols: [6]string{"DataFileRead"}}}
	m := &EventMonitor{
		lines: old, lastCPU: 10, lastTotal: 20,
		lastEvents: map[string]eventSample{"DataFileRead": {waits: 3, timeUs: 4}},
	}
	m.failRefresh()
	if !reflect.DeepEqual(m.lines, old) || m.lastCPU != 10 || m.lastTotal != 20 {
		t.Fatalf("failed event round mutated snapshot: %+v", m)
	}
}

func TestEventRefreshWithContextValidationFailureRetainsSnapshotAndBaseline(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	old := []eventLine{{cols: [6]string{"DataFileRead"}}}
	oldEvents := map[string]eventSample{"DataFileRead": {waits: 3, timeUs: 4}}
	m := &EventMonitor{
		base:  newBase(eventName, eventHeight, Deps{Logger: logger}),
		lines: old, lastCPU: 10, lastTotal: 20, lastEvents: oldEvents,
		immediate: true, currImmediate: false,
		queryContext: func(_ context.Context, _ string) []dbconn.Row {
			return []dbconn.Row{{int64(11), "DataFileRead", int64(4), int64(0), float64(0), "IO"}}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("invalid event totals returned nil error")
	}
	if !reflect.DeepEqual(m.lines, old) || m.lastCPU != 10 || m.lastTotal != 20 ||
		!reflect.DeepEqual(m.lastEvents, oldEvents) || m.currImmediate {
		t.Fatalf("failed event cycle mutated snapshot/baseline: %+v", m)
	}
}

func TestEventRefreshRejectsCrossAssociatedSessionRows(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	old := []eventLine{{cols: [6]string{"DataFileRead"}}}
	oldEvents := map[string]eventSample{"DataFileRead": {waits: 3, timeUs: 4}}
	sessionRow := dbconn.Row{
		int64(281454774376096), "omm", "gstop", int64(281454774376096),
		int64(4099012020), "select 1", "SELECT", nil, int64(1000),
		"active", "ON CPU", "none", nil, nil, nil, "postgres", nil, nil,
		int64(1000), "none", nil,
	}
	m := &EventMonitor{
		base:  newBase(eventName, eventHeight, Deps{Logger: logger}),
		lines: old, lastCPU: 10, lastTotal: 20, lastEvents: oldEvents,
		immediate: true, currImmediate: true,
		queryContext: func(_ context.Context, _ string) []dbconn.Row {
			return []dbconn.Row{sessionRow}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("cross-associated session rows returned nil error")
	}
	if !reflect.DeepEqual(m.lines, old) || m.lastCPU != 10 || m.lastTotal != 20 ||
		!reflect.DeepEqual(m.lastEvents, oldEvents) {
		t.Fatalf("cross-associated session rows changed event snapshot/baseline: %+v", m)
	}
}

func TestSessionFailedRoundRetainsPublishedRows(t *testing.T) {
	oldValues := []model.SessionRow{{int64(1)}}
	oldRaw := []dbconn.Row{{int64(1)}}
	m := &SessionMonitor{values: oldValues, currSessResult: oldRaw}
	m.failRefresh()
	if !reflect.DeepEqual(m.values, oldValues) || !reflect.DeepEqual(m.Session(), oldRaw) {
		t.Fatalf("failed session round values=%+v raw=%+v", m.values, m.Session())
	}
}

func TestSessionRefreshWithContextLateFailureRetainsPublishedRows(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{"dynamic_mem_enable": true}})
	logger := logging.New("monitor-failure-test", "")
	oldValues := []model.SessionRow{{int64(1)}}
	oldRaw := []dbconn.Row{{int64(1)}}
	db := dbconn.New(cfg, logger)
	m := &SessionMonitor{
		base:           newBase(sessionName, sessionHeight, Deps{Cfg: cfg, DB: db, Logger: logger}),
		values:         oldValues,
		currSessResult: oldRaw,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			switch query {
			case sessionMemQuery, sessionStatementQuery:
				return []dbconn.Row{}
			default:
				return nil
			}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("late session failure returned nil error")
	}
	if !reflect.DeepEqual(m.values, oldValues) || !reflect.DeepEqual(m.Session(), oldRaw) {
		t.Fatalf("failed session values=%+v raw=%+v", m.values, m.Session())
	}
}

func TestSessionRefreshRejectsCrossAssociatedEventRows(t *testing.T) {
	cfg := config.FromMap(map[string]any{})
	logger := logging.New("monitor-failure-test", "")
	oldValues := []model.SessionRow{{int64(1)}}
	oldRaw := []dbconn.Row{{int64(1)}}
	db := dbconn.New(cfg, logger)
	m := &SessionMonitor{
		base: newBase(sessionName, sessionHeight, Deps{
			Cfg: cfg, DB: db, Logger: logger,
		}),
		values: oldValues, currSessResult: oldRaw,
		queryContext: func(_ context.Context, query string) []dbconn.Row {
			switch query {
			case sessionStatementQuery:
				return []dbconn.Row{}
			case sessionQueryFor(db.Kind()):
				return []dbconn.Row{{
					int64(10494930208), "BufHashTableSearch",
					int64(1940789603), int64(1968176646), float64(1), "LWLock",
				}}
			default:
				t.Fatalf("unexpected query %q", query)
				return nil
			}
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("cross-associated event rows returned nil error")
	}
	if !reflect.DeepEqual(m.values, oldValues) || !reflect.DeepEqual(m.Session(), oldRaw) {
		t.Fatalf("cross-associated event rows changed session snapshot: %+v", m.values)
	}
}

func TestOSMonitorCanceledRoundRetainsPreviousValues(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	cfg := config.FromMap(map[string]any{})
	m := &OSMonitor{
		base: newBase(osName, osHeight, Deps{
			Cfg: cfg, OS: oscmd.New(logger, time.Second),
			Logger: logger, Health: health.New(cfg),
		}),
		items: []string{"%CPU"}, valueTypes: []string{"cpu"},
		widths: []int{10}, values: []string{"12.34"},
		osCmd: []string{"printf disk", "printf cpu", "printf mem", "printf load"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.RefreshWithContext(ctx); err == nil {
		t.Fatal("canceled OS refresh returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"12.34"}) {
		t.Fatalf("failed OS values=%v", got)
	}
}

func TestOSMonitorLateProbeFailureRetainsSnapshotAndBaselines(t *testing.T) {
	logger := logging.New("monitor-failure-test", "")
	cfg := config.FromMap(map[string]any{})
	oldDisk := [diskFields]int64{1, 2, 3, 4, 5, 6}
	oldCPU := [cpuFields]int64{7, 8, 9, 10, 11, 12, 13}
	oldDiskTime := time.Unix(100, 0)
	oldCPUTime := time.Unix(200, 0)
	m := &OSMonitor{
		base: newBase(osName, osHeight, Deps{
			Cfg: cfg, Logger: logger, Health: health.New(cfg),
		}),
		items: []string{"%CPU"}, valueTypes: []string{"CPU"},
		widths: []int{10}, values: []string{"12.34"},
		osCmd:         []string{"disk", "cpu", "mem", "load"},
		osValues:      [osCmdCount]string{"old-disk", "old-cpu", "old-mem", "old-load"},
		osOK:          [osCmdCount]bool{true, true, true, true},
		prevDiskstats: oldDisk,
		curDiskstats:  oldDisk,
		lastDiskTime:  oldDiskTime,
		hasDiskTime:   true,
		lastCPUInfo:   oldCPU,
		curCPUInfo:    oldCPU,
		lastCPUTime:   oldCPUTime,
		hasCPUTime:    true,
		runContext: func(_ context.Context, command string, _ bool) (string, bool) {
			if command == "load" {
				return "", false
			}
			return "new-" + command, true
		},
	}

	if err := m.RefreshWithContext(context.Background()); err == nil {
		t.Fatal("late OS probe failure returned nil error")
	}
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"12.34"}) {
		t.Fatalf("failed OS values=%v", got)
	}
	if m.osValues != ([osCmdCount]string{"old-disk", "old-cpu", "old-mem", "old-load"}) ||
		m.osOK != ([osCmdCount]bool{true, true, true, true}) ||
		m.prevDiskstats != oldDisk || m.curDiskstats != oldDisk ||
		m.lastDiskTime != oldDiskTime || !m.hasDiskTime ||
		m.lastCPUInfo != oldCPU || m.curCPUInfo != oldCPU ||
		m.lastCPUTime != oldCPUTime || !m.hasCPUTime {
		t.Fatalf("late OS failure mutated sampling state: %+v", m)
	}
}
