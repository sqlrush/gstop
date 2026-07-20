package emergency

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gstop/internal/dbconn"
)

// Inspection runs a configurable set of periodic health checks (memory usage,
// slow-SQL growth, long transactions, invalid indexes, un-vacuumed/un-analyzed
// tables, lock-wait ratio) and reports each as an independent alarm. Unlike the
// other scenarios it never marks itself triggered or renders a panel; it only
// emits alarms. Port of emergency/inspection.py.

const (
	inspectionName    = "Inspection"
	inspectionHeader  = "[EMER08 - Inspection]"
	inspectionPersist = 0
)

// SQL preserved verbatim from inspection.py.
const (
	inspStatementHistorySQL = `SELECT count(*) FROM dbe_perf.statement_history WHERE db_name != 'postgres';`

	inspTotalMemorySQL = `SELECT memorytype, memorymbytes FROM pg_catalog.pv_total_memory_detail();`

	inspInvalidIndexSQL = `SELECT a.schemaname, a.relname, a.indexrelname, b.indisusable, b.indisready, b.indisvalid
            FROM pg_stat_user_indexes a LEFT JOIN pg_index b ON a.indexrelid = b.indexrelid
            WHERE (b.indisusable = 'f' OR b.indisready = 'f' OR b.indisvalid = 'f')
                AND a.schemaname NOT IN (SELECT nspname FROM pg_namespace WHERE nspowner = 10);`

	inspUnvacuumSQL = `SELECT * FROM PG_STAT_ALL_TABLES
            WHERE schemaname NOT IN (SELECT nspname FROM PG_NAMESPACE WHERE nspowner = 10) AND last_vacuum IS NULL AND last_autovacuum IS NULL;`

	inspUnanalyzeSQL = `SELECT * FROM PG_STAT_ALL_TABLES
            WHERE schemaname NOT IN (SELECT nspname FROM PG_NAMESPACE WHERE nspowner = 10) AND last_analyze IS NULL AND last_autoanalyze IS NULL;`
)

// inspNoDetails renders the Python None the memory/slow-SQL checks return for
// the "详细信息" field, matching f"...{details}" with details == None.
const inspNoDetails = "None"

// inspLongTxnThreshold is the 120-minute long-transaction cut-off.
const inspLongTxnThreshold = 120 * 60 * time.Second

// slowSQLAlarmID identifies the slow-SQL check whose last count is pre-seeded at
// construction time, matching the constructor's special case.
const slowSQLAlarmID = "ALM-5101756"

// inspResult is one check's outcome: the numeric value used for the threshold
// comparison, its Python-faithful text form, and the detail string.
type inspResult struct {
	value     float64
	valueText string
	details   string
}

// inspCheck is one configured inspection item.
type inspCheck struct {
	alarmID          string
	name             string
	intervalSec      int
	interval         time.Duration
	threshold        float64
	checkConsecutive bool
	fn               func() inspResult
}

// inspParsed holds the four fields parsed from a check's config value.
type inspParsed struct {
	name             string
	intervalSec      int
	threshold        float64
	checkConsecutive int
}

// inspDescriptor maps a fixed alarm id to its check function, preserving the
// Python _check_functions ordering so alarms are evaluated deterministically.
type inspDescriptor struct {
	alarmID string
	fn      func() inspResult
}

// Inspection is the periodic health-check scenario.
type Inspection struct {
	*Base

	enabled bool
	checks  []inspCheck

	// lastCheck records when each item last ran, keyed by alarm id, so each
	// item can honour its own interval.
	lastCheck map[string]time.Time

	// slow-SQL running count cache (the Python last_statement_history).
	lastSlowSQL    int64
	hasLastSlowSQL bool
}

// NewInspection builds the inspection scenario, loading its items from config
// and pre-seeding the slow-SQL baseline when that item is enabled.
func NewInspection(deps Deps) *Inspection {
	s := &Inspection{
		Base:      NewBase(inspectionName, inspectionHeader, deps, inspectionPersist),
		lastCheck: map[string]time.Time{},
	}
	s.loadConfig()
	if s.enabled {
		deps.Logger.Info("Inspection module initialized with %d items loaded.", len(s.checks))
	} else {
		deps.Logger.Info("Inspection feature is disabled.")
	}
	// The slow-SQL baseline is seeded lazily on the first check (checkSlowSQL
	// handles !hasLastSlowSQL); running a query here would put a potentially slow
	// statement-history count on the startup critical path.
	return s
}

