package monitor

import (
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/health"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/oscmd"
)

func TestInstanceFailedRoundBlanksValuesAndRestoresSamplingBaseline(t *testing.T) {
	baselineTime := time.Date(2026, 7, 20, 10, 0, 0, 0, time.Local)
	m := &InstanceMonitor{
		items:    []string{"time", "TPS", "QPS", "XLOG(kB/s)"},
		values:   []string{"", "12", "34", "56"},
		lastTime: baselineTime,
		interval: 2,
		prevTPS:  100,
		prevQPS:  200,
		prevXlog: 300,
	}
	saved := m.captureSampleState()
	m.lastTime = baselineTime.Add(time.Second)
	m.interval = 1
	m.prevTPS = 101
	m.prevQPS = 201
	m.prevXlog = 301
	m.failRefresh(saved)

	if m.lastTime != baselineTime || m.interval != 2 || m.prevTPS != 100 || m.prevQPS != 200 || m.prevXlog != 300 {
		t.Fatalf("failed round changed baseline: time=%v interval=%v tps=%d qps=%d xlog=%d",
			m.lastTime, m.interval, m.prevTPS, m.prevQPS, m.prevXlog)
	}
	for i, value := range m.MonitorValues() {
		if value != "" {
			t.Fatalf("blank value[%d] = %q", i, value)
		}
	}
}

func TestDBMonitorFailedCellIsBlank(t *testing.T) {
	cfg := config.FromMap(map[string]any{"main": map[string]any{"collect_timeout": int64(5)}})
	logger := logging.New("monitor-failure-test", "")
	db := dbconn.New(cfg, logger)
	db.Cancel()
	deps := Deps{Cfg: cfg, DB: db, OS: oscmd.New(logger, time.Second), Logger: logger, Health: health.New(cfg)}
	m := &DBMonitor{
		base:      newBase(dbName, dbHeight, deps),
		items:     []string{"FAILED"},
		methods:   []string{"select 1"},
		osMethods: []string{""},
	}
	m.Refresh()
	if got := m.MonitorValues(); len(got) != 1 || got[0] != "" {
		t.Fatalf("failed DB cell = %+v, want one blank value", got)
	}
}

func TestEventFailedRoundBlanksLinesButKeepsSamplingBaseline(t *testing.T) {
	m := &EventMonitor{
		lines:      []eventLine{{cols: [6]string{"DataFileRead"}}},
		lastCPU:    10,
		lastTotal:  20,
		lastEvents: map[string]eventSample{"DataFileRead": {waits: 3, timeUs: 4}},
	}
	m.clearFailedRound()
	if len(m.lines) != 0 {
		t.Fatalf("failed event round retained rows: %+v", m.lines)
	}
	if m.lastCPU != 10 || m.lastTotal != 20 || m.lastEvents["DataFileRead"] != (eventSample{waits: 3, timeUs: 4}) {
		t.Fatalf("failed event round changed baseline: %+v", m)
	}
}

func TestSessionFailedRoundBlanksPublishedRows(t *testing.T) {
	m := &SessionMonitor{
		values:         []model.SessionRow{{int64(1)}},
		currSessResult: []dbconn.Row{{int64(1)}},
	}
	m.clearFailedRound()
	if len(m.values) != 0 || len(m.Session()) != 0 {
		t.Fatalf("failed session round retained rows: values=%+v sessions=%+v", m.values, m.Session())
	}
}
