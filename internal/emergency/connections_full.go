package emergency

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gstop/internal/dbconn"
)

const (
	connectionsFullName   = "ConnectionsFull"
	connectionsFullHeader = "[EMER07 - ConnectionsFull] - select the username and press 'k' to terminate sessions"
	// connectionsFullPersist matches the Python FIRST_PERSIST_NUMBER = 120.
	connectionsFullPersist = 120
)

// connSessTableHeader is the per-user session-state table header, copied verbatim
// from connections_full.py so the fixed-width columns line up with the rows.
const connSessTableHeader = "USER                     TOTAL_SESS           IDLE_SESS         IDLE_IN_XACT_SESS       ACTIVE_SESS    NONE_SESS"

// connKillIdleCommand is the operator quick-kill for all idle/idle-in-xact/NULL
// sessions ([LIMIT] is substituted by the operator). Verbatim from the source.
const connKillIdleCommand = "SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE datname != 'postgres' AND (state IS NULL OR state IN ('idle', 'idle in transaction')) LIMIT [LIMIT];"

// connKillDNCommand is the last-resort node-side command that kills the DN
// processes when the database is unreachable. Verbatim from the source.
const connKillDNCommand = "ps -ux | grep gaussdb | grep dn_ | awk '{print $2}' | xargs kill -9"

// connUsernamePattern guards the interactive termination path against SQL
// injection: a username taken from the selected display line may only contain
// letters, digits, underscore, '$' and CJK ideographs. Port of the Python
// r'^[\w\$一-龥]+$'.
var connUsernamePattern = regexp.MustCompile(`^[\w\$\x{4e00}-\x{9fa5}]+$`)

// ConnectionsFull detects a saturated connection pool and surfaces the busiest
// users for termination, optionally auto-killing idle sessions. Port of
// emergency/connections_full.py.
type ConnectionsFull struct {
	*Base
}

// NewConnectionsFull builds the connections-full scenario.
func NewConnectionsFull(deps Deps) *ConnectionsFull {
	return &ConnectionsFull{Base: NewBase(connectionsFullName, connectionsFullHeader, deps, connectionsFullPersist)}
}

// connUserStat aggregates one user's session counts by state. Order mirrors the
// Python list [total, idle, idle_in_xact, active, none].
type connUserStat struct {
	user       string
	total      int
	idle       int
	idleInXact int
	active     int
	none       int
}

// Analyze triggers when the connection-usage percent is at or above the
// threshold, then aggregates per-user session state and optionally auto-kills
// idle sessions.
func (s *ConnectionsFull) Analyze() {
	cell := s.instanceCell(12) // CONNECTION, e.g. '50%(5000/10000)'
	pct, fraction, ok := connParsePercent(cell)
	if !ok {
		s.deps.Logger.Error("invalid connection data for connections full: %q", cell)
		return
	}
	if pct < s.deps.Cfg.GetInt("emergency.connections_full.connections_full_thresh", 90) {
		return
	}
	curConn, maxConn, ok := connParseFraction(fraction)
	if !ok {
		s.deps.Logger.Error("invalid connection fraction for connections full: %q", fraction)
		return
	}

	s.setWhitelist()
	stats := s.analyzeSessionState()
	s.triggerEmergency(pct, curConn, maxConn, stats)
	s.autoTerminate()
}

// connParsePercent splits a CONNECTION cell like '50%(5000/10000)' into the
// integer usage percent and the remaining fraction text '(5000/10000)'.
func connParsePercent(cell string) (pct int, fraction string, ok bool) {
	pctPart, rest, found := strings.Cut(cell, "%")
	if !found {
		return 0, "", false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(pctPart), 64)
	if err != nil {
		return 0, "", false
	}
	return int(f), rest, true
}

// connParseFraction parses the '(CUR/MAX)' fraction into current and maximum
// connection counts.
func connParseFraction(fraction string) (curConn, maxConn int, ok bool) {
	inner := strings.Trim(fraction, "()")
	curStr, maxStr, found := strings.Cut(inner, "/")
	if !found {
		return 0, 0, false
	}
	c, err := strconv.Atoi(strings.TrimSpace(curStr))
	if err != nil {
		return 0, 0, false
	}
	m, err := strconv.Atoi(strings.TrimSpace(maxStr))
	if err != nil {
		return 0, 0, false
	}
	return c, m, true
}

// setWhitelist loads the comma-separated terminate whitelist into the shared
// Base field, leaving it nil when unset. Port of the Python __init__ logic.
func (s *ConnectionsFull) setWhitelist() {
	raw := s.deps.Cfg.GetString("emergency.connections_full.user_whitelist", "")
	if raw == "" {
		return
	}
	s.whitelist = strings.Split(raw, ",")
}

// analyzeSessionState aggregates the full session list by user and returns the
// per-user counts sorted by total sessions descending (stable on ties, matching
// Python's sorted over an insertion-ordered dict).
func (s *ConnectionsFull) analyzeSessionState() []connUserStat {
	agg := map[string]*connUserStat{}
	var order []string
	for _, row := range s.snap.Session {
		user := row.Str(1)
		st := agg[user]
		if st == nil {
			st = &connUserStat{user: user}
			agg[user] = st
			order = append(order, user)
		}
		st.total++
		connCountState(st, row)
	}

	stats := make([]connUserStat, 0, len(order))
	for _, user := range order {
		stats = append(stats, *agg[user])
	}
	sort.SliceStable(stats, func(i, j int) bool {
		return stats[i].total > stats[j].total
	})
	return stats
}