// checkDescriptors lists the built-in checks in their fixed evaluation order.
func (s *Inspection) checkDescriptors() []inspDescriptor {
	return []inspDescriptor{
		{"ALM-5101260", s.checkProcessMemoryUsage},
		{"ALM-5101263", s.checkDynamicMemoryUsage},
		{"ALM-5101756", s.checkSlowSQL},
		{"ALM-5023543", s.checkLongTransaction},
		{"ALM-5101989", s.checkInvalidIndex},
		{"ALM-5102000", s.checkUnvacuumedTables},
		{"ALM-5102001", s.checkUnanalyzedTables},
		{"ALM-5102006", s.checkLockWaitRatio},
	}
}

// loadConfig reads the master switch and each enabled item from config.
func (s *Inspection) loadConfig() {
	if !s.deps.Cfg.GetBool("emergency.inspection.enable", false) {
		return
	}
	s.enabled = true

	for _, d := range s.checkDescriptors() {
		raw := s.deps.Cfg.Get("emergency.inspection." + strings.ToLower(d.alarmID))
		if raw == nil {
			continue
		}
		text, ok := raw.(string)
		if !ok {
			s.deps.Logger.Error("Failed to parse configuration for inspection item %s: %v", d.alarmID, raw)
			continue
		}
		parsed, ok := s.parseCheckConfig(d.alarmID, text)
		if !ok {
			s.deps.Logger.Error("Failed to parse configuration for inspection item %s: %s", d.alarmID, text)
			continue
		}
		s.checks = append(s.checks, inspCheck{
			alarmID:          d.alarmID,
			name:             parsed.name,
			intervalSec:      parsed.intervalSec,
			interval:         time.Duration(parsed.intervalSec) * time.Second,
			threshold:        parsed.threshold,
			checkConsecutive: parsed.checkConsecutive != 0,
			fn:               d.fn,
		})
		s.deps.Logger.Info("Loaded inspection item: %s - %s, interval: %ds, threshold: %s, check_consecutive: %d",
			d.alarmID, parsed.name, parsed.intervalSec, pyFloat(parsed.threshold), parsed.checkConsecutive)
	}
}

// parseCheckConfig splits "NAME,INTERVAL,THRESHOLD,CHECK_CONSECUTIVE" and logs
// the specific failure, mirroring _parse_check_config.
func (s *Inspection) parseCheckConfig(alarmID, raw string) (inspParsed, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		s.deps.Logger.Error("Invalid configuration format for inspection item %s, expected 4 fields: %s", alarmID, raw)
		return inspParsed{}, false
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	intervalSec, err := strconv.Atoi(parts[1])
	if err != nil {
		s.logParseNumericError(alarmID, raw, err)
		return inspParsed{}, false
	}
	threshold, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		s.logParseNumericError(alarmID, raw, err)
		return inspParsed{}, false
	}
	checkConsecutive, err := strconv.Atoi(parts[3])
	if err != nil {
		s.logParseNumericError(alarmID, raw, err)
		return inspParsed{}, false
	}
	return inspParsed{
		name:             parts[0],
		intervalSec:      intervalSec,
		threshold:        threshold,
		checkConsecutive: checkConsecutive,
	}, true
}

func (s *Inspection) logParseNumericError(alarmID, raw string, err error) {
	s.deps.Logger.Error("Failed to parse numeric values for inspection item %s: %s, error: %v", alarmID, raw, err)
}

// hasCheck reports whether an item with the given alarm id was loaded.
func (s *Inspection) hasCheck(alarmID string) bool {
	for _, c := range s.checks {
		if c.alarmID == alarmID {
			return true
		}
	}
	return false
}

// Analyze evaluates every enabled item that is due and reports the ones that are
// at or above their threshold. Port of analyze().
func (s *Inspection) Analyze() {
	if !s.enabled {
		return
	}
	now := time.Now()
	for i := range s.checks {
		c := s.checks[i]
		if now.Sub(s.lastCheck[c.alarmID]) < c.interval {
			continue
		}
		res := c.fn()
		s.lastCheck[c.alarmID] = now
		if res.value < c.threshold {
			continue
		}
		key := inspectionName + "_" + c.alarmID
		text := fmt.Sprintf("Gausstop巡检告警，巡检项：%s，巡检间隔：%ds，巡检项阈值: %s，巡检项当前值：%s，详细信息：%s",
			c.name, c.intervalSec, pyFloat(c.threshold), res.valueText, res.details)
		s.deps.Alarm.CheckAndReport(s.deps.Logger, key, text, c.checkConsecutive)
	}
}

