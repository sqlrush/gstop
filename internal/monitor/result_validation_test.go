package monitor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
)

func TestInstanceConfigActiveIOExcludesIdleSessions(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve monitor test location")
	}
	contents, err := os.ReadFile(filepath.Join(
		filepath.Dir(filename), "..", "..", "configs", "monitor", "instance.cfg",
	))
	if err != nil {
		t.Fatal(err)
	}
	const activeIO = "count(CASE WHEN state IS NOT NULL AND state NOT LIKE 'idle%' AND W.wait_status = 'wait io' THEN 1 END) AS asi"
	if !strings.Contains(string(contents), activeIO) {
		t.Fatal("ASI query counts idle sessions even though the aggregate is validated as active I/O")
	}
}

func TestInstanceRejectsCrossAssociatedSessionRows(t *testing.T) {
	logger := logging.New("monitor-result-validation-test", "")
	tests := []struct {
		name string
		row  dbconn.Row
	}{
		{
			name: "wait event row",
			row: dbconn.Row{
				int64(10494930208), "BufHashTableSearch",
				int64(1940789603), int64(1968176646), float64(1), "LWLock",
			},
		},
		{name: "xlog row", row: dbconn.Row{"18B/6800830"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &InstanceMonitor{
				base:         newBase(instanceName, instanceHeight, Deps{Logger: logger}),
				items:        []string{"SN"},
				methods:      []string{"session query"},
				queryContext: func(context.Context, string) []dbconn.Row { return []dbconn.Row{test.row} },
			}
			values := []string{"old"}
			var sessionRow dbconn.Row
			if m.refreshSession(context.Background(), 0, values, &sessionRow) {
				t.Fatal("cross-associated session result was accepted")
			}
			if !reflect.DeepEqual(values, []string{"old"}) || sessionRow != nil {
				t.Fatalf("rejected session result mutated values=%v row=%v", values, sessionRow)
			}
		})
	}
}

func TestInstanceAcceptsValidSessionAggregate(t *testing.T) {
	m := &InstanceMonitor{
		base:    newBase(instanceName, instanceHeight, Deps{Logger: logging.New("monitor-result-validation-test", "")}),
		items:   []string{"SN"},
		methods: []string{"session query"},
		queryContext: func(context.Context, string) []dbconn.Row {
			return []dbconn.Row{{int64(19), int64(8), int64(7), int64(0), int64(11)}}
		},
	}
	values := []string{"old"}
	var sessionRow dbconn.Row
	if !m.refreshSession(context.Background(), 0, values, &sessionRow) {
		t.Fatal("valid session aggregate was rejected")
	}
	if values[0] != "19" || !reflect.DeepEqual(sessionRow, dbconn.Row{
		int64(19), int64(8), int64(7), int64(0), int64(11),
	}) {
		t.Fatalf("valid session aggregate values=%v row=%v", values, sessionRow)
	}
}

func TestInstanceRejectsCrossAssociatedXlogValue(t *testing.T) {
	m := &InstanceMonitor{
		base:     newBase(instanceName, instanceHeight, Deps{Logger: logging.New("monitor-result-validation-test", "")}),
		items:    []string{"XLOG(kB/s)"},
		methods:  []string{"xlog query"},
		interval: 1,
		prevXlog: 4096,
		queryContext: func(context.Context, string) []dbconn.Row {
			return []dbconn.Row{{int64(3571632)}}
		},
	}
	values := []string{"old"}
	if m.refreshXlog(context.Background(), 0, values) {
		t.Fatal("cross-associated xlog value was accepted")
	}
	if values[0] != "old" || m.prevXlog != 4096 {
		t.Fatalf("rejected xlog result mutated values=%v prev=%d", values, m.prevXlog)
	}
}

func TestParseBusyRejectsCrossAssociatedRows(t *testing.T) {
	tests := []dbconn.Row{
		{
			int64(10494930208), "BufHashTableSearch",
			int64(1940789603), int64(1968176646), float64(1), "LWLock",
		},
		{"18B/6800830"},
	}
	for _, row := range tests {
		if got := parseBusy(row); got.valid {
			t.Fatalf("parseBusy accepted cross-associated row %v as %+v", row, got)
		}
	}

	ts := time.Date(2026, 8, 1, 19, 12, 9, 0, time.Local)
	if got := parseBusy(dbconn.Row{float64(100), float64(200), ts}); !got.valid || got.cpuTime != 100 || got.dbTime != 200 || got.ts != ts {
		t.Fatalf("parseBusy rejected valid sample: %+v", got)
	}
}

func TestDBRefreshRejectsCrossAssociatedBusyRow(t *testing.T) {
	logger := logging.New("monitor-result-validation-test", "")
	oldBusy := busyResult{
		cpuTime: 100, dbTime: 200,
		ts: time.Date(2026, 8, 1, 19, 12, 9, 0, time.Local), valid: true,
	}
	oldLast := []busyResult{{cpuTime: 90, dbTime: 180, ts: oldBusy.ts.Add(-time.Second), valid: true}}
	m := &DBMonitor{
		base:      newBase(dbName, dbHeight, Deps{Cfg: config.FromMap(map[string]any{}), Logger: logger}),
		items:     []string{"db%"},
		methods:   []string{"busy query"},
		osMethods: []string{"nproc"},
		values:    []string{"12.5"},
		busy:      oldBusy,
		lastBusy:  oldLast,
		queryContext: func(context.Context, string) []dbconn.Row {
			return []dbconn.Row{{
				int64(10494930208), "BufHashTableSearch",
				int64(1940789603), int64(1968176646), float64(1), "LWLock",
			}}
		},
	}

	m.Refresh()
	if got := m.MonitorValues(); !reflect.DeepEqual(got, []string{"12.5"}) {
		t.Fatalf("invalid busy result published values=%v", got)
	}
	if m.busy != oldBusy || !reflect.DeepEqual(m.lastBusy, oldLast) {
		t.Fatalf("invalid busy result mutated baselines: busy=%+v last=%+v", m.busy, m.lastBusy)
	}
}
