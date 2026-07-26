package healthdash

import (
	"sync"
	"testing"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
)

type fakeQueryer struct {
	mu       sync.Mutex
	queries  map[string]int
	queryFn  func(string, int) []dbconn.Row
	userDBFn func(string) map[string][]dbconn.Row
	kind     dbcompat.Kind
}

type fakeMemoryGate struct{ allow bool }

func (g *fakeMemoryGate) ShouldRefreshMemory(string) bool { return g.allow }

func (f *fakeQueryer) Query(query string) []dbconn.Row {
	f.mu.Lock()
	if f.queries == nil {
		f.queries = map[string]int{}
	}
	f.queries[query]++
	call := f.queries[query]
	fn := f.queryFn
	f.mu.Unlock()
	if fn == nil {
		return []dbconn.Row{}
	}
	return fn(query, call)
}

func (f *fakeQueryer) ExecuteOnUserDB(query string) map[string][]dbconn.Row {
	if f.userDBFn == nil {
		return map[string][]dbconn.Row{}
	}
	return f.userDBFn(query)
}

func (f *fakeQueryer) Kind() dbcompat.Kind { return f.kind }

func (f *fakeQueryer) count(query string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[query]
}

func TestCollectorEstablishesStartupBaselinesThenPublishesDeltas(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			return []dbconn.Row{{int64(9), int64(10 + call), float64(1_000_000), "select nine"}}
		case activeSQLQuery:
			return []dbconn.Row{}
		case waitQuery:
			return []dbconn.Row{{"DataFileRead", int64(20 + call), int64(2_000 + 100*call), "IO"}}
		case cpuQuery:
			return []dbconn.Row{{int64(1_000 + 50*call)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	first := c.Snapshot()
	if len(first.ExecutionSQL) != 0 || len(first.Waits) != 0 || first.CPU.TimeUSDelta != 0 {
		t.Fatalf("first refresh must establish baselines: %+v", first)
	}

	c.RefreshFast()
	second := c.Snapshot()
	if len(second.ExecutionSQL) != 1 || second.ExecutionSQL[0].CallsDelta != 1 {
		t.Fatalf("execution delta = %+v, want 1", second.ExecutionSQL)
	}
	if len(second.Waits) != 1 || second.Waits[0].TimeUSDelta != 100 || second.CPU.TimeUSDelta != 50 {
		t.Fatalf("wait/cpu delta = waits %+v cpu %+v", second.Waits, second.CPU)
	}
}

func TestCollectorRefreshesMemoryOnlyWhenDue(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery, waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(1)}}
		case activeSQLQuery:
			return []dbconn.Row{{int64(7), int64(70), "700", "select seven", float64(10_000)}}
		case sessionMemoryQuery:
			return []dbconn.Row{{"700", float64(64)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, true)
	c.now = func() time.Time { return now }
	c.lastSlowRefresh = now

	c.RefreshFast()
	c.wg.Wait()
	if fake.count(sessionMemoryQuery) != 1 || len(c.Snapshot().MemorySQL) != 1 {
		t.Fatalf("initial memory refresh missing: calls=%d snapshot=%+v", fake.count(sessionMemoryQuery), c.Snapshot())
	}

	now = now.Add(29 * time.Second)
	c.RefreshFast()
	c.wg.Wait()
	if fake.count(sessionMemoryQuery) != 1 {
		t.Fatalf("memory refreshed before mem_interval: calls=%d", fake.count(sessionMemoryQuery))
	}

	now = now.Add(time.Second)
	c.RefreshFast()
	c.wg.Wait()
	if fake.count(sessionMemoryQuery) != 2 {
		t.Fatalf("memory did not refresh at mem_interval: calls=%d", fake.count(sessionMemoryQuery))
	}
}

func TestCollectorHonorsDynamicMemoryThrottle(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if query == cpuQuery {
			return []dbconn.Row{{int64(1)}}
		}
		return []dbconn.Row{}
	}
	cfg := config.FromMap(map[string]any{
		"main": map[string]any{"dynamic_mem_enable": true, "mem_interval": int64(30), "health_slow_interval": int64(300)},
	})
	c := NewCollector(cfg, fake, logging.New("health-test", ""), &fakeMemoryGate{allow: false})
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.wg.Wait()

	if fake.count(sessionMemoryQuery) != 0 {
		t.Fatalf("memory query calls = %d, want throttle to skip", fake.count(sessionMemoryQuery))
	}
}