// HandleCommand has no interactive remediation for inspection alarms.
func (s *Inspection) HandleCommand(cmd *Command, line string) {}

// Check implementations ------------------------------------------------------

// checkProcessMemoryUsage reads the process-memory percentage from the memory
// panel, falling back to pv_total_memory_detail. Port of
// _check_process_memory_usage.
func (s *Inspection) checkProcessMemoryUsage() inspResult {
	if v, ok := inspMemoryValue(s.snap.Memory, 2); ok {
		return inspFloatResult(v, inspNoDetails)
	}
	maxMem, usedMem, ok := s.queryMemoryPair("max_process_memory", "process_used_memory")
	if !ok {
		return inspFloatResult(0, inspNoDetails)
	}
	if maxMem == 0 {
		s.deps.Logger.Error("Invalid max_process_memory value")
		return inspFloatResult(0, inspNoDetails)
	}
	return inspFloatResult(round2(usedMem/maxMem*100), inspNoDetails)
}

// checkDynamicMemoryUsage reads the dynamic-memory percentage from the memory
// panel, falling back to pv_total_memory_detail. Port of
// _check_dynamic_memory_usage.
func (s *Inspection) checkDynamicMemoryUsage() inspResult {
	if v, ok := inspMemoryValue(s.snap.Memory, 5); ok {
		return inspFloatResult(v, inspNoDetails)
	}
	maxMem, usedMem, ok := s.queryMemoryPair("max_dynamic_memory", "dynamic_used_memory")
	if !ok {
		return inspFloatResult(0, inspNoDetails)
	}
	if maxMem == 0 {
		s.deps.Logger.Error("Invalid max_dynamic_memory value")
		return inspFloatResult(0, inspNoDetails)
	}
	return inspFloatResult(round2(usedMem/maxMem*100), inspNoDetails)
}

// queryMemoryPair sums the two named rows from pv_total_memory_detail.
func (s *Inspection) queryMemoryPair(maxKey, usedKey string) (maxVal, usedVal float64, ok bool) {
	rows := s.deps.DB.Query(inspTotalMemorySQL)
	if rows == nil {
		s.deps.Logger.Error("Query pg_catalog.pv_total_memory_detail failed.")
		return 0, 0, false
	}
	for _, row := range rows {
		switch row.Str(0) {
		case maxKey:
			maxVal = dbFloat(row.Col(1))
		case usedKey:
			usedVal = dbFloat(row.Col(1))
		}
	}
	return maxVal, usedVal, true
}

// checkSlowSQL reports the growth in dbe_perf.statement_history since the last
// run. Port of _check_slow_sql.
func (s *Inspection) checkSlowSQL() inspResult {
	if !s.hasLastSlowSQL {
		n, ok := s.querySlowSQLCount()
		if !ok {
			return inspIntResult(0, inspNoDetails)
		}
		s.lastSlowSQL, s.hasLastSlowSQL = n, true
	}
	curr, ok := s.querySlowSQLCount()
	if !ok {
		s.hasLastSlowSQL = false
		return inspIntResult(0, inspNoDetails)
	}
	diff := curr - s.lastSlowSQL
	s.lastSlowSQL, s.hasLastSlowSQL = curr, true
	return inspIntResult(diff, inspNoDetails)
}

// querySlowSQLCount returns the current slow-SQL count, or ok=false on failure.
func (s *Inspection) querySlowSQLCount() (int64, bool) {
	rows := s.deps.DB.Query(inspStatementHistorySQL)
	if rows == nil {
		s.deps.Logger.Error("Query dbe_perf.statement_history failed.")
		return 0, false
	}
	if len(rows) == 0 {
		return 0, false
	}
	n, _ := dbInt64(rows[0].Col(0))
	return n, true
}

// checkLongTransaction counts sessions whose current transaction has been open
// for at least 120 minutes. Port of _check_long_transaction.
func (s *Inspection) checkLongTransaction() inspResult {
	if s.snap.Session == nil {
		return inspIntResult(0, inspNoDetails)
	}
	now := time.Now()
	var count int64
	for _, row := range s.snap.Session {
		xactStart, ok := row.Time(16)
		if !ok {
			continue
		}
		if now.Sub(xactStart) < inspLongTxnThreshold {
			continue
		}
		count++
	}
	return inspIntResult(count, fmt.Sprintf("当前长事务数量: %d个", count))
}

