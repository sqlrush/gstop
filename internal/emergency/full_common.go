package emergency

import (
	"fmt"
	"regexp"

	"gstop/internal/dbconn"
)

// leadingInt matches an optional-whitespace-prefixed integer at the start of a
// display line, used to pull the SQL id out of a selected info row.
var leadingInt = regexp.MustCompile(`^\s*(\d+)`)

// topSQLTableHeader is the column header shared by CPU-full and IO-full.
const topSQLTableHeader = "SQL_ID      ACTIVE_SESS  CPU_PCT  IO_PCT  ANALYZE_CMD           SQL_TEXT"

// renderTopSQLTable appends the top-3 hot-SQL rows to the info list and returns
// the top SQL id (for workload-rule / abort-patch suggestions). Shared by the
// CPU-full and IO-full scenarios.
func renderTopSQLTable(b *Base) int64 {
	b.AddInfo(topSQLTableHeader)
	var topID int64
	for i, top := range b.sortedTopSQL {
		if i >= 3 {
			break
		}
		if i == 0 {
			topID = top.SQLID
		}
		cpuPct, ioPct := "0", "0"
		if top.DBTime > 0 {
			cpuPct = pyFloat(round2(top.CPUTime / top.DBTime * 100))
			ioPct = pyFloat(round2(top.IOTime / top.DBTime * 100))
		}
		b.AddInfo(fmt.Sprintf("%-10d  %-11d  %-7s  %-6s  %-20s  %s",
			top.SQLID, top.ActiveNum, cpuPct, ioPct, truncate(top.AnalyzeCmd, 20), truncate(top.Query, 85)))
	}
	return topID
}

// appendRemediationCommands appends the quick-kill, workload-rule, and abort-patch
// commands to the info list when emergency commands are permitted, shared by the
// CPU-full and IO-full scenarios.
func (b *Base) appendRemediationCommands(topSQLID int64) (killCmd, ruleCmd, patchCmd string) {
	sessionIDs := quotedList(b.overtimeSessList)
	killCmd = fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE sessionid IN (%s);", sessionIDs)
	ruleCmd = dbconn.BuildWorkloadRuleCmd(itoa64(topSQLID))
	patchCmd = dbconn.BuildAbortPatchCmd(itoa64(topSQLID))

	if b.deps.Cfg.GetBool("main.support_emergency_command", false) {
		b.AddInfo("")
		b.AppendSplitString(killCmd, "CMD")
		b.AppendSplitString(ruleCmd, "CMD")
		b.AppendSplitString(patchCmd, "CMD")
	}
	return killCmd, ruleCmd, patchCmd
}

// handleFullCommand runs the CPU/IO-full 'k' sub-menu: terminate all or the top-X
// over-threshold active sessions of the selected SQL id.
func handleFullCommand(b *Base, cmd *Command, line string, overtimeThreshMS int) {
	m := leadingInt.FindStringSubmatch(line)
	if m == nil {
		b.deps.Logger.Warning("unable to extract sql id, text: %s", line)
		return
	}
	sqlID := m[1]

	choice := cmd.ShowMenu([]string{"Terminate sessions: [1] all active sessions  [2] top X active sessions  [*] Quit"})
	if (choice == '1' || choice == '2') && !cmd.Confirm() {
		return
	}
	switch choice {
	case '1':
		b.terminateUnlimitedSessionsWithTime(sqlID, overtimeThreshMS)
	case '2':
		if n := cmd.InputNumber(); n > 0 {
			b.terminateLimitedSessionsWithTime(sqlID, n, overtimeThreshMS)
		}
	}
}

// quotedList renders a comma-separated list of single-quoted ids.
func quotedList(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += "'" + id + "'"
	}
	return out
}

// truncate returns the first n runes of s.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func itoa64(n int64) string {
	return fmt.Sprintf("%d", n)
}
