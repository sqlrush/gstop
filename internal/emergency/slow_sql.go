package emergency

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
	"gstop/internal/model"
)

const (
	slowSQLName   = "SlowSQL"
	slowSQLHeader = "[EMER03 - SlowSQL]"
)

// slowWhitelistSQL are transaction-control and maintenance statements that are
// never treated as slow SQL. The Python source concatenated these string
// literals into a single element by accident (missing commas); per the port
// contract the intended per-statement list is spelled out here.
var slowWhitelistSQL = []string{
	"start transaction",
	"begin",
	"commit",
	"end",
	"vacuum",
	"analyze",
}

// slowStrategy is one time-window policy: within [startSec, endSec] (seconds of
// day, inclusive) the given check interval and slow-SQL / slow-procedure
// thresholds apply. Port of StrategyConfig.
type slowStrategy struct {
	startSec               int
	endSec                 int
	checkInterval          int
	slowSQLThreshold       int
	slowProcedureThreshold int
}

// slowThresholds carries the thresholds selected for the currently active
// strategy window, replacing the mutable self.check_interval/self.*_threshold
// fields the Python code stashed on the instance.
type slowThresholds struct {
	checkInterval          int
	slowSQLThreshold       int
	slowProcedureThreshold int
}

// SlowSQL reports long-running active statements to the alarm log and, when the
// terminate switch is on, kills their sessions. It is time-window driven: each
// window has its own check interval and thresholds. Unlike the CPU/IO scenarios
// it does not paint the interactive emergency panel (the Python analyze never
// set self.triggered), so it never marks itself triggered and HandleCommand is a
// no-op. Port of emergency/slow_sql.py.
type SlowSQL struct {
	*Base

	terminate        bool
	excludeUsers     []string
	excludeDatabases []string
	strategies       []slowStrategy
	procPatterns     []*regexp.Regexp
	whitePatterns    []*regexp.Regexp

	lastCheck time.Time
}

// NewSlowSQL builds the slow-SQL scenario, parsing and validating its strategy
// windows and compiling the procedure/whitelist matchers up front. Invalid
// configuration is logged and skipped rather than aborting startup (the Python
// constructor raised); see the strategy helpers.
func NewSlowSQL(deps Deps) *SlowSQL {
	s := &SlowSQL{Base: NewBase(slowSQLName, slowSQLHeader, deps, 0)}
	s.terminate = deps.Cfg.GetBool("emergency.slow_sql.terminate", false)
	s.excludeDatabases = slowSplitConfig(deps.Cfg, "emergency.slow_sql.exclude_databases")
	s.excludeUsers = slowSplitConfig(deps.Cfg, "emergency.slow_sql.exclude_users")
	procedures := slowSplitConfig(deps.Cfg, "emergency.slow_sql.procedure")

	s.strategies = slowParseStrategies(deps.Cfg, deps.Logger)
	slowValidateStrategies(s.strategies, deps.Logger)

	s.procPatterns = slowCompilePatterns(procedures, deps.Logger)
	s.whitePatterns = slowCompilePatterns(slowWhitelistSQL, deps.Logger)
	return s
}

// Analyze selects the active strategy window, throttles by its check interval,
// then inspects every session for a slow statement. Port of analyze().
func (s *SlowSQL) Analyze() {
	now := time.Now()
	thresholds, ok := s.matchStrategy(slowSecondsOfDay(now))
	if !ok {
		return
	}
	if !s.dueForCheck(now, thresholds.checkInterval) {
		return
	}
	for _, row := range s.snap.Session {
		s.inspectSession(row, now, thresholds)
	}
}

// HandleCommand is intentionally empty: slow SQL has no interactive remediation.
// Port of handle_emergency_command (pass).
func (s *SlowSQL) HandleCommand(cmd *Command, line string) {}

// matchStrategy returns the thresholds of the first strategy whose window
// contains nowSec, matching check_strategies_and_update.
func (s *SlowSQL) matchStrategy(nowSec int) (slowThresholds, bool) {
	for _, st := range s.strategies {
		if st.startSec <= nowSec && nowSec <= st.endSec {
			return slowThresholds{
				checkInterval:          st.checkInterval,
				slowSQLThreshold:       st.slowSQLThreshold,
				slowProcedureThreshold: st.slowProcedureThreshold,
			}, true
		}
	}
	return slowThresholds{}, false
}

