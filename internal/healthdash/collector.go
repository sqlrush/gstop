package healthdash

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbcompat"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
)

const defaultSlowInterval = 300 * time.Second

// Queryer is the database boundary used by Collector and DetailLoader.
type Queryer interface {
	Query(string) []dbconn.Row
	ExecuteOnUserDB(string) map[string][]dbconn.Row
	Kind() dbcompat.Kind
}

// MemoryRefreshGate reuses gstop's CPU and minimum-interval protection for the
// expensive session-memory query.
type MemoryRefreshGate interface {
	ShouldRefreshMemory(string) bool
}

// Collector owns the startup baselines and three independent refresh classes.
// RefreshFast is called by app's post-monitor hook; memory and cross-database
// work run in their own single-flight goroutines.
type Collector struct {
	cfg        *config.Config
	db         Queryer
	logger     *logging.Logger
	now        func() time.Time
	memoryGate MemoryRefreshGate

	mu          sync.RWMutex
	snapshot    Snapshot
	active      []ActiveSQL
	lifecycleMu sync.Mutex

	statementBaseline    []StatementSample
	statementBaselineSet bool
	waitBaseline         []WaitSample
	waitBaselineCPU      int64
	waitBaselineSet      bool
	lastMemoryRefresh    time.Time
	lastSlowRefresh      time.Time

	fastBusy   atomic.Bool
	memoryBusy atomic.Bool
	slowBusy   atomic.Bool
	stopping   atomic.Bool
	wg         sync.WaitGroup
}

// NewCollector builds a collector with empty baselines stamped at process start.
func NewCollector(cfg *config.Config, db Queryer, logger *logging.Logger, gates ...MemoryRefreshGate) *Collector {
	now := time.Now
	memoryEnabled := cfg.GetBool("main.dynamic_mem_enable", false) && cfg.GetInt("main.mem_interval", 30) > 0
	var gate MemoryRefreshGate
	if len(gates) > 0 {
		gate = gates[0]
	}
	return &Collector{
		cfg: cfg, db: db, logger: logger, now: now, memoryGate: gate,
		snapshot: Snapshot{
			StartedAt:     now(),
			MemoryEnabled: memoryEnabled,
		},
	}
}

// RefreshFast samples statement, active-session, wait, and CPU counters. A
// concurrent call is skipped; the caller is never queued behind a slow sample.
func (c *Collector) RefreshFast() {
	if !c.beginWork(&c.fastBusy) {
		return
	}
	defer c.endWork(&c.fastBusy)

	refreshedAt := c.now()
	var statementRows, activeRows, waitRows, cpuRows []dbconn.Row
	var queries sync.WaitGroup
	queries.Add(4)
	go func() {
		defer queries.Done()
		statementRows = c.db.Query(statementQuery)
	}()
	go func() {
		defer queries.Done()
		activeRows = c.db.Query(activeSQLQuery)
	}()
	go func() {
		defer queries.Done()
		waitRows = c.db.Query(waitQuery)
	}()
	go func() {
		defer queries.Done()
		cpuRows = c.db.Query(cpuQuery)
	}()
	queries.Wait()

	var errors []string
	statements, statementOK := parseStatements(statementRows)
	active, activeOK := parseActive(activeRows)
	waits, waitOK := parseWaits(waitRows)
	cpu, cpuOK := parseCPU(cpuRows)
	if !statementOK {
		errors = append(errors, "SQL统计查询失败")
	}
	if !activeOK {
		errors = append(errors, "活跃SQL查询失败")
	}
	if !waitOK {
		errors = append(errors, "等待事件查询失败")
	}
	if !cpuOK {
		errors = append(errors, "DB CPU查询失败")
	}

	c.mu.Lock()
	if activeOK {
		c.active = append([]ActiveSQL(nil), active...)
	}
	// A failed active-session sample must not reuse the previous round's running
	// durations. Keep c.active only as the last successful internal sample; this
	// round deliberately contributes no in-flight SQL time.
	if activeOK {
		active = append([]ActiveSQL(nil), c.active...)
	} else {
		active = nil
	}
	if statementOK {
		if !c.statementBaselineSet {
			c.statementBaseline = append([]StatementSample(nil), statements...)
			c.statementBaselineSet = true
		} else {
			c.statementBaseline = rebaseStatements(c.statementBaseline, statements)
		}
		c.snapshot.AverageSQL, c.snapshot.ExecutionSQL = BuildSQLMetrics(statements, c.statementBaseline, active)
	} else {
		c.snapshot.AverageSQL = nil
		c.snapshot.ExecutionSQL = nil
	}
	if waitOK && cpuOK {
		if !c.waitBaselineSet {
			c.waitBaseline = append([]WaitSample(nil), waits...)
			c.waitBaselineCPU = cpu
			c.waitBaselineSet = true
		} else {
			c.waitBaseline = rebaseWaits(c.waitBaseline, waits)
			if cpu < c.waitBaselineCPU {
				c.waitBaselineCPU = cpu
			}
		}
		c.snapshot.Waits, c.snapshot.CPU = BuildWaitMetrics(waits, c.waitBaseline, cpu, c.waitBaselineCPU)
	} else {
		c.snapshot.Waits = nil
		c.snapshot.CPU = CPUStat{}
	}
	c.snapshot.FastRefreshedAt = refreshedAt
	c.snapshot.FastError = strings.Join(errors, "；")
	c.mu.Unlock()

	c.startMemoryRefresh(refreshedAt, active)
	c.startSlowRefresh(false)
}

