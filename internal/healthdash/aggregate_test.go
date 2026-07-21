package healthdash

import (
	"math"
	"testing"
)

func TestBuildSQLMetricsIncludesRunningExecutionsInAverage(t *testing.T) {
	current := []StatementSample{{SQLID: 42, Calls: 100, DBTimeUS: 1_000_000, Query: "select 42"}}
	baseline := []StatementSample{{SQLID: 42, Calls: 90, DBTimeUS: 900_000, Query: "select 42"}}
	active := []ActiveSQL{{SQLID: 42, PID: 10, ElapsedUS: 500_000, Query: "select 42"}}

	average, executions := BuildSQLMetrics(current, baseline, active)

	if len(average) != 1 {
		t.Fatalf("average rows = %d, want 1", len(average))
	}
	wantAverage := 1_500_000.0 / 101.0
	if math.Abs(average[0].AverageUS-wantAverage) > 0.001 {
		t.Fatalf("average = %f, want %f", average[0].AverageUS, wantAverage)
	}
	if average[0].ActiveSessions != 1 || average[0].Calls != 100 {
		t.Fatalf("average metadata = %+v", average[0])
	}
	if len(executions) != 1 || executions[0].CallsDelta != 10 {
		t.Fatalf("execution delta = %+v, want 10", executions)
	}
}

func TestBuildSQLMetricsHandlesNewAndResetCounters(t *testing.T) {
	current := []StatementSample{
		{SQLID: 1, Calls: 3, DBTimeUS: 300, Query: "new"},
		{SQLID: 2, Calls: 2, DBTimeUS: 200, Query: "reset"},
	}
	baseline := []StatementSample{{SQLID: 2, Calls: 9, DBTimeUS: 900, Query: "reset"}}

	_, executions := BuildSQLMetrics(current, baseline, nil)

	if len(executions) != 1 {
		t.Fatalf("execution rows = %+v, want only the new SQL", executions)
	}
	if executions[0].SQLID != 1 || executions[0].CallsDelta != 3 {
		t.Fatalf("execution row = %+v, want SQL 1 delta 3", executions[0])
	}
}

func TestBuildSQLMetricsReturnsTopThree(t *testing.T) {
	current := []StatementSample{
		{SQLID: 1, Calls: 1, DBTimeUS: 10},
		{SQLID: 2, Calls: 2, DBTimeUS: 40},
		{SQLID: 3, Calls: 3, DBTimeUS: 90},
		{SQLID: 4, Calls: 4, DBTimeUS: 160},
	}

	average, executions := BuildSQLMetrics(current, nil, nil)

	if len(average) != 3 || average[0].SQLID != 4 || average[1].SQLID != 3 || average[2].SQLID != 2 {
		t.Fatalf("average top3 = %+v", average)
	}
	if len(executions) != 3 || executions[0].SQLID != 4 || executions[1].SQLID != 3 || executions[2].SQLID != 2 {
		t.Fatalf("execution top3 = %+v", executions)
	}
}

func TestBuildSQLMetricsIncludesSQLThatHasOnlyARunningExecution(t *testing.T) {
	average, _ := BuildSQLMetrics(nil, nil, []ActiveSQL{{
		SQLID: 77, PID: 700, ElapsedUS: 250_000, Query: "select first execution",
	}})

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

	got := BuildMemoryMetrics(active)

	if len(got) != 2 || got[0].SQLID != 8 || got[1].SQLID != 7 {
		t.Fatalf("memory rows = %+v", got)
	}
	seven := got[1]
	if seven.ActiveSessions != 2 || seven.TotalMemoryMB != 18 || seven.MaxMemoryMB != 12.5 {
		t.Fatalf("SQL 7 aggregate = %+v", seven)
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