// dueForCheck reports whether enough time has elapsed since the last inspection,
// updating the last-check timestamp when it proceeds. Port of the
// last_check_timestamp logic in analyze().
func (s *SlowSQL) dueForCheck(now time.Time, interval int) bool {
	if s.lastCheck.IsZero() {
		s.lastCheck = now
		return true
	}
	if int(now.Sub(s.lastCheck).Seconds()) < interval {
		return false
	}
	s.lastCheck = now
	return true
}

// inspectSession applies the slow-SQL filters to one session row and reports it
// when it qualifies. Port of the per-session body of analyze().
func (s *SlowSQL) inspectSession(row dbconn.Row, now time.Time, th slowThresholds) {
	if row.Str(9) != "active" || row.IsNull(17) {
		return
	}
	queryStart, ok := row.Time(17)
	if !ok {
		return
	}
	if slowContains(s.excludeUsers, row.Str(1)) || slowContains(s.excludeDatabases, row.Str(15)) {
		return
	}

	queryUpper := strings.ToUpper(row.Str(5))
	threshold := th.slowSQLThreshold
	if len(s.procPatterns) != 0 && slowMatchAny(s.procPatterns, queryUpper) {
		threshold = th.slowProcedureThreshold
	}
	if int(now.Sub(queryStart).Seconds()) < threshold {
		return
	}
	if slowMatchAny(s.whitePatterns, queryUpper) {
		return
	}
	s.report(row, threshold)
}

// report logs the slow statement, raises the alarm, and terminates the session
// when both the scenario and global terminate switches are on. Port of the
// reporting tail of analyze().
func (s *SlowSQL) report(row dbconn.Row, threshold int) {
	pid := row.Col(0)
	sessionID := row.Col(3)
	usename := row.Str(1)
	datname := row.Str(15)
	uniqueSQLID := model.DisplayValue(row.Col(4))
	query := row.Str(5)
	xactStart := model.DisplayValue(row.Col(16))
	queryStart := model.DisplayValue(row.Col(17))

	s.deps.Logger.Warning(
		"Slow SQL: %s  Query start: %s  Xact start: %s  Threshold: %d  SQL ID: %s  SQL TEXT: %s",
		query, queryStart, xactStart, threshold, uniqueSQLID, query)

	key := fmt.Sprintf("%s_%s_%s_%s", slowSQLName, datname, usename, uniqueSQLID)
	command := model.TerminateSessionCmd(pid, sessionID)
	value := s.buildAlarmValue(queryStart, xactStart, threshold, uniqueSQLID, query, command)
	s.deps.Alarm.CheckAndReport(s.deps.Logger, key, value, true)

	if s.terminate && s.deps.Cfg.GetBool("main.support_terminate", false) {
		s.terminateSession(pid, sessionID)
	}
}

// buildAlarmValue renders the alarm text, appending the kill command only when
// emergency commands are permitted. Port of the value f-strings in analyze().
func (s *SlowSQL) buildAlarmValue(queryStart, xactStart string, threshold int, sqlID, query, command string) string {
	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		return fmt.Sprintf(
			"Gausstop检测到慢SQL，查询开始时间: %s，事务开始时间: %s，慢SQL阈值: %d，SQL ID: %s，SQL语句: %s，使用以下命令查杀慢SQL会话: %s",
			queryStart, xactStart, threshold, sqlID, query, command)
	}
	return fmt.Sprintf(
		"Gausstop检测到慢SQL，查询开始时间: %s，事务开始时间: %s，慢SQL阈值: %d，SQL ID: %s，SQL语句: %s",
		queryStart, xactStart, threshold, sqlID, query)
}

// --- config parsing helpers -------------------------------------------------

// slowSplitConfig returns the semicolon-separated list at key, or nil when the
// key is absent. Matches Python's `value.split(';') if value is not None else []`.
func slowSplitConfig(cfg *config.Config, key string) []string {
	v := cfg.Get(key)
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return strings.Split(s, ";")
}

