package emergency

import (
	"fmt"
	"regexp"
	"strings"
)

// pcTableRe extracts the target table for a plan-change ANALYZE suggestion.
var pcTableRe = regexp.MustCompile(`(?i)(?:FROM|UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+([\w\.]+)`)

// pcSQLIDRe pulls the SQL id out of a selected "SQL_ID: <n>" display line.
var pcSQLIDRe = regexp.MustCompile(`SQL_ID:\s+(\w+)`)

// snapshotHeaderLine is the column header for the Prev/Curr comparison.
const snapshotHeaderLine = "Snapshot:  ID       TIMESTAMP            SQL_ACS  INS_ACS  PCT   LATENCY      SQL_CPU      SQL_QPS   INS_CPU%"

// triggerEmergency renders the regression comparison and reports the alarm. Port
// of plan_change.trigger_emergency.
func (s *PlanChange) triggerEmergency(ins InsInfo, sql SQLInfo, snap SQLInfo, hasTriggered bool) {
	s.Trigger()
	sqlText := s.findSQLText(snap.UniqueSQLID)
	s.recordPlanChangeEvent(ins.SnapTS, sql, snap, sqlText)

	insSnap, ok := s.deps.Persist.GetInsInfoSnap(snap.DBID, snap.SnapID)
	if !ok {
		s.deps.Logger.Error("Query ins snap failed, db_id: %d, snap_id: %d", snap.DBID, snap.SnapID)
		return
	}

	analyzeCmd := planAnalyzeCommand(sqlText)
	sqlText = firstLine(sqlText)

	s.appendComparison(ins, sql, snap, insSnap, sqlText, analyzeCmd)
	s.reportAlarm(snap.UniqueSQLID, sqlText, analyzeCmd)
}

// appendComparison writes the SQL header and the Prev/Curr statistic rows.
func (s *PlanChange) appendComparison(ins InsInfo, sql, snap SQLInfo, insSnap InsInfo, sqlText, analyzeCmd string) {
	if analyzeCmd != "" {
		s.AddInfo(fmt.Sprintf("SQL_ID: %d    ANALYZE_CMD: '%s'    SQL_TEXT: %s", snap.UniqueSQLID, analyzeCmd, sqlText))
	} else {
		s.AddInfo(fmt.Sprintf("SQL_ID: %d    SQL_TEXT: %s", snap.UniqueSQLID, sqlText))
	}
	s.AddInfo(snapshotHeaderLine)
	s.AddInfo(s.comparisonRow("Prev", snap.SnapID, snap.SnapTS.Format("2006-01-02 15:04:05"),
		snap.SQLAcsCnt, insSnap.InsAcsCnt, snap.SQLLatency, snap.SQLCPUTime, snap.SQLQPS, insSnap.InsCPUUtl))
	s.AddInfo(s.comparisonRow("Curr", ins.SnapID, s.lastSnapTS.Format("2006-01-02 15:04:05"),
		sql.SQLAcsCnt, ins.InsAcsCnt, sql.SQLLatency, sql.SQLCPUTime, sql.SQLQPS, ins.InsCPUUtl))
}

// comparisonRow formats one Prev/Curr line with the same column widths as the
// original f-string.
func (s *PlanChange) comparisonRow(label string, snapID int, ts string, sqlAcs, insAcs int, latency, cpu, qps, insCPU float64) string {
	pct := "0.0"
	if insAcs != 0 {
		pct = pyFloat(round2(float64(sqlAcs) / float64(insAcs)))
	}
	return fmt.Sprintf("  %s ->  %-7d  %-19s  %-7d  %-7d  %-4s  %-11s  %-11s  %-8s  %-7s",
		label, snapID, ts, sqlAcs, insAcs, pct,
		pyFloat(latency), pyFloat(cpu), pyFloat(qps), pyFloat(insCPU))
}

// reportAlarm appends the quick-kill command (when permitted) and reports the
// plan-change alarm.
func (s *PlanChange) reportAlarm(uniqueSQLID int64, sqlText, analyzeCmd string) {
	if !s.notificationsAreEnabled() {
		return
	}
	terminateCmd := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE unique_sql_id = %d;", uniqueSQLID)
	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		s.AddInfo("")
		s.AppendSplitString(terminateCmd, "CMD")
	}

	key := fmt.Sprintf("%s_%d", planChangeName, uniqueSQLID)
	var value string
	if s.deps.Cfg.GetBool("main.support_emergency_command", false) {
		value = fmt.Sprintf("Gausstop检测到执行计划跳变，发生执行计划跳变的SQL id：%d，SQL语句：%s，使用以下命令更新统计数据：%s，快速查杀计划跳变会话：%s",
			uniqueSQLID, sqlText, analyzeCmd, terminateCmd)
	} else {
		value = fmt.Sprintf("Gausstop检测到执行计划跳变，发生执行计划跳变的SQL id：%d，SQL语句：%s，使用以下命令更新统计数据：%s",
			uniqueSQLID, sqlText, analyzeCmd)
	}
	s.deps.Alarm.CheckAndReport(s.deps.Logger, key, value, true)
}

// findSQLText returns the query text of the first session matching uniqueSQLID.
func (s *PlanChange) findSQLText(uniqueSQLID int64) string {
	for _, row := range s.snap.Session {
		if id, ok := row.Int(4); ok && id == uniqueSQLID {
			return row.Str(5)
		}
	}
	return ""
}

// HandleCommand terminates up to a user-supplied count of the selected SQL's
// sessions. Port of plan_change.handle_emergency_command.
func (s *PlanChange) HandleCommand(cmd *Command, line string) {
	m := pcSQLIDRe.FindStringSubmatch(line)
	if m == nil {
		s.deps.Logger.Warning("unable to extract sql id, text: %s", line)
		return
	}
	sqlID := m[1]
	id, ok := parseInt64(sqlID)
	if !ok || !containsID(s.sqlIDs, id) {
		return
	}
	if !cmd.Confirm() {
		return
	}
	if n := cmd.InputNumber(); n > 0 {
		s.terminateLimitedSessions(sqlID, n)
	}
}

func planAnalyzeCommand(sqlText string) string {
	if m := pcTableRe.FindStringSubmatch(sqlText); m != nil {
		return "ANALYZE " + m[1] + ";"
	}
	return ""
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