// connCountState increments the per-state counter for one session row. A NULL
// state is counted as "none", matching the Python `state is None` branch.
func connCountState(st *connUserStat, row dbconn.Row) {
	if row.IsNull(9) {
		st.none++
		return
	}
	switch row.Str(9) {
	case "idle":
		st.idle++
	case "idle in transaction":
		st.idleInXact++
	case "active":
		st.active++
	}
}

// triggerEmergency marks the fault, renders the top-3 user table plus the quick
// remediation command, and reports the alarm.
func (s *ConnectionsFull) triggerEmergency(pct, curConn, maxConn int, stats []connUserStat) {
	s.Trigger()
	s.appendSessionTable(stats)

	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		s.AddInfo("")
		s.AppendSplitString(connKillIdleCommand, "CMD")
	}
	s.reportAlarm(pct, curConn, maxConn)
}

// appendSessionTable renders the header and the top-3 busiest users.
func (s *ConnectionsFull) appendSessionTable(stats []connUserStat) {
	s.AddInfo(connSessTableHeader)
	for i, st := range stats {
		if i >= 3 {
			break
		}
		s.AddInfo(fmt.Sprintf("%-24s %-20d %-17d %-23d %-14d %d",
			st.user, st.total, st.idle, st.idleInXact, st.active, st.none))
	}
}

// reportAlarm builds and reports the connections-full alarm, including the
// operator remediation commands when emergency commands are permitted.
func (s *ConnectionsFull) reportAlarm(pct, curConn, maxConn int) {
	threshold := s.deps.Cfg.GetInt("emergency.connections_full.connections_full_thresh", 90)

	var value string
	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		value = fmt.Sprintf("Gausstop检测到连接数满，当前连接数使用率：%d%%，连接数满阈值：%d%%，当前连接数：%d，最大连接数：%d，使用以下命令快速查杀所有空闲会话（将[LIMIT]替换成需要查杀的最大会话数量）：%s，如果无法连接数据库，可以在主节点执行杀DN进程命令：%s",
			pct, threshold, curConn, maxConn, connKillIdleCommand, connKillDNCommand)
	} else {
		value = fmt.Sprintf("Gausstop检测到连接数满，当前连接数使用率：%d%%，连接数满阈值：%d%%，当前连接数：%d，最大连接数：%d，查杀空闲会话来降低连接数，如果无法连接数据库，可以在主节点杀DN进程",
			pct, threshold, curConn, maxConn)
	}
	s.deps.Alarm.CheckAndReport(s.deps.Logger, connectionsFullName, value, true)
}

// autoTerminate kills idle sessions when both the scenario switch and the global
// terminate switch are on. The whitelist is honoured by the Base helpers.
func (s *ConnectionsFull) autoTerminate() {
	if !s.deps.Cfg.GetBool("emergency.connections_full.terminate", false) ||
		!s.deps.Cfg.GetBool("main.support_terminate", false) {
		return
	}
	s.deps.Logger.Warning("start terminate sessions because of connections full")
	s.terminateNoneSessions()
	s.terminateIdleSessions()
	s.terminateIdleInXactSessions()
}

// HandleCommand runs the terminate sub-menu for the selected user line.
func (s *ConnectionsFull) HandleCommand(cmd *Command, line string) {
	username, ok := connSelectedUser(line)
	if !ok {
		return
	}
	if !connUsernamePattern.MatchString(username) {
		s.deps.Logger.Error("Invalid username format for termination: %s", username)
		return
	}

	choice := cmd.ShowMenu([]string{"Terminate sessions: [1] None  [2] idle  [3] idle in xact  [4] top X active of selected user  [*] Quit"})
	if connIsTerminateChoice(choice) && !cmd.Confirm() {
		return
	}
	switch choice {
	case '1':
		s.terminateNoneSessionsWithName(username)
	case '2':
		s.terminateIdleSessionsWithName(username)
	case '3':
		s.terminateIdleInXactSessionsWithName(username)
	case '4':
		if n := cmd.InputNumber(); n > 0 {
			s.terminateTopActiveSessions(username, n)
		}
	}
}

// connSelectedUser extracts the username from the first column of a selected
// display line, rejecting the header row and blank lines. Port of
// `value.split()[0]` plus its guards.
func connSelectedUser(line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", false
	}
	username := fields[0]
	if username == "" || username == "USER" {
		return "", false
	}
	return username, true
}

// connIsTerminateChoice reports whether the menu key requires confirmation.
func connIsTerminateChoice(choice rune) bool {
	return choice == '1' || choice == '2' || choice == '3' || choice == '4'
}

// terminateNoneSessionsWithName kills the NULL-state sessions of one user.
func (s *ConnectionsFull) terminateNoneSessionsWithName(username string) {
	sql := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE state IS NULL AND usename = '%s';", username)
	s.deps.DB.Query(sql)
}

// terminateIdleSessionsWithName kills the idle sessions of one user.
func (s *ConnectionsFull) terminateIdleSessionsWithName(username string) {
	sql := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE state = 'idle' AND usename = '%s';", username)
	s.deps.DB.Query(sql)
}

// terminateIdleInXactSessionsWithName kills the idle-in-transaction sessions of
// one user.
func (s *ConnectionsFull) terminateIdleInXactSessionsWithName(username string) {
	sql := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE state = 'idle in transaction' AND usename = '%s';", username)
	s.deps.DB.Query(sql)
}

// terminateTopActiveSessions kills up to limit of the oldest active sessions of
// one user, ordered by transaction start.
func (s *ConnectionsFull) terminateTopActiveSessions(username string, limit int) {
	sql := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE state = 'active' AND usename = '%s' ORDER BY xact_start LIMIT %d;", username, limit)
	s.deps.DB.Query(sql)
}