// slowCompilePatterns builds a case-insensitive word-boundary matcher for each
// non-empty term. Terms are regex-escaped so config input cannot inject regex.
// Port of the procedure/whitelist pattern initialisation.
func slowCompilePatterns(words []string, logger *logging.Logger) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, w := range words {
		if len(w) == 0 {
			continue
		}
		p, err := regexp.Compile(`(?i)\b` + regexp.QuoteMeta(w) + `\b`)
		if err != nil {
			logger.Error("slow_sql: compile pattern %q failed: %v", w, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// slowParseStrategies parses strategy0..strategy9 from config, skipping absent
// or empty entries. A malformed entry is logged and skipped (the Python
// constructor raised ValueError; this returns *SlowSQL and cannot). Port of
// _parse_strategy.
func slowParseStrategies(cfg *config.Config, logger *logging.Logger) []slowStrategy {
	var out []slowStrategy
	for i := 0; i < 10; i++ {
		raw := cfg.Get(fmt.Sprintf("emergency.slow_sql.strategy%d", i))
		if raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok || value == "" {
			continue
		}
		st, err := slowParseStrategy(value)
		if err != nil {
			logger.Error("slow_sql: %v", err)
			continue
		}
		out = append(out, st)
	}
	return out
}

// slowParseStrategy parses one "start,end,interval,sql_thresh,proc_thresh" entry
// and validates its fields. Port of the body of _parse_strategy's loop.
func slowParseStrategy(value string) (slowStrategy, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 5 {
		return slowStrategy{}, fmt.Errorf("strategy format error: %s", value)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	startSec, err := slowParseTimeOfDay(parts[0])
	if err != nil {
		return slowStrategy{}, err
	}
	endSec, err := slowParseTimeOfDay(parts[1])
	if err != nil {
		return slowStrategy{}, err
	}
	if startSec >= endSec {
		return slowStrategy{}, fmt.Errorf("start time %s must be before end time %s", parts[0], parts[1])
	}

	interval, e1 := strconv.Atoi(parts[2])
	sqlThresh, e2 := strconv.Atoi(parts[3])
	procThresh, e3 := strconv.Atoi(parts[4])
	if e1 != nil || e2 != nil || e3 != nil {
		return slowStrategy{}, fmt.Errorf("number format error: %s, %s, %s", parts[2], parts[3], parts[4])
	}

	if err := slowValidatePositive(interval, sqlThresh, procThresh); err != nil {
		return slowStrategy{}, err
	}
	return slowStrategy{
		startSec:               startSec,
		endSec:                 endSec,
		checkInterval:          interval,
		slowSQLThreshold:       sqlThresh,
		slowProcedureThreshold: procThresh,
	}, nil
}

// slowValidatePositive enforces the >0 constraints on the three numeric fields.
func slowValidatePositive(interval, sqlThresh, procThresh int) error {
	if interval <= 0 {
		return fmt.Errorf("the check interval must be greater than 0: %d", interval)
	}
	if sqlThresh <= 0 {
		return fmt.Errorf("the slow SQL threshold must be greater than 0: %d", sqlThresh)
	}
	if procThresh <= 0 {
		return fmt.Errorf("the slow procedure threshold must be greater than 0: %d", procThresh)
	}
	return nil
}

// slowParseTimeOfDay parses an "HH:MM" clock time into seconds of day. Port of
// _parse_time_string.
func slowParseTimeOfDay(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("time format error: %s", s)
	}
	return t.Hour()*3600 + t.Minute()*60, nil
}

// slowValidateStrategies logs any pair of overlapping windows. The Python
// _validate_strategy raised RuntimeError; here the misconfiguration is logged so
// startup can continue, and the first matching window still wins at runtime.
func slowValidateStrategies(strategies []slowStrategy, logger *logging.Logger) {
	for i := 0; i < len(strategies); i++ {
		for j := i + 1; j < len(strategies); j++ {
			a, b := strategies[i], strategies[j]
			if a.endSec <= b.startSec || b.endSec <= a.startSec {
				continue
			}
			logger.Error("slow_sql: validate strategy failed: %s %s %s %s",
				slowFormatSec(a.startSec), slowFormatSec(a.endSec),
				slowFormatSec(b.startSec), slowFormatSec(b.endSec))
		}
	}
}

// --- small utilities --------------------------------------------------------

// slowSecondsOfDay returns t's wall-clock time as seconds since midnight,
// preserving the seconds granularity of Python's datetime.now().time() so an
// end boundary of HH:MM excludes HH:MM:SS just as the original comparison did.
func slowSecondsOfDay(t time.Time) int {
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

func slowFormatSec(sec int) string {
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
}

func slowContains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func slowMatchAny(patterns []*regexp.Regexp, sql string) bool {
	for _, p := range patterns {
		if p.MatchString(sql) {
			return true
		}
	}
	return false
}
