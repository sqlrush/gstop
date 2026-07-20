package emergency

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

const (
	threadPoolFullName    = "ThreadPoolFull"
	threadPoolFullHeader  = "[EMER06 - ThreadPoolFull] - select the SQL id and press 'k' to terminate sessions"
	threadPoolFullPersist = 120
)

// threadPoolDetailHeader labels the per-SQL detail rows. Spacing is preserved
// verbatim from thread_pool_full.py.
const threadPoolDetailHeader = "SQL_ID           XACT_OVERTIME_SESS            IDLE_SESS        IDLE_IN_XACT_SESS"

// idle / idle-in-transaction quick-kill templates surfaced in the panel and the
// alarm. [LIMIT] is a placeholder the operator replaces with a session cap.
const (
	threadPoolIdleCmd       = "SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE datname != 'postgres' AND state = 'idle' LIMIT [LIMIT];"
	threadPoolIdleInXactCmd = "SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE datname != 'postgres' AND state = 'idle in transaction' LIMIT [LIMIT];"
)

// ThreadPoolFull detects a saturated thread pool and surfaces the sessions
// (grouped by SQL id) driving it for termination. Port of
// emergency/thread_pool_full.py.
type ThreadPoolFull struct {
	*Base
}

// NewThreadPoolFull builds the thread-pool-full scenario, seeding the terminate
// whitelist from config.
func NewThreadPoolFull(deps Deps) *ThreadPoolFull {
	base := NewBase(threadPoolFullName, threadPoolFullHeader, deps, threadPoolFullPersist)
	base.whitelist = tpoolWhitelist(deps.Cfg.GetString("emergency.thread_pool_full.user_whitelist", ""))
	return &ThreadPoolFull{Base: base}
}

// tpoolCounts holds the per-cycle global session tallies shown in the summary
// line.
type tpoolCounts struct {
	idle         int
	idleInXact   int
	active       int
	xactOvertime int
}

// tpoolSQLAgg is one SQL id's [idle, idle-in-xact, active-overtime] tally,
// mirroring the 3-element list keyed by unique_sql_id in the Python source.
type tpoolSQLAgg struct {
	sqlID             int64
	idleNum           int
	idleInXactNum     int
	activeOvertimeNum int
}

// Analyze triggers when the thread-pool usage percentage is at or above the
// threshold, then aggregates session state by SQL id.
func (s *ThreadPoolFull) Analyze() {
	data := s.instanceCell(13) // THREADPOOL in instance.cfg, like '50%'
	if data == "" {
		return
	}
	pct, ok := tpoolParsePct(data)
	if !ok {
		return
	}
	threshold := s.deps.Cfg.GetInt("emergency.thread_pool_full.thread_pool_full_thresh", 70)
	if pct < threshold {
		return
	}

	overtimeMS := s.deps.Cfg.GetFloat("emergency.thread_pool_full.overtime_thresh", 50)
	counts, topSQL := s.analyzeSessionState(overtimeMS)
	s.triggerEmergency(pct, threshold, counts, topSQL)
	s.autoTerminate()
}

// analyzeSessionState walks the session snapshot, accumulating global tallies and
// per-SQL-id counts, and records the pids of active over-threshold transactions.
// Returns the tallies and the SQL aggregates sorted by idle count descending.
func (s *ThreadPoolFull) analyzeSessionState(overtimeMS float64) (tpoolCounts, []tpoolSQLAgg) {
	var counts tpoolCounts
	agg := map[int64]*tpoolSQLAgg{}
	var order []int64

	ensure := func(sqlID int64, ok bool) *tpoolSQLAgg {
		if !ok || sqlID == 0 {
			return nil
		}
		v := agg[sqlID]
		if v == nil {
			v = &tpoolSQLAgg{sqlID: sqlID}
			agg[sqlID] = v
			order = append(order, sqlID)
		}
		return v
	}

	for _, row := range s.snap.Session {
		sqlID, ok := dbInt64(row.Col(4))
		v := ensure(sqlID, ok)
		switch row.Str(9) {
		case "idle":
			counts.idle++
			if v != nil {
				v.idleNum++
			}
		case "idle in transaction":
			counts.idleInXact++
			if v != nil {
				v.idleInXactNum++
			}
		case "active":
			s.classifyActive(row, overtimeMS, &counts, v)
		}
	}
	return counts, tpoolSortByIdle(agg, order)
}

// classifyActive tallies an active session, and — when its transaction has been
// running past the overtime threshold — records its pid and per-SQL count.
// Matches the `state == 'active' and xact_start is not None` branch.
func (s *ThreadPoolFull) classifyActive(row dbconn.Row, overtimeMS float64, counts *tpoolCounts, v *tpoolSQLAgg) {
	if row.IsNull(16) { // xact_start
		return
	}
	counts.active++
	xactRunMS := round2(dbFloat(row.Col(18)) / 1000) // "us" -> "ms"
	if xactRunMS < overtimeMS {
		return
	}
	counts.xactOvertime++
	if pid, ok := dbInt64(row.Col(0)); ok {
		s.pids = append(s.pids, pid)
	}
	if v != nil {
		v.activeOvertimeNum++
	}
}