func TestCollectorSlowRefreshIsDueAndManualAndKeepsPerDatabaseErrors(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	fake := &fakeQueryer{}
	fake.userDBFn = func(query string) map[string][]dbconn.Row {
		switch query {
		case analyzeHistoryQuery:
			return map[string][]dbconn.Row{
				"app":    {{"public", "orders", now.Add(-time.Hour), nil}},
				"denied": nil,
			}
		case invalidIndexQuery:
			return map[string][]dbconn.Row{
				"app":    {{"public", "orders", "orders_idx", false, true, false}},
				"denied": nil,
			}
		}
		return nil
	}
	c := newTestCollector(fake, false)
	c.now = func() time.Time { return now }

	c.RefreshFast()
	c.wg.Wait()
	first := c.Snapshot()
	if len(first.AnalyzeHistory) != 1 || len(first.InvalidIndexes) != 1 {
		t.Fatalf("slow data = analyze %+v invalid %+v", first.AnalyzeHistory, first.InvalidIndexes)
	}
	if len(first.DatabaseErrors) != 2 {
		t.Fatalf("database errors = %+v, want analyze+index errors for denied", first.DatabaseErrors)
	}

	now = now.Add(60 * time.Second)
	c.RefreshFast()
	c.wg.Wait()
	if !c.Snapshot().SlowRefreshedAt.Equal(first.SlowRefreshedAt) {
		t.Fatal("automatic slow refresh ran before 300 seconds")
	}

	c.RequestSlowRefresh()
	c.wg.Wait()
	if !c.Snapshot().SlowRefreshedAt.Equal(now) {
		t.Fatalf("manual slow refresh time = %v, want %v", c.Snapshot().SlowRefreshedAt, now)
	}
}

func TestCollectorFastRefreshIsSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if query == statementQuery {
			if call == 1 {
				close(entered)
				<-release
			}
			return []dbconn.Row{}
		}
		if query == cpuQuery {
			return []dbconn.Row{{int64(1)}}
		}
		return []dbconn.Row{}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	done := make(chan struct{})
	go func() {
		c.RefreshFast()
		close(done)
	}()
	<-entered
	c.RefreshFast()
	close(release)
	<-done

	if fake.count(statementQuery) != 1 {
		t.Fatalf("statement query calls = %d, want one in-flight refresh", fake.count(statementQuery))
	}
}

func TestCollectorFastQueriesRunConcurrently(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		time.Sleep(80 * time.Millisecond)
		if query == cpuQuery {
			return []dbconn.Row{{int64(1)}}
		}
		return []dbconn.Row{}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	start := time.Now()
	c.RefreshFast()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("fast queries took %v; independent queries must share one timeout window", elapsed)
	}
}

