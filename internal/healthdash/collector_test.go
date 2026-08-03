package healthdash

import (
	"context"
	"strings"
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

func TestActiveSQLQueryUsesTotalElapsedTimeAcrossDays(t *testing.T) {
	normalized := strings.ToUpper(activeSQLQuery)
	if !strings.Contains(normalized, "EXTRACT(EPOCH FROM") ||
		!strings.Contains(normalized, "STATEMENT_TIMESTAMP()") {
		t.Fatalf("active SQL elapsed expression is not based on one total-time clock: %s", activeSQLQuery)
	}
	if strings.Contains(normalized, "EXTRACT(HOUR FROM") {
		t.Fatalf("active SQL elapsed expression wraps after 24 hours: %s", activeSQLQuery)
	}
}

type fakeCommandRunner struct {
	mu      sync.Mutex
	outputs []string
	calls   int
}

func (r *fakeCommandRunner) Run(command string, check bool) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if command != clusterCMCommand || check {
		return "", false
	}
	if r.calls > len(r.outputs) || r.outputs[r.calls-1] == "" {
		return "", false
	}
	return r.outputs[r.calls-1], true
}

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

func (f *fakeQueryer) QueryContext(ctx context.Context, query string) []dbconn.Row {
	if ctx.Err() != nil {
		return nil
	}
	return f.Query(query)
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

func TestParseStatementsAggregatesDatabaseAndUsersBySQLID(t *testing.T) {
	got, ok := parseStatements([]dbconn.Row{
		{int64(7), "postgres", "alice", int64(2), float64(20), "select 7"},
		{int64(7), "postgres", "bob", int64(3), float64(45), "select 7"},
	})
	if !ok || len(got) != 1 {
		t.Fatalf("parseStatements=%+v ok=%t", got, ok)
	}
	if got[0].Calls != 5 || got[0].DBTimeUS != 65 ||
		len(got[0].Databases) != 1 || got[0].Databases[0] != "postgres" ||
		len(got[0].Users) != 2 || got[0].Users[0] != "alice" || got[0].Users[1] != "bob" {
		t.Fatalf("statement aggregate=%+v", got[0])
	}
}

func TestParseActiveReadsDatabaseAndUser(t *testing.T) {
	queryStart := time.Date(2026, 8, 3, 22, 0, 0, 123000000, time.FixedZone("CST", 8*60*60))
	got, ok := parseActive([]dbconn.Row{{
		int64(7), int64(70), "s70", "select 7", float64(123),
		"sales", "carol", queryStart,
	}})
	if !ok || len(got) != 1 {
		t.Fatalf("parseActive=%+v ok=%t", got, ok)
	}
	if got[0].Database != "sales" || got[0].User != "carol" ||
		!got[0].QueryStart.Equal(queryStart) {
		t.Fatalf("active identity=%+v", got[0])
	}
}

func TestCollectorEstablishesStartupBaselinesThenPublishesDeltas(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.Local)
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			return []dbconn.Row{{int64(9), int64(10 + call), float64(1_000_000), "select nine"}}
		case activeSQLQuery:
			return []dbconn.Row{{int64(9), int64(90), "session-90", "select nine", float64(5_000)}}
		case waitQuery:
			return []dbconn.Row{{"DataFileRead", int64(20 + call), int64(2_000 + 100*call), "IO"}}
		case cpuQuery:
			return []dbconn.Row{{int64(1_000 + 50*call)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.now = func() time.Time { return now }
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
	if !second.AverageSQL[0].CapturedAt.Equal(now) || !second.ExecutionSQL[0].CapturedAt.Equal(now) {
		t.Fatalf("SQL metric capture times = average %+v execution %+v, want %v", second.AverageSQL, second.ExecutionSQL, now)
	}
	if len(second.Waits) != 1 || second.Waits[0].TimeUSDelta != 100 || second.CPU.TimeUSDelta != 50 {
		t.Fatalf("wait/cpu delta = waits %+v cpu %+v", second.Waits, second.CPU)
	}
}

func TestCollectorFirstSamplePublishesOnlyActiveSQLAverage(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, _ int) []dbconn.Row {
		switch query {
		case statementQuery:
			return []dbconn.Row{{
				int64(7), int64(50), float64(5_000_000), "select old",
			}}
		case activeSQLQuery:
			return []dbconn.Row{{
				int64(8), int64(808), "session-808",
				"select running", float64(90_000_000),
			}}
		case waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(1_000)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	got := c.Snapshot()

	if len(got.AverageSQL) != 1 || got.AverageSQL[0].SQLID != 8 ||
		got.AverageSQL[0].AverageUS != 90_000_000 {
		t.Fatalf("first average = %+v", got.AverageSQL)
	}
	if len(got.ExecutionSQL) != 0 {
		t.Fatalf("first executions = %+v", got.ExecutionSQL)
	}
}

func TestCollectorPublishesActiveElapsedWhenStatementQueryFails(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, _ int) []dbconn.Row {
		switch query {
		case statementQuery:
			return nil
		case activeSQLQuery:
			return []dbconn.Row{{int64(42), int64(420), "session-420", "select active", float64(90_000_000)}}
		case waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(1_000)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	snapshot := c.Snapshot()
	if len(snapshot.ActiveElapsedSQL) != 1 || snapshot.ActiveElapsedSQL[0].SQLID != 42 {
		t.Fatalf("active SQL was hidden by statement failure: %+v", snapshot)
	}
	if len(snapshot.AverageSQL) != 0 {
		t.Fatalf("failed statement sample retained average rows: %+v", snapshot.AverageSQL)
	}
}

func TestCollectorClearsActiveElapsedWhenActiveQueryFails(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery, waitQuery:
			return []dbconn.Row{}
		case activeSQLQuery:
			if call == 1 {
				return []dbconn.Row{{int64(42), int64(420), "session-420", "select active", float64(90_000_000)}}
			}
			return nil
		case cpuQuery:
			return []dbconn.Row{{int64(1_000)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	if got := c.Snapshot().ActiveElapsedSQL; len(got) != 1 || got[0].SQLID != 42 {
		t.Fatalf("first active SQL sample = %+v", got)
	}

	c.RefreshFast()
	snapshot := c.Snapshot()
	if len(snapshot.ActiveElapsedSQL) != 0 {
		t.Fatalf("failed active sample retained elapsed rows: %+v", snapshot.ActiveElapsedSQL)
	}
	if !strings.Contains(snapshot.FastError, "活跃SQL查询失败") {
		t.Fatalf("failed active sample missing fast error: %+v", snapshot)
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
	c := NewCollector(cfg, fake, logging.New("health-test", ""), &fakeMemoryGate{allow: false}, nil)
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

func TestCollectorRefreshesClusterSourcesAndClearsFailedRound(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.Local)
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		if query != clusterTopologyQuery {
			if query == cpuQuery {
				return []dbconn.Row{{int64(1)}}
			}
			return []dbconn.Row{}
		}
		if call == 2 {
			return nil
		}
		return []dbconn.Row{{"cn_5001", "C", "10.0.0.1", int64(5001), "10.0.0.2", int64(5001), true, true, true, true, true, int64(1)}}
	}
	fake.userDBFn = func(query string) map[string][]dbconn.Row {
		if query == analyzeHistoryQuery {
			return map[string][]dbconn.Row{"app": {{"public", "orders", now, nil}}}
		}
		return map[string][]dbconn.Row{}
	}
	runner := &fakeCommandRunner{outputs: []string{
		"[ CMServer State ]\nnode instance state\n1 cm1 1 Primary",
		"",
	}}
	cfg := config.FromMap(map[string]any{"main": map[string]any{
		"dynamic_mem_enable": false, "mem_interval": int64(30), "health_slow_interval": int64(300),
	}})
	c := NewCollector(cfg, fake, logging.New("health-test", ""), nil, runner)
	c.now = func() time.Time { return now }

	c.RequestSlowRefresh()
	c.wg.Wait()
	first := c.Snapshot()
	if !first.Cluster.SQLAvailable || !first.Cluster.CMAvailable || len(first.Cluster.Nodes) != 1 || len(first.Cluster.Components) != 1 || len(first.AnalyzeHistory) != 1 {
		t.Fatalf("first slow sample=%+v", first)
	}

	now = now.Add(time.Minute)
	c.RequestSlowRefresh()
	c.wg.Wait()
	second := c.Snapshot()
	if second.Cluster.SQLAvailable || second.Cluster.CMAvailable || len(second.Cluster.Nodes) != 0 || len(second.Cluster.Components) != 0 {
		t.Fatalf("failed current cluster round retained stale data: %+v", second.Cluster)
	}
	if len(second.AnalyzeHistory) != 1 {
		t.Fatalf("cluster failure affected analyze data: %+v", second.AnalyzeHistory)
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

func TestCollectorPublishesLockAndReplicationClearsAndRecovers(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery, activeSQLQuery, waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(call)}}
		case lockHealthQuery:
			if call == 2 {
				return nil
			}
			return []dbconn.Row{{int64(11), "11.1", int64(21), "21.1", "relation", "ExclusiveLock", "tag", int64(101), "update t", float64(call * 1000)}}
		case replicationQuery:
			if call == 2 {
				return nil
			}
			return []dbconn.Row{{"LOCAL", "Primary", "", "", "", "", "", "", "", "", "", "0", "", "0"}}
		case replicationFallbackQuery:
			return nil
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	first := c.Snapshot()
	if len(first.Lock.Chains) != 1 || first.LockError != "" || first.Replication.LocalRole != "Primary" || first.ReplicationError != "" {
		t.Fatalf("first sample: %+v", first)
	}

	c.RefreshFast()
	failed := c.Snapshot()
	if len(failed.Lock.Chains) != 0 || failed.LockError == "" || len(failed.Replication.Channels) != 0 || failed.Replication.LocalRole != "" || failed.ReplicationError == "" {
		t.Fatalf("failed sample retained data or omitted error: %+v", failed)
	}

	c.RefreshFast()
	recovered := c.Snapshot()
	if len(recovered.Lock.Chains) != 1 || recovered.Lock.Chains[0].ElapsedUS != 3000 || recovered.LockError != "" || recovered.Replication.LocalRole != "Primary" || recovered.ReplicationError != "" {
		t.Fatalf("recovered sample: %+v", recovered)
	}
}

func TestCollectorReplicationUsesFallback(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case cpuQuery:
			return []dbconn.Row{{int64(1)}}
		case replicationQuery:
			return nil
		case replicationFallbackQuery:
			return []dbconn.Row{{"LOCAL", "Standby", "", "", "", "", "", "", "", "", "", "0", "", "0"}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()

	if got := c.Snapshot(); got.Replication.LocalRole != "Standby" || got.ReplicationError != "" || fake.count(replicationFallbackQuery) != 1 {
		t.Fatalf("fallback snapshot=%+v calls=%d", got, fake.count(replicationFallbackQuery))
	}
}

func TestSnapshotCloneDeepCopiesLockAndReplication(t *testing.T) {
	original := Snapshot{
		Lock:        LockHealth{Chains: []LockChain{{WaiterPID: 11}}},
		Replication: ReplicationHealth{Channels: []ReplicationChannel{{Direction: "SENDER"}}},
		ActiveElapsedSQL: []SQLMetric{{
			Databases: []string{"db1"},
			Users:     []string{"user1"},
		}},
	}
	cloned := original.Clone()
	cloned.Lock.Chains[0].WaiterPID = 99
	cloned.Replication.Channels[0].Direction = "RECEIVER"
	cloned.ActiveElapsedSQL[0].Databases[0] = "db2"
	cloned.ActiveElapsedSQL[0].Users[0] = "user2"

	if original.Lock.Chains[0].WaiterPID != 11 || original.Replication.Channels[0].Direction != "SENDER" ||
		original.ActiveElapsedSQL[0].Databases[0] != "db1" || original.ActiveElapsedSQL[0].Users[0] != "user1" {
		t.Fatalf("clone mutated original: %+v", original)
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

func TestCollectorRebasesWhenStatementDBTimeRegresses(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			calls := []int64{10, 11, 12}[call-1]
			dbTimes := []float64{1_000, 100, 250}
			return []dbconn.Row{{
				int64(9), calls, dbTimes[call-1], "select nine",
			}}
		case activeSQLQuery, waitQuery:
			return []dbconn.Row{}
		case cpuQuery:
			return []dbconn.Row{{int64(1_000 + call)}}
		default:
			return []dbconn.Row{}
		}
	}
	c := newTestCollector(fake, false)
	c.lastSlowRefresh = c.now()

	c.RefreshFast()
	c.RefreshFast()
	reset := c.Snapshot()
	if len(reset.AverageSQL) != 0 || len(reset.ExecutionSQL) != 0 {
		t.Fatalf("DB-time reset cycle = avg=%+v exec=%+v",
			reset.AverageSQL, reset.ExecutionSQL)
	}

	c.RefreshFast()
	resumed := c.Snapshot()
	if len(resumed.AverageSQL) != 1 ||
		resumed.AverageSQL[0].Calls != 1 ||
		resumed.AverageSQL[0].AverageUS != 150 {
		t.Fatalf("DB-time resumed average = %+v", resumed.AverageSQL)
	}
}

func TestCollectorRebasesRegressionAboveStartupBaseline(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			calls := []int64{100, 200, 150, 151}[call-1]
			dbTimes := []float64{1_000, 2_000, 1_500, 1_600}
			return []dbconn.Row{{
				int64(9), calls, dbTimes[call-1], "select nine",
			}}
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
	first := c.Snapshot()
	if len(first.AverageSQL) != 0 || len(first.ExecutionSQL) != 0 {
		t.Fatalf("first sample must establish baseline: avg=%+v execution=%+v",
			first.AverageSQL, first.ExecutionSQL)
	}

	c.RefreshFast()
	second := c.Snapshot()
	if len(second.AverageSQL) != 1 ||
		second.AverageSQL[0].Calls != 100 ||
		second.AverageSQL[0].AverageUS != 10 {
		t.Fatalf("second sample average = %+v, want calls=100 average=10",
			second.AverageSQL)
	}

	c.RefreshFast()
	reset := c.Snapshot()
	if len(reset.AverageSQL) != 0 || len(reset.ExecutionSQL) != 0 {
		t.Fatalf("regression above startup baseline must publish no rows: avg=%+v execution=%+v",
			reset.AverageSQL, reset.ExecutionSQL)
	}

	c.RefreshFast()
	resumed := c.Snapshot()
	if len(resumed.AverageSQL) != 1 ||
		resumed.AverageSQL[0].Calls != 1 ||
		resumed.AverageSQL[0].CallsDelta != 1 ||
		resumed.AverageSQL[0].AverageUS != 100 {
		t.Fatalf("sample after regression = %+v, want calls=1 delta=1 average=100",
			resumed.AverageSQL)
	}
}

func TestCollectorNewSQLLaterRegresses(t *testing.T) {
	fake := &fakeQueryer{}
	fake.queryFn = func(query string, call int) []dbconn.Row {
		switch query {
		case statementQuery:
			if call == 1 {
				return []dbconn.Row{{int64(9), int64(100), float64(1_000), "select nine"}}
			}
			calls := []int64{10, 20, 15, 16}[call-2]
			dbTimes := []float64{1_000, 2_000, 1_500, 1_600}
			return []dbconn.Row{{
				int64(22), calls, dbTimes[call-2], "select twenty two",
			}}
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
	baseline := c.Snapshot()
	if len(baseline.AverageSQL) != 0 || len(baseline.ExecutionSQL) != 0 {
		t.Fatalf("startup sample must establish baseline: avg=%+v execution=%+v",
			baseline.AverageSQL, baseline.ExecutionSQL)
	}

	c.RefreshFast()
	initial := c.Snapshot()
	if len(initial.AverageSQL) != 1 ||
		initial.AverageSQL[0].SQLID != 22 ||
		initial.AverageSQL[0].Calls != 10 ||
		initial.AverageSQL[0].CallsDelta != 10 {
		t.Fatalf("new SQL initial sample = %+v, want post-start calls=10",
			initial.AverageSQL)
	}

	c.RefreshFast()
	grown := c.Snapshot()
	if len(grown.AverageSQL) != 1 ||
		grown.AverageSQL[0].Calls != 20 ||
		grown.AverageSQL[0].CallsDelta != 20 {
		t.Fatalf("new SQL monotonic sample = %+v, want post-start calls=20",
			grown.AverageSQL)
	}

	c.RefreshFast()
	reset := c.Snapshot()
	if len(reset.AverageSQL) != 0 || len(reset.ExecutionSQL) != 0 {
		t.Fatalf("new SQL regression must publish no rows: avg=%+v execution=%+v",
			reset.AverageSQL, reset.ExecutionSQL)
	}

	c.RefreshFast()
	resumed := c.Snapshot()
	if len(resumed.AverageSQL) != 1 ||
		resumed.AverageSQL[0].SQLID != 22 ||
		resumed.AverageSQL[0].Calls != 1 ||
		resumed.AverageSQL[0].CallsDelta != 1 ||
		resumed.AverageSQL[0].AverageUS != 100 {
		t.Fatalf("new SQL sample after regression = %+v, want calls=1 delta=1 average=100",
			resumed.AverageSQL)
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
	return NewCollector(cfg, q, logging.New("health-test", ""), nil, nil)
}