// checkInvalidIndex counts invalid/unusable indexes across user databases. Port
// of _check_invalid_index.
func (s *Inspection) checkInvalidIndex() inspResult {
	results := s.deps.DB.ExecuteOnUserDB(inspInvalidIndexSQL)
	if results == nil {
		s.deps.Logger.Error("Query index data failed.")
		return inspIntResult(0, inspNoDetails)
	}
	count, details := inspFormatResults(results, "索引失效的表（db.schema.table.index）：", []int{0, 1, 2}, true)
	return inspIntResult(count, details)
}

// checkUnvacuumedTables counts tables never vacuumed across user databases. Port
// of _check_unvacuumed_tables.
func (s *Inspection) checkUnvacuumedTables() inspResult {
	results := s.deps.DB.ExecuteOnUserDB(inspUnvacuumSQL)
	if results == nil {
		s.deps.Logger.Error("Query pg_stat_all_tables failed.")
		return inspIntResult(0, inspNoDetails)
	}
	count, details := inspFormatResults(results, "未做过vacuum的表（db.schema.table）：", []int{1, 2}, false)
	return inspIntResult(count, details)
}

// checkUnanalyzedTables counts tables never analyzed across user databases. Port
// of _check_unanalyzed_tables.
func (s *Inspection) checkUnanalyzedTables() inspResult {
	results := s.deps.DB.ExecuteOnUserDB(inspUnanalyzeSQL)
	if results == nil {
		s.deps.Logger.Error("Query pg_stat_all_tables failed.")
		return inspIntResult(0, inspNoDetails)
	}
	count, details := inspFormatResults(results, "未做过analyze的表（db.schema.table）：", []int{1, 2}, false)
	return inspIntResult(count, details)
}

// checkLockWaitRatio computes the percentage of user sessions that are blocked.
// Port of _check_lock_wait_ratio.
func (s *Inspection) checkLockWaitRatio() inspResult {
	var userSessions, waitSessions int
	var relations []string
	for _, row := range s.snap.Session {
		if row.Str(15) == "postgres" {
			continue
		}
		userSessions++
		if row.IsNull(7) {
			continue
		}
		waitSessions++
		relations = append(relations, fmt.Sprintf("%s -> %s", row.Str(0), row.Str(7)))
	}

	details := "锁等待状态会话列表（waiter -> holder）："
	if len(relations) == 0 {
		details += "无"
	} else {
		details += strings.Join(relations, "，")
	}

	var ratio float64
	if userSessions != 0 {
		ratio = round2(float64(waitSessions*100) / float64(userSessions))
	}
	return inspFloatResult(ratio, details)
}

// Helpers --------------------------------------------------------------------

// inspFloatResult renders a float value the way Python str() would.
func inspFloatResult(v float64, details string) inspResult {
	return inspResult{value: v, valueText: pyFloat(v), details: details}
}

// inspIntResult renders an integer value without a decimal point.
func inspIntResult(n int64, details string) inspResult {
	return inspResult{value: float64(n), valueText: strconv.FormatInt(n, 10), details: details}
}

// inspMemoryValue returns the memory panel cell at the first row and given
// column, or ok=false when the panel is absent or too short. The "absent"
// result drives the SQL fall-back, matching "if self.curr_memory is not None".
func inspMemoryValue(panels []MemPanel, col int) (float64, bool) {
	if len(panels) == 0 {
		return 0, false
	}
	rows := panels[0].Value
	if len(rows) == 0 || col < 0 || col >= len(rows[0]) {
		return 0, false
	}
	return dbFloat(rows[0][col]), true
}

// inspFormatResults counts the rows returned per database and joins their
// db.schema.table[.index] identifiers into a detail string. Port of
// _format_query_result_details. The caller has already handled a nil result.
func inspFormatResults(results map[string][]dbconn.Row, prefix string, cols []int, includeIndex bool) (int64, string) {
	var count int64
	var items []string
	for db, rows := range results {
		for _, row := range rows {
			count++
			schema := row.Str(cols[0])
			rel := row.Str(cols[1])
			if includeIndex && len(cols) > 2 {
				items = append(items, fmt.Sprintf("%s.%s.%s.%s", db, schema, rel, row.Str(cols[2])))
			} else {
				items = append(items, fmt.Sprintf("%s.%s.%s", db, schema, rel))
			}
		}
	}
	if len(items) == 0 {
		return count, prefix + "无"
	}
	return count, prefix + strings.Join(items, "，")
}