// UpdatePlanChanges publishes the plan engine's retained event history.
func (c *Collector) UpdatePlanChanges(events []PlanChangeEvent) {
	c.mu.Lock()
	c.snapshot.PlanChanges = append([]PlanChangeEvent(nil), events...)
	c.mu.Unlock()
}

// Snapshot returns a deep copy of the latest published dashboard state.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot.Clone()
}

// RequestSlowRefresh starts a manual cross-database refresh regardless of age.
func (c *Collector) RequestSlowRefresh() { c.startSlowRefresh(true) }

func (c *Collector) startMemoryRefresh(now time.Time, active []ActiveSQL) {
	if !c.cfg.GetBool("main.dynamic_mem_enable", false) {
		return
	}
	interval := time.Duration(c.cfg.GetInt("main.mem_interval", 30)) * time.Second
	if interval <= 0 {
		return
	}
	c.mu.Lock()
	due := c.lastMemoryRefresh.IsZero() || now.Sub(c.lastMemoryRefresh) >= interval
	if due && c.memoryBusy.Load() {
		c.mu.Unlock()
		return
	}
	if due && c.memoryGate != nil && !c.memoryGate.ShouldRefreshMemory("health-dashboard") {
		c.mu.Unlock()
		return
	}
	if due && c.beginWork(&c.memoryBusy) {
		c.lastMemoryRefresh = now
		go c.refreshMemory(now, append([]ActiveSQL(nil), active...))
	}
	c.mu.Unlock()
}

func (c *Collector) refreshMemory(refreshedAt time.Time, active []ActiveSQL) {
	defer c.endWork(&c.memoryBusy)
	rows := c.db.Query(sessionMemoryQuery)
	if rows == nil {
		c.mu.Lock()
		c.snapshot.MemorySQL = nil
		c.snapshot.MemoryError = "动态内存查询失败"
		c.mu.Unlock()
		return
	}
	memory := make(map[string]float64, len(rows))
	for _, row := range rows {
		value, _ := row.Float(1)
		memory[row.Str(0)] = value
	}
	for i := range active {
		active[i].MemoryMB = memory[active[i].SessionID]
	}
	c.mu.Lock()
	c.snapshot.MemorySQL = BuildMemoryMetrics(active)
	c.snapshot.MemoryRefreshedAt = refreshedAt
	c.snapshot.MemoryError = ""
	c.mu.Unlock()
}

func (c *Collector) startSlowRefresh(force bool) {
	now := c.now()
	interval := time.Duration(c.cfg.GetInt("main.health_slow_interval", int(defaultSlowInterval/time.Second))) * time.Second
	if interval <= 0 {
		interval = defaultSlowInterval
	}
	c.mu.Lock()
	due := force || c.lastSlowRefresh.IsZero() || now.Sub(c.lastSlowRefresh) >= interval
	if !due || !c.beginWork(&c.slowBusy) {
		c.mu.Unlock()
		return
	}
	c.lastSlowRefresh = now
	c.snapshot.SlowRefreshing = true
	c.mu.Unlock()
	go c.refreshSlow(now)
}