func TestCollectorRebasesResetCountersAndResumesDeltas(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			calls := []int64{10, 2, 3}[call-1]
			return []dbconn.Row{{int64(9), calls, float64(calls * 100), "select nine"}}
		case activeSQLQuery:
			return []dbconn.Row{}
		case waitQuery:
			waits := []int64{10, 2, 3}[call-1]
			return []dbconn.Row{{"DataFileRead", waits, waits * 100, "IO"}}
		case cpuQuery:
			return []dbconn.Row{{[]int64{1_000, 100, 150}[call-1]}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.RefreshFast()
	reset := c.Snapshot()
	if len(reset.ExecutionSQL) != 0 || len(reset.Waits) != 0 || reset.CPU.TimeUSDelta != 0 {
		t.Fatalf("reset cycle must be zero: %+v", reset)
	}

	c.RefreshFast()
	resumed := c.Snapshot()
	if len(resumed.ExecutionSQL) != 1 || resumed.ExecutionSQL[0].CallsDelta != 1 {
		t.Fatalf("execution delta did not resume after reset: %+v", resumed.ExecutionSQL)
	}
	if len(resumed.Waits) != 1 || resumed.Waits[0].TimeUSDelta != 100 || resumed.CPU.TimeUSDelta != 50 {
		t.Fatalf("wait/cpu deltas did not resume: waits=%+v cpu=%+v", resumed.Waits, resumed.CPU)
	}
}

func TestCollectorStatementFailureBlanksRoundAndRecoversFromLastSuccessfulBaseline(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			if call == 3 {
				return nil
			}
			calls := []int64{10, 12, 0, 15}[call-1]
			return []dbconn.Row{{int64(9), calls, float64(calls * 100), "select nine"}}
		case activeSQLQuery, waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(call)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.RefreshFast()
	if got := c.Snapshot().ExecutionSQL; len(got) != 1 || got[0].CallsDelta != 2 {
		t.Fatalf("second sample execution delta = %+v, want 2", got)
	}

	c.RefreshFast()
	failed := c.Snapshot()
	if len(failed.AverageSQL) != 0 || len(failed.ExecutionSQL) != 0 {
		t.Fatalf("failed statement round retained stale rows: avg=%+v execution=%+v", failed.AverageSQL, failed.ExecutionSQL)
	}

	c.RefreshFast()
	recovered := c.Snapshot()
	if len(recovered.ExecutionSQL) != 1 || recovered.ExecutionSQL[0].CallsDelta != 5 {
		t.Fatalf("recovered delta = %+v, want latest 15 minus successful baseline 10", recovered.ExecutionSQL)
	}
}

func TestCollectorWaitFailureBlanksRoundAndRecoversFromLastSuccessfulBaseline(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery, activeSQLQuery:
			return []dbconn.Row{}
		case waitQuery:
			if call == 3 {
				return nil
			}
			value := []int64{100, 120, 0, 150}[call-1]
			return []dbconn.Row{{"DataFileRead", value, value * 10, "IO"}}
		case cpuQuery:
			if call == 3 {
				return nil
			}
			value := []int64{1_000, 1_020, 0, 1_050}[call-1]
			return []dbconn.Row{{value}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.RefreshFast()
	if got := c.Snapshot().Waits; len(got) != 1 || got[0].TimeUSDelta != 200 {
		t.Fatalf("second sample waits = %+v, want 200us", got)
	}

	c.RefreshFast()
	failed := c.Snapshot()
	if len(failed.Waits) != 0 || failed.CPU != (CPUStat{}) {
		t.Fatalf("failed wait round retained stale values: waits=%+v cpu=%+v", failed.Waits, failed.CPU)
	}

	c.RefreshFast()
	recovered := c.Snapshot()
	if len(recovered.Waits) != 1 || recovered.Waits[0].TimeUSDelta != 500 || recovered.CPU.TimeUSDelta != 50 {
		t.Fatalf("recovered values = waits=%+v cpu=%+v", recovered.Waits, recovered.CPU)
	}
}

func TestCollectorMemoryFailureBlanksRowsUntilNextSuccessfulRefresh(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery, waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(1)}}
		case activeSQLQuery:
			return []dbconn.Row{{int64(7), int64(70), "700", "select seven", float64(10_000)}}
		case sessionMemoryQuery:
			if call == 2 {
				return nil
			}
			return []dbconn.Row{{"700", float64(64 + call)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, true)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.wg.Wait()
	if len(c.Snapshot().MemorySQL) != 1 {
		t.Fatal("initial memory sample was not published")
	}
	c.lastMemoryRefresh = time.Time{}
	c.RefreshFast()
	c.wg.Wait()
	if got := c.Snapshot().MemorySQL; len(got) != 0 {
		t.Fatalf("failed memory round retained stale rows: %+v", got)
	}
	c.lastMemoryRefresh = time.Time{}
	c.RefreshFast()
	c.wg.Wait()
	if got := c.Snapshot().MemorySQL; len(got) != 1 || got[0].TotalMemoryMB != 67 {
		t.Fatalf("memory refresh did not recover: %+v", got)
	}
}

func TestCollectorKeepsLastSuccessfulDatabaseRowsWhenOneDatabaseFails(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.Local)
	analyzeCall := 0
	fake := &fakeQueryer{}
	fake.userDBFn = func(query string) map[string][]dbconn.Row {
		if query == invalidIndexQuery {
			return map[string][]dbconn.Row{}
		}
		analyzeCall++
		if analyzeCall == 1 {
			return map[string][]dbconn.Row{
				"app":     {{"public", "orders", now.Add(-time.Hour), nil}},
				"archive": {{"public", "old_orders", now.Add(-2 * time.Hour), nil}},
			}
		}
		return map[string][]dbconn.Row{
			"app":     {{"public", "orders", now, nil}},
			"archive": nil,
		}
	}
	c := newTestCollector(fake, false)
	c.now = func() time.Time { return now }

	c.RequestSlowRefresh()
	c.wg.Wait()
	now = now.Add(time.Minute)
	c.RequestSlowRefresh()
	c.wg.Wait()
	snapshot := c.Snapshot()

	foundArchive := false
	for _, row := range snapshot.AnalyzeHistory {
		if row.Database == "archive" && row.Table == "old_orders" {
			foundArchive = true
		}
	}
	if !foundArchive {
		t.Fatalf("last successful archive row was discarded: %+v", snapshot.AnalyzeHistory)
	}
	if len(snapshot.DatabaseErrors) != 1 || snapshot.DatabaseErrors[0].Database != "archive" {
		t.Fatalf("database errors = %+v", snapshot.DatabaseErrors)
	}
}

func TestCollectorStopWaitsForInFlightFastRefresh(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if query == statementQuery {
			close(entered)
			<-release
		}
		if query == cpuQuery {
			return []dbconn.Row{{int64(1)}}
		}
		return []dbconn.Row{}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()
	go c.RefreshFast()
	<-entered

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while fast refresh was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after fast refresh completed")
	}
}

func newTestCollector(q Queryer, dynamicMemory bool) *Collector {
	cfg := config.FromMap(map[string]any{
		"main": map[string]any{
			"dynamic_mem_enable":   dynamicMemory,
			"mem_interval":         int64(30),
			"health_slow_interval": int64(300),
		},
	})
	return NewCollector(cfg, q, logging.New("health-test", ""))
}
