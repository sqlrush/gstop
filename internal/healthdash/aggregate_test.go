package healthdash

import (
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestBuildSQLMetricsIncludesRunningExecutionsInAverage(t *testing.T) {
	current := []StatementSample{{SQLID: 42, Calls: 100, DBTimeUS: 1_000_000, Query: "select 42"}}
	baseline := []StatementSample{{SQLID: 42, Calls: 90, DBTimeUS: 900_000, Query: "select 42"}}
	active := []ActiveSQL{{SQLID: 42, PID: 10, ElapsedUS: 500_000, Query: "select 42"}}

	average, executions := BuildSQLMetrics(current, baseline, active, time.Unix(1, 0))

	if len(average) != 1 {
		t.Fatalf("average rows = %d, want 1", len(average))
	}
	wantAverage := 600_000.0 / 11.0
	if math.Abs(average[0].AverageUS-wantAverage) > 0.001 {
		t.Fatalf("average = %f, want %f", average[0].AverageUS, wantAverage)
	}
	if average[0].Calls != 10 || average[0].ActiveSessions != 1 {
		t.Fatalf("average metadata = %+v", average[0])
	}
	if len(executions) != 1 || executions[0].CallsDelta != 10 {
		t.Fatalf("execution delta = %+v, want 10", executions)
	}
}

func TestBuildActiveElapsedMetricsGroupsBySQLIDAndUsesLongestSession(t *testing.T) {
	capturedAt := time.Unix(100, 0)
	queryStart := capturedAt.Add(-9 * time.Second)
	got := BuildActiveElapsedMetrics([]ActiveSQL{
		{SQLID: 42, PID: 1, SessionID: "s1", Query: "select 42", Database: "db1", User: "u1", ElapsedUS: 2_000_000},
		{SQLID: 42, PID: 2, SessionID: "s2", Query: "select 42", Database: "db2", User: "u2", ElapsedUS: 9_000_000, QueryStart: queryStart},
		{SQLID: 7, PID: 3, SessionID: "s3", Query: "select 7", ElapsedUS: 5_000_000},
		{SQLID: 0, PID: 4, SessionID: "ignored", ElapsedUS: 99_000_000},
	}, capturedAt)
	if len(got) != 2 || got[0].SQLID != 42 || got[0].ActiveSessions != 2 ||
		got[0].RepresentativePID != 2 || got[0].RepresentativeSessionID != "s2" ||
		got[0].RepresentativeElapsedUS != 9_000_000 ||
		!got[0].RepresentativeQueryStart.Equal(queryStart) || !got[0].CapturedAt.Equal(capturedAt) {
		t.Fatalf("active elapsed metrics=%+v", got)
	}
}

func TestBuildActiveElapsedMetricsIgnoresRowsWithoutSessionIdentity(t *testing.T) {
	got := BuildActiveElapsedMetrics([]ActiveSQL{
		{SQLID: 42, Query: "select malformed", ElapsedUS: 99_000_000},
		{SQLID: 7, PID: 7, Query: "select 7", ElapsedUS: 1_000_000},
	}, time.Unix(100, 0))

	if len(got) != 1 || got[0].SQLID != 7 || got[0].ActiveSessions != 1 {
		t.Fatalf("unidentifiable active row was published: %+v", got)
	}
}

func TestBuildActiveElapsedMetricsDeduplicatesParallelThreadsBySessionID(t *testing.T) {
	got := BuildActiveElapsedMetrics([]ActiveSQL{
		{SQLID: 42, PID: 101, SessionID: "session-1", Query: "select 42", ElapsedUS: 0},
		{SQLID: 42, PID: 102, SessionID: "session-1", Query: "select 42", ElapsedUS: 0},
		{SQLID: 42, PID: 103, SessionID: "session-1", Query: "select 42", ElapsedUS: 9_000_000},
		{SQLID: 42, PID: 201, SessionID: "session-2", Query: "select 42", ElapsedUS: 8_000_000},
	}, time.Unix(100, 0))

	if len(got) != 1 || got[0].ActiveSessions != 2 ||
		got[0].RepresentativePID != 103 || got[0].RepresentativeSessionID != "session-1" ||
		got[0].RepresentativeElapsedUS != 9_000_000 {
		t.Fatalf("parallel worker rows were not collapsed by logical session: %+v", got)
	}
}

func TestBuildActiveElapsedMetricsFallsBackToPIDForZeroSessionID(t *testing.T) {
	got := BuildActiveElapsedMetrics([]ActiveSQL{
		{SQLID: 42, PID: 101, SessionID: "0", Query: "select 42", ElapsedUS: 2_000_000},
		{SQLID: 42, PID: 102, SessionID: "0", Query: "select 42", ElapsedUS: 3_000_000},
	}, time.Unix(100, 0))

	if len(got) != 1 || got[0].ActiveSessions != 2 || got[0].RepresentativePID != 102 {
		t.Fatalf("zero session ID did not fall back to PID identity: %+v", got)
	}
}

func TestBuildActiveElapsedMetricsLimitsFiveAndBreaksTiesBySQLID(t *testing.T) {
	active := make([]ActiveSQL, 0, 6)
	for id := int64(6); id >= 1; id-- {
		active = append(active, ActiveSQL{SQLID: id, PID: id, ElapsedUS: 1_000_000})
	}
	got := BuildActiveElapsedMetrics(active, time.Unix(1, 0))
	if len(got) != 5 || got[0].SQLID != 1 || got[4].SQLID != 5 {
		t.Fatalf("stable top5=%+v", got)
	}
}

func TestBuildSQLMetricsMergesDatabaseAndUsersWithoutSplittingSQLID(t *testing.T) {
	current := []StatementSample{
		{
			SQLID: 7, Calls: 2, DBTimeUS: 20, Query: "select 7",
			Databases: []string{"postgres"}, Users: []string{"alice"},
		},
		{
			SQLID: 7, Calls: 3, DBTimeUS: 45, Query: "select 7",
			Databases: []string{"postgres"}, Users: []string{"bob"},
		},
	}
	active := []ActiveSQL{{
		SQLID: 7, PID: 70, SessionID: "s70", Query: "select 7",
		Database: "sales", User: "carol",
	}}

	average, executions := BuildSQLMetrics(current, nil, active, time.Unix(1, 0))

	for _, rows := range [][]SQLMetric{average, executions} {
		if len(rows) != 1 {
			t.Fatalf("rows=%+v, want one SQL_ID aggregate", rows)
		}
		got := rows[0]
		if got.Calls != 5 || got.CallsDelta != 5 {
			t.Fatalf("calls=%+v, want 5", got)
		}
		if !reflect.DeepEqual(got.Databases, []string{"postgres", "sales"}) {
			t.Fatalf("databases=%v", got.Databases)
		}
		if !reflect.DeepEqual(got.Users, []string{"alice", "bob", "carol"}) {
			t.Fatalf("users=%v", got.Users)
		}
	}
}

func TestBuildSQLMetricsExcludesInactiveSQLWithoutStartupDelta(t *testing.T) {
	current := []StatementSample{{
		SQLID: 1443482919, Calls: 100, DBTimeUS: 366_673_346,
		Query: "select old cumulative sql",
	}}
	baseline := append([]StatementSample(nil), current...)

	average, executions := BuildSQLMetrics(
		current, baseline, nil, time.Unix(1, 0),
	)

	if len(average) != 0 || len(executions) != 0 {
		t.Fatalf("stale SQL leaked into rankings: avg=%+v exec=%+v",
			average, executions)
	}
}

func TestBuildSQLMetricsUsesZeroBaselineForNewSQL(t *testing.T) {
	current := []StatementSample{{
		SQLID: 8, Calls: 3, DBTimeUS: 900, Query: "select new",
	}}
	average, executions := BuildSQLMetrics(
		current, nil, nil, time.Unix(1, 0),
	)
	if len(average) != 1 || average[0].Calls != 3 ||
		average[0].AverageUS != 300 {
		t.Fatalf("new SQL average = %+v", average)
	}
	if len(executions) != 1 || executions[0].CallsDelta != 3 {
		t.Fatalf("new SQL executions = %+v", executions)
	}
}

func TestBuildSQLMetricsPreservesFullElapsedForPreexistingActiveSQL(t *testing.T) {
	active := []ActiveSQL{{
		SQLID: 9, PID: 90, SessionID: "session-90",
		ElapsedUS: 125_000_000, Query: "select long running",
	}}
	average, _ := BuildSQLMetrics(
		nil, nil, active, time.Unix(1, 0),
	)
	if len(average) != 1 || average[0].AverageUS != 125_000_000 ||
		average[0].ActiveSessions != 1 {
		t.Fatalf("preexisting active SQL = %+v", average)
	}
}

func TestBuildSQLMetricsTreatsDBTimeRegressionAsZeroDelta(t *testing.T) {
	current := []StatementSample{{
		SQLID: 10, Calls: 10, DBTimeUS: 100, Query: "select reset",
	}}
	baseline := []StatementSample{{
		SQLID: 10, Calls: 9, DBTimeUS: 900, Query: "select reset",
	}}
	average, executions := BuildSQLMetrics(
		current, baseline, nil, time.Unix(1, 0),
	)
	if len(average) != 1 || average[0].Calls != 1 ||
		average[0].AverageUS != 0 {
		t.Fatalf("DB-time regression result = %+v", average)
	}
	if len(executions) != 1 || executions[0].CallsDelta != 1 {
		t.Fatalf("DB-time regression executions = %+v", executions)
	}
}

func TestBuildSQLMetricsHandlesNewAndResetCounters(t *testing.T) {
	current := []StatementSample{
		{SQLID: 1, Calls: 3, DBTimeUS: 300, Query: "new"},
		{SQLID: 2, Calls: 2, DBTimeUS: 200, Query: "reset"},
	}
	baseline := []StatementSample{{SQLID: 2, Calls: 9, DBTimeUS: 900, Query: "reset"}}

	_, executions := BuildSQLMetrics(current, baseline, nil, time.Unix(1, 0))

	if len(executions) != 1 {
		t.Fatalf("execution rows = %+v, want only the new SQL", executions)
	}
	if executions[0].SQLID != 1 || executions[0].CallsDelta != 3 {
		t.Fatalf("execution row = %+v, want SQL 1 delta 3", executions[0])
	}
}

func TestBuildSQLMetricsReturnsTopFive(t *testing.T) {
	current := make([]StatementSample, 6)
	for i := range current {
		rank := i + 1
		current[i] = StatementSample{
			SQLID: int64(rank), Calls: int64(rank),
			DBTimeUS: float64(rank * rank * 10), Query: fmt.Sprintf("select %d", rank),
		}
	}

	average, executions := BuildSQLMetrics(current, nil, nil, time.Unix(1, 0))

	if len(average) != 5 || average[0].SQLID != 6 || average[4].SQLID != 2 {
		t.Fatalf("average top5 = %+v", average)
	}
	if len(executions) != 5 || executions[0].SQLID != 6 || executions[4].SQLID != 2 {
		t.Fatalf("execution top5 = %+v", executions)
	}
}

func TestBuildSQLMetricsIncludesSQLThatHasOnlyARunningExecution(t *testing.T) {
	average, _ := BuildSQLMetrics(nil, nil, []ActiveSQL{{
		SQLID: 77, PID: 700, ElapsedUS: 250_000, Query: "select first execution",
	}}, time.Unix(1, 0))

	if len(average) != 1 || average[0].SQLID != 77 || average[0].AverageUS != 250_000 || average[0].ActiveSessions != 1 {
		t.Fatalf("active-only average = %+v", average)
	}
}

func TestBuildMemoryMetricsAggregatesActiveSessions(t *testing.T) {
	active := []ActiveSQL{
		{SQLID: 7, PID: 1, MemoryMB: 12.5, Query: "select seven"},
		{SQLID: 7, PID: 2, MemoryMB: 5.5, Query: "select seven"},
		{SQLID: 8, PID: 3, MemoryMB: 20, Query: "select eight"},
		{SQLID: 0, PID: 4, MemoryMB: 999, Query: "ignored"},
	}

	got := BuildMemoryMetrics(active, time.Unix(1, 0))

	if len(got) != 2 || got[0].SQLID != 8 || got[1].SQLID != 7 {
		t.Fatalf("memory rows = %+v", got)
	}
	seven := got[1]
	if seven.ActiveSessions != 2 || seven.TotalMemoryMB != 18 || seven.MaxMemoryMB != 12.5 {
		t.Fatalf("SQL 7 aggregate = %+v", seven)
	}
}

func TestBuildMemoryMetricsReturnsTopFive(t *testing.T) {
	active := make([]ActiveSQL, 6)
	for i := range active {
		active[i] = ActiveSQL{
			SQLID: int64(i + 1), PID: int64(i + 10),
			MemoryMB: float64(i + 1), Query: fmt.Sprintf("select %d", i+1),
		}
	}

	got := BuildMemoryMetrics(active, time.Unix(1, 0))

	if len(got) != 5 || got[0].SQLID != 6 || got[4].SQLID != 2 {
		t.Fatalf("memory top5 = %+v", got)
	}
}

func TestBuildSQLMetricsCarriesLongestActiveSessionIdentity(t *testing.T) {
	capturedAt := time.Date(2026, 7, 24, 10, 0, 0, 0, time.Local)
	active := []ActiveSQL{
		{SQLID: 77, PID: 701, SessionID: "s-701", Query: "select 77", ElapsedUS: 2_000},
		{SQLID: 77, PID: 702, SessionID: "s-702", Query: "select 77", ElapsedUS: 9_000},
	}
	average, executions := BuildSQLMetrics(
		[]StatementSample{{SQLID: 77, Calls: 3, DBTimeUS: 30_000, Query: "select 77"}},
		nil, active, capturedAt,
	)
	for _, rows := range [][]SQLMetric{average, executions} {
		if len(rows) != 1 {
			t.Fatalf("rows=%+v", rows)
		}
		got := rows[0]
		if got.RepresentativePID != 702 || got.RepresentativeSessionID != "s-702" ||
			got.RepresentativeElapsedUS != 9_000 || !got.CapturedAt.Equal(capturedAt) {
			t.Fatalf("representative identity=%+v", got)
		}
	}
}

func TestBuildSQLMetricsRepresentativeTieBreakIsDeterministic(t *testing.T) {
	active := []ActiveSQL{
		{SQLID: 88, PID: 900, SessionID: "session-b", ElapsedUS: 5_000},
		{SQLID: 88, PID: 802, SessionID: "session-a", ElapsedUS: 5_000},
		{SQLID: 88, PID: 801, SessionID: "session-a", ElapsedUS: 5_000},
	}
	average, _ := BuildSQLMetrics(nil, nil, active, time.Unix(1, 0))
	if len(average) != 1 || average[0].RepresentativeSessionID != "session-a" ||
		average[0].RepresentativePID != 801 {
		t.Fatalf("representative identity=%+v", average)
	}
}

func TestBuildSQLMetricsCompletedOnlyRowHasNoRepresentativeIdentity(t *testing.T) {
	average, _ := BuildSQLMetrics(
		[]StatementSample{{SQLID: 99, Calls: 1, DBTimeUS: 10, Query: "select 99"}},
		nil, nil, time.Unix(2, 0),
	)
	if average[0].RepresentativePID != 0 || average[0].RepresentativeSessionID != "" ||
		!average[0].CapturedAt.IsZero() {
		t.Fatalf("completed-only metric=%+v", average[0])
	}
}

func TestBuildMemoryMetricsCarriesRepresentativeIdentity(t *testing.T) {
	at := time.Unix(3, 0)
	rows := BuildMemoryMetrics([]ActiveSQL{
		{SQLID: 12, PID: 1201, SessionID: "a", ElapsedUS: 10, MemoryMB: 100},
		{SQLID: 12, PID: 1202, SessionID: "b", ElapsedUS: 20, MemoryMB: 50},
	}, at)
	if len(rows) != 1 || rows[0].RepresentativePID != 1202 ||
		rows[0].RepresentativeSessionID != "b" || !rows[0].CapturedAt.Equal(at) {
		t.Fatalf("memory metric=%+v", rows)
	}
}

func TestBuildWaitMetricsRanksWaitsAndReportsCPUSeparately(t *testing.T) {
	baseline := []WaitSample{
		{Event: "A", Waits: 10, TimeUS: 100, Type: "IO"},
		{Event: "B", Waits: 10, TimeUS: 100, Type: "LOCK"},
	}
	current := []WaitSample{
		{Event: "A", Waits: 12, TimeUS: 300, Type: "IO"},
		{Event: "B", Waits: 11, TimeUS: 400, Type: "LOCK"},
		{Event: "C", Waits: 4, TimeUS: 100, Type: "NETWORK"},
	}

	waits, cpu := BuildWaitMetrics(current, baseline, 1_500, 1_000)

	if len(waits) != 3 || waits[0].Event != "B" || waits[1].Event != "A" || waits[2].Event != "C" {
		t.Fatalf("wait ranking = %+v", waits)
	}
	if waits[0].WaitsDelta != 1 || waits[0].TimeUSDelta != 300 || waits[0].AverageUS != 300 {
		t.Fatalf("B metric = %+v", waits[0])
	}
	if cpu.TimeUSDelta != 500 {
		t.Fatalf("cpu delta = %d, want 500", cpu.TimeUSDelta)
	}
	wantCPUShare := 500.0 / 1_100.0
	if math.Abs(cpu.Share-wantCPUShare) > 0.000001 {
		t.Fatalf("cpu share = %f, want %f", cpu.Share, wantCPUShare)
	}
}

func TestBuildWaitMetricsClampsResetCountersAndReturnsTopFive(t *testing.T) {
	baseline := []WaitSample{{Event: "reset", Waits: 9, TimeUS: 90}}
	current := []WaitSample{{Event: "reset", Waits: 1, TimeUS: 10}}
	for i := int64(1); i <= 6; i++ {
		current = append(current, WaitSample{Event: string(rune('a' + i)), Waits: i, TimeUS: i * 100})
	}

	waits, cpu := BuildWaitMetrics(current, baseline, 100, 200)

	if len(waits) != 5 {
		t.Fatalf("wait rows = %d, want 5", len(waits))
	}
	for _, row := range waits {
		if row.Event == "reset" {
			t.Fatalf("reset counter should have zero delta and be omitted: %+v", waits)
		}
	}
	if cpu.TimeUSDelta != 0 || cpu.Share != 0 {
		t.Fatalf("reset CPU = %+v, want zero", cpu)
	}
}