func (c *Collector) refreshSlow(refreshedAt time.Time) {
	defer c.endWork(&c.slowBusy)

	var analyzeResults, indexResults map[string][]dbconn.Row
	var queries sync.WaitGroup
	queries.Add(2)
	go func() {
		defer queries.Done()
		analyzeResults = c.db.ExecuteOnUserDB(analyzeHistoryQuery)
	}()
	go func() {
		defer queries.Done()
		indexResults = c.db.ExecuteOnUserDB(invalidIndexQuery)
	}()
	queries.Wait()
	analyze, analyzeErrors := parseAnalyzeResults(analyzeResults)
	indexes, indexErrors := parseIndexResults(indexResults)

	c.mu.Lock()
	if analyzeResults != nil {
		c.snapshot.AnalyzeHistory = mergeAnalyzeHistory(c.snapshot.AnalyzeHistory, analyze, analyzeErrors)
	} else {
		c.snapshot.AnalyzeHistory = nil
	}
	if indexResults != nil {
		c.snapshot.InvalidIndexes = mergeInvalidIndexes(c.snapshot.InvalidIndexes, indexes, indexErrors)
	} else {
		c.snapshot.InvalidIndexes = nil
	}
	c.snapshot.DatabaseErrors = append(analyzeErrors, indexErrors...)
	c.snapshot.SlowRefreshedAt = refreshedAt
	c.snapshot.SlowRefreshing = false
	c.mu.Unlock()
}

// Stop prevents new background work and waits for already-started refreshes.
func (c *Collector) Stop() {
	c.Cancel()
	c.wg.Wait()
}

// Cancel prevents new background work without waiting for current operations;
// their database contexts are canceled separately by the app's global q path.
func (c *Collector) Cancel() {
	c.lifecycleMu.Lock()
	c.stopping.Store(true)
	c.lifecycleMu.Unlock()
}

func (c *Collector) beginWork(busy *atomic.Bool) bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.stopping.Load() || !busy.CompareAndSwap(false, true) {
		return false
	}
	c.wg.Add(1)
	return true
}

func (c *Collector) endWork(busy *atomic.Bool) {
	busy.Store(false)
	c.wg.Done()
}

func parseStatements(rows []dbconn.Row) ([]StatementSample, bool) {
	if rows == nil {
		return nil, false
	}
	out := make([]StatementSample, 0, len(rows))
	for _, row := range rows {
		id, ok := row.Int(0)
		if !ok || id == 0 {
			continue
		}
		calls, _ := row.Int(1)
		dbTime, _ := row.Float(2)
		out = append(out, StatementSample{SQLID: id, Calls: calls, DBTimeUS: dbTime, Query: row.Str(3)})
	}
	return out, true
}

func parseActive(rows []dbconn.Row) ([]ActiveSQL, bool) {
	if rows == nil {
		return nil, false
	}
	out := make([]ActiveSQL, 0, len(rows))
	for _, row := range rows {
		id, ok := row.Int(0)
		if !ok || id == 0 {
			continue
		}
		pid, _ := row.Int(1)
		elapsed, _ := row.Float(4)
		out = append(out, ActiveSQL{
			SQLID: id, PID: pid, SessionID: row.Str(2), Query: row.Str(3), ElapsedUS: elapsed,
		})
	}
	return out, true
}

func parseWaits(rows []dbconn.Row) ([]WaitSample, bool) {
	if rows == nil {
		return nil, false
	}
	out := make([]WaitSample, 0, len(rows))
	for _, row := range rows {
		waits, _ := row.Int(1)
		timeUS, _ := row.Int(2)
		out = append(out, WaitSample{Event: row.Str(0), Waits: waits, TimeUS: timeUS, Type: row.Str(3)})
	}
	return out, true
}

func parseCPU(rows []dbconn.Row) (int64, bool) {
	if rows == nil || len(rows) == 0 {
		return 0, false
	}
	cpu, ok := rows[0].Int(0)
	return cpu, ok
}

func rebaseStatements(baseline, current []StatementSample) []StatementSample {
	out := append([]StatementSample(nil), baseline...)
	positions := make(map[int64]int, len(out))
	for i, row := range out {
		positions[row.SQLID] = i
	}
	for _, row := range current {
		if index, ok := positions[row.SQLID]; ok && row.Calls < out[index].Calls {
			out[index].Calls = row.Calls
			out[index].DBTimeUS = row.DBTimeUS
		}
	}
	return out
}

func rebaseWaits(baseline, current []WaitSample) []WaitSample {
	out := append([]WaitSample(nil), baseline...)
	positions := make(map[string]int, len(out))
	for i, row := range out {
		positions[row.Event] = i
	}
	for _, row := range current {
		index, ok := positions[row.Event]
		if !ok {
			continue
		}
		if row.Waits < out[index].Waits {
			out[index].Waits = row.Waits
		}
		if row.TimeUS < out[index].TimeUS {
			out[index].TimeUS = row.TimeUS
		}
	}
	return out
}