// triggerEmergency renders the summary, per-SQL detail, and quick-kill commands,
// then reports the alarm.
func (s *ThreadPoolFull) triggerEmergency(pct, threshold int, counts tpoolCounts, topSQL []tpoolSQLAgg) {
	s.Trigger()

	s.AddInfo(fmt.Sprintf("ACTIVE_SESS: %d    IDLE_SESS: %d    IDLE_IN_XACT_SESS: %d    XACT_OVERTIME_SESS: %d",
		counts.active, counts.idle, counts.idleInXact, counts.xactOvertime))
	s.AddInfo(threadPoolDetailHeader)
	for i, agg := range topSQL {
		if i >= 3 {
			break
		}
		s.AddInfo(fmt.Sprintf("%-15d  %-25d     %-15d  %-15d",
			agg.sqlID, agg.activeOvertimeNum, agg.idleNum, agg.idleInXactNum))
	}

	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		s.AddInfo("")
		s.AppendSplitString(threadPoolIdleCmd, "CMD")
		s.AppendSplitString(threadPoolIdleInXactCmd, "CMD")
	}

	s.reportAlarm(pct, threshold)
}

// reportAlarm builds and reports the thread-pool-full alarm, including the
// quick-kill commands when permitted.
func (s *ThreadPoolFull) reportAlarm(pct, threshold int) {
	var value string
	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		value = fmt.Sprintf("Gausstop检测到线程池满，当前线程池使用率：%d%%，线程池满阈值：%d%%，使用以下命令快速查杀idle状态的会话（将[LIMIT]替换成需要查杀的最大会话数量）：%s，如果线程池仍然满，则查杀idle in transaction状态的会话：%s",
			pct, threshold, threadPoolIdleCmd, threadPoolIdleInXactCmd)
	} else {
		value = fmt.Sprintf("Gausstop检测到线程池满，当前线程池使用率：%d%%，线程池满阈值：%d%%，查杀idle状态的会话，如果线程池仍然满，则查杀idle in transaction状态的会话",
			pct, threshold)
	}
	s.deps.Alarm.CheckAndReport(s.deps.Logger, threadPoolFullName, value, true)
}

// autoTerminate kills none/idle/idle-in-xact sessions when both the scenario
// terminate switch and the global terminate switch are on.
func (s *ThreadPoolFull) autoTerminate() {
	if !s.deps.Cfg.GetBool("emergency.thread_pool_full.terminate", false) ||
		!s.deps.Cfg.GetBool("main.support_terminate", false) {
		return
	}
	s.deps.Logger.Warning("start terminate sessions because of thread pool full")
	s.terminateNoneSessions()
	s.terminateIdleSessions()
	s.terminateIdleInXactSessions()
}

// HandleCommand runs the terminate sub-menu for the selected line: kill by state,
// or the top-X active sessions of the selected SQL id.
func (s *ThreadPoolFull) HandleCommand(cmd *Command, line string) {
	choice := cmd.ShowMenu([]string{"Terminate sessions: [1] None  [2] idle  [3] idle in xact  [4] top X active of selected SQL id  [*] Quit"})
	if choice == '1' || choice == '2' || choice == '3' || choice == '4' {
		if !cmd.Confirm() {
			return
		}
	}
	switch choice {
	case '1':
		s.terminateNoneSessions()
	case '2':
		s.terminateIdleSessions()
	case '3':
		s.terminateIdleInXactSessions()
	case '4':
		s.terminateSelectedSQL(cmd, line)
	}
}

// terminateSelectedSQL extracts the SQL id from the selected display line and
// terminates up to the requested number of its sessions.
func (s *ThreadPoolFull) terminateSelectedSQL(cmd *Command, line string) {
	m := leadingInt.FindStringSubmatch(line)
	if m == nil {
		s.deps.Logger.Warning("unable to extract sql id, text: %s", line)
		return
	}
	if n := cmd.InputNumber(); n > 0 {
		s.terminateLimitedSessions(m[1], n)
	}
}

// tpoolSortByIdle flattens the aggregates in insertion order, then stably sorts
// them by idle count descending, matching sorted(..., key=item[1][0], reverse=True).
func tpoolSortByIdle(agg map[int64]*tpoolSQLAgg, order []int64) []tpoolSQLAgg {
	out := make([]tpoolSQLAgg, 0, len(order))
	for _, id := range order {
		out = append(out, *agg[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].idleNum > out[j].idleNum
	})
	return out
}

// tpoolParsePct pulls the leading percentage out of a THREADPOOL cell like
// '50%', returning the truncated integer, matching int(float(data.split('%')[0])).
func tpoolParsePct(data string) (int, bool) {
	part := data
	if i := strings.IndexByte(data, '%'); i >= 0 {
		part = data[:i]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
	if err != nil {
		return 0, false
	}
	return int(f), true
}

// tpoolWhitelist splits a comma-separated whitelist config value, returning nil
// when empty, matching the Python `if len(v) > 0: v.split(',')`.
func tpoolWhitelist(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}