func parseAnalyzeResults(results map[string][]dbconn.Row) ([]AnalyzeRecord, []DatabaseError) {
	if results == nil {
		return nil, []DatabaseError{{Database: "*", Area: "统计信息", Message: "无法枚举或连接用户数据库"}}
	}
	var records []AnalyzeRecord
	var errors []DatabaseError
	for database, rows := range results {
		if rows == nil {
			errors = append(errors, DatabaseError{Database: database, Area: "统计信息", Message: "查询失败或无权限"})
			continue
		}
		for _, row := range rows {
			if at, ok := rowTime(row, 2); ok {
				records = append(records, AnalyzeRecord{Database: database, Schema: row.Str(0), Table: row.Str(1), Source: "ANALYZE", At: at})
			}
			if at, ok := rowTime(row, 3); ok {
				records = append(records, AnalyzeRecord{Database: database, Schema: row.Str(0), Table: row.Str(1), Source: "AUTOANALYZE", At: at})
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].At.After(records[j].At) })
	if len(records) > 20 {
		records = records[:20]
	}
	sortDatabaseErrors(errors)
	return records, errors
}

func parseIndexResults(results map[string][]dbconn.Row) ([]InvalidIndex, []DatabaseError) {
	if results == nil {
		return nil, []DatabaseError{{Database: "*", Area: "失效索引", Message: "无法枚举或连接用户数据库"}}
	}
	var indexes []InvalidIndex
	var errors []DatabaseError
	for database, rows := range results {
		if rows == nil {
			errors = append(errors, DatabaseError{Database: database, Area: "失效索引", Message: "查询失败或无权限"})
			continue
		}
		for _, row := range rows {
			indexes = append(indexes, InvalidIndex{
				Database: database, Schema: row.Str(0), Table: row.Str(1), Index: row.Str(2),
				Usable: rowBool(row, 3), Ready: rowBool(row, 4), Valid: rowBool(row, 5),
			})
		}
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		left := indexes[i].Database + "." + indexes[i].Schema + "." + indexes[i].Index
		right := indexes[j].Database + "." + indexes[j].Schema + "." + indexes[j].Index
		return left < right
	})
	sortDatabaseErrors(errors)
	return indexes, errors
}

func mergeAnalyzeHistory(previous, current []AnalyzeRecord, errors []DatabaseError) []AnalyzeRecord {
	failed := failedDatabases(errors)
	out := append([]AnalyzeRecord(nil), current...)
	for _, row := range previous {
		if failed[row.Database] {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func mergeInvalidIndexes(previous, current []InvalidIndex, errors []DatabaseError) []InvalidIndex {
	failed := failedDatabases(errors)
	out := append([]InvalidIndex(nil), current...)
	for _, row := range previous {
		if failed[row.Database] {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := out[i].Database + "." + out[i].Schema + "." + out[i].Index
		right := out[j].Database + "." + out[j].Schema + "." + out[j].Index
		return left < right
	})
	return out
}

func failedDatabases(errors []DatabaseError) map[string]bool {
	out := make(map[string]bool, len(errors))
	for _, item := range errors {
		if item.Database != "*" {
			out[item.Database] = true
		}
	}
	return out
}

func rowTime(row dbconn.Row, index int) (time.Time, bool) {
	if value, ok := row.Time(index); ok {
		return value, true
	}
	text := strings.TrimSpace(row.Str(index))
	for _, layout := range []string{"2006-01-02 15:04:05.999999-07:00", "2006-01-02 15:04:05", time.RFC3339Nano} {
		if value, err := time.Parse(layout, text); err == nil {
			return value, true
		}
	}
	return time.Time{}, false
}

func rowBool(row dbconn.Row, index int) bool {
	switch value := row.Col(index).(type) {
	case bool:
		return value
	default:
		text := strings.ToLower(strings.TrimSpace(row.Str(index)))
		return text == "t" || text == "true" || text == "1"
	}
}

func sortDatabaseErrors(errors []DatabaseError) {
	sort.SliceStable(errors, func(i, j int) bool {
		return fmt.Sprintf("%s.%s", errors[i].Database, errors[i].Area) < fmt.Sprintf("%s.%s", errors[j].Database, errors[j].Area)
	})
}
