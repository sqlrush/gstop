package monitor

import (
	"fmt"
	"strings"

	"gstop/internal/dbcompat"
	"gstop/internal/model"
	"gstop/internal/tui"
)

// detailsExtraRows is how much taller the details pad is than the session pad,
// borrowing the emergency window area (self.height + 20 in the original).
const detailsExtraRows = 20

// lineWriter accumulates lines into a pad, tracking the next row and stopping at
// the pad height, reproducing the print_string_to_pad closure.
type lineWriter struct {
	pad    *tui.Pad
	y      int
	height int
}

// line writes text at the current row and advances. A prefix (used for block-tree
// branches) is drawn first, then the text one column past it.
func (w *lineWriter) line(text string, style model.Style) {
	w.prefixed("", text, style)
}

func (w *lineWriter) prefixed(prefix, text string, style model.Style) {
	if w.y >= w.height {
		return
	}
	x := 0
	if prefix != "" {
		w.pad.AddStr(w.y, 0, prefix, model.Normal)
		x = len(prefix) + 1
	}
	if text != "" {
		w.pad.AddStr(w.y, x, text, style)
	}
	w.y++
}

// PrintMoreDetails renders the session detail panel for the selected row and runs
// its command menu until the user quits. Port of session.print_more_details.
func (m *SessionMonitor) PrintMoreDetails(screen *tui.Screen, cursorY int) {
	idx := m.SelectedIndex(cursorY)
	if idx < 0 || idx >= len(m.values) {
		return
	}
	pad := tui.NewPad(m.height+detailsExtraRows, m.width)
	for {
		row := m.values[idx]
		w := &lineWriter{pad: pad, y: 0, height: m.height + detailsExtraRows}
		pad.Clear()

		m.drawDetailHeader(w, row)
		sessionPID := row.Get(model.SIdxPID)
		_, lockHolder, sqlIDs := m.printBlockedTree(w, sessionPID)
		if lockHolder == "" {
			w.line("", model.Normal)
		}
		m.drawDetailMenu(w, lockHolder)

		pad.Blit(screen.Raw(), m.beginX, m.beginY)
		screen.Show()

		key, ok := screen.GetKey(tui.BlockForever)
		if !ok {
			continue
		}
		screen.FlushInput()
		if m.handleDetailKey(screen, key, row, sessionPID, sqlIDs, lockHolder, w) {
			return
		}
	}
}

// drawDetailHeader renders the session's descriptive fields.
func (m *SessionMonitor) drawDetailHeader(w *lineWriter, row model.SessionRow) {
	header := "SESSION DETAILS"
	w.line(header+strings.Repeat(" ", max0(m.width-1-len(header))), model.Style{Pair: model.PairReverse})
	w.line(fmt.Sprintf("DATABASE: %v    USERNAME: %v    CLIENT_ADDR: %v    CLIENT_PORT: %v    CLIENT_HOSTNAME: %v",
		row.Display(model.SIdxDatabase), row.Display(model.SIdxUser), row.Display(model.SIdxClientAddr),
		row.Display(model.SIdxClientPort), row.Display(model.SIdxClientHost)), model.Normal)
	w.line(fmt.Sprintf("SESSION_ID: %v    QUERY_START: %v    XACT_START: %v",
		row.Display(model.SIdxSessionID), row.Display(model.SIdxQueryStart), row.Display(model.SIdxXactStart)), model.Normal)
	sqlText := sTruncate(row.Display(model.SIdxSQL), 110)
	w.line(fmt.Sprintf("SQL_ID: %v    SQL_TEXT: %v", row.Display(model.SIdxSQLID), sqlText), model.Normal)
	w.line("", model.Normal)
}

// drawDetailMenu renders the support-command menu, gated on the terminate switch
// and whether a lock holder was found.
func (m *SessionMonitor) drawDetailMenu(w *lineWriter, lockHolder string) {
	support := m.deps.Cfg.GetBool("main.support_terminate", false)
	w.line("[SUPPORT COMMANDS]", model.Normal)
	w.line("  [1] Print the full SQL text", model.Normal)
	w.line("  [2] Print the execution plan", model.Normal)
	if support {
		w.line("  [3] Terminate single selected session", model.Normal)
		w.line("  [4] Terminate part of sessions with same SQL id", model.Normal)
		w.line("  [5] Terminate all sessions with same SQL id", model.Normal)
	}
	if lockHolder != "" {
		if support {
			w.line(fmt.Sprintf("  [6] Terminate the blocker session '%s'", lockHolder), model.Normal)
		}
		w.line("  [7] Print the full SQL text of block tree", model.Normal)
	}
	w.line("  [*] Quit", model.Normal)
	w.line("", model.Normal)
}

// handleDetailKey applies one menu keypress; returns true to leave the panel.
func (m *SessionMonitor) handleDetailKey(screen *tui.Screen, key tui.Key, row model.SessionRow, sessionPID any, sqlIDs []any, lockHolder string, w *lineWriter) bool {
	support := m.deps.Cfg.GetBool("main.support_terminate", false)
	sessionID := row.Get(model.SIdxSessionID)
	sqlID, _ := sInt64(row.Get(model.SIdxSQLID))
	confirmY := m.beginY + w.y

	switch {
	case key.IsRune('1'):
		m.printDetailSQLText(screen, sessionID)
	case key.IsRune('2'):
		m.printExecutePlan(screen, sessionPID, sqlID)
	case key.IsRune('3') && support:
		if tui.TerminateConfirmPassed(screen, 0, confirmY) {
			m.terminateSession(sessionPID, sessionID)
		}
	case key.IsRune('4') && support:
		m.terminatePartSameSQLID(screen, sqlID, confirmY)
	case key.IsRune('5') && support:
		m.terminateAllSameSQLID(screen, sqlID, confirmY)
	case key.IsRune('6') && support && lockHolder != "":
		if tui.TerminateConfirmPassed(screen, 0, confirmY) {
			m.terminateBlockerSession(sessionPID, lockHolder)
		}
	case key.IsRune('7') && lockHolder != "":
		m.printBlockTreeSQL(screen, sqlIDs, confirmY)
	default:
		return true
	}
	return false
}

// terminatePartSameSQLID kills up to a user-supplied number of sessions sharing
// the SQL id.
func (m *SessionMonitor) terminatePartSameSQLID(screen *tui.Screen, sqlID int64, confirmY int) {
	if sqlID == 0 {
		m.deps.Logger.Error("Cannot terminate the sessions with SQL ID is 0")
		return
	}
	if !tui.TerminateConfirmPassed(screen, 0, confirmY) {
		return
	}
	n := tui.GetInputNumber(screen, 44, confirmY)
	if n <= 0 {
		return
	}
	block, err := model.TerminateLimitedSessions(fmtInt64(sqlID), n)
	if err != nil {
		m.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	m.deps.DB.NoReturn(block)
}

// terminateAllSameSQLID kills every session sharing the SQL id.
func (m *SessionMonitor) terminateAllSameSQLID(screen *tui.Screen, sqlID int64, confirmY int) {
	if sqlID == 0 {
		m.deps.Logger.Error("Cannot terminate the sessions with SQL ID is 0")
		return
	}
	if !tui.TerminateConfirmPassed(screen, 0, confirmY) {
		return
	}
	block, err := model.TerminateUnlimitedSessions(fmtInt64(sqlID))
	if err != nil {
		m.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	m.deps.DB.NoReturn(block)
}

// printBlockTreeSQL prompts for a block-tree entry number and prints that
// session's full SQL text.
func (m *SessionMonitor) printBlockTreeSQL(screen *tui.Screen, sqlIDs []any, confirmY int) {
	n := tui.GetInputNumber(screen, 22, confirmY)
	if n >= 0 && n < len(sqlIDs) {
		m.printDetailSQLText(screen, sqlIDs[n])
	}
}

// printExecutePlan shows the runtime and history execution plans in an overlay.
// Port of print_execute_plan.
func (m *SessionMonitor) printExecutePlan(screen *tui.Screen, sessionPID any, sqlID int64) {
	pad := tui.NewPad(m.height+detailsExtraRows, m.width)
	w := &lineWriter{pad: pad, y: 0, height: m.height + detailsExtraRows}
	header := "SESSION DETAILS"
	w.line(header+strings.Repeat(" ", max0(m.width-1-len(header))), model.Style{Pair: model.PairReverse})

	w.line("[THE RUNTIME PLAN]", model.Normal)
	m.appendRuntimePlan(w, sessionPID)
	w.line("[THE STATEMENT HISTORY PLAN]", model.Normal)
	if sqlID != 0 {
		q := fmt.Sprintf("SELECT query_plan FROM dbe_perf.statement_history WHERE unique_query_id = '%d' ORDER BY start_time DESC LIMIT 1;", sqlID)
		m.appendPlan(w, q, "No recorded query plan in dbe_perf.statement_history.")
	}

	w.line("", model.Normal)
	w.line("Press any key to continue...", model.Normal)
	pad.Blit(screen.Raw(), m.beginX, m.beginY)
	screen.Show()
	screen.GetKey(tui.BlockForever)
	screen.FlushInput()
}

// appendRuntimePlan writes the live runtime plan. On GaussDB it uses
// gs_get_explain(pid) (the actual plan of the running thread). openGauss has no
// such function, so it falls back to EXPLAIN of the session's current SQL — a
// re-planned estimate, not the exact running instance — matching the
// self-adaptive routing in internal/dbcompat.
func (m *SessionMonitor) appendRuntimePlan(w *lineWriter, sessionPID any) {
	if dbcompat.SupportsGsGetExplain(m.deps.DB.Kind()) {
		if pid, ok := sInt64(sessionPID); ok && pid != 0 {
			m.appendPlan(w, fmt.Sprintf("SELECT * FROM gs_get_explain(%d);", pid), "No recorded query plan in gs_get_explain().")
		}
		return
	}
	w.line("(gs_get_explain is GaussDB-only; showing an EXPLAIN estimate of the current SQL)", model.Normal)
	query := m.fullQueryByPID(sessionPID)
	if strings.TrimSpace(query) == "" {
		w.line("No active SQL to explain.", model.Normal)
		return
	}
	m.appendExplain(w, query)
}

// appendExplain runs EXPLAIN (plan only, no execution — read-only safe) on the
// query and writes up to 20 plan lines, degrading gracefully when the statement
// cannot be planned standalone (e.g. it uses bind parameters).
func (m *SessionMonitor) appendExplain(w *lineWriter, query string) {
	stmt := strings.TrimRight(strings.TrimSpace(query), ";")
	rows := m.deps.DB.Query("EXPLAIN " + stmt)
	if rows == nil {
		w.line("EXPLAIN failed (the statement may use bind parameters or be a utility command).", model.Normal)
		return
	}
	if len(rows) == 0 {
		w.line("No plan returned.", model.Normal)
		return
	}
	for i, row := range rows {
		if i >= 20 {
			break
		}
		w.line(row.Str(0), model.Normal)
	}
}

// fullQueryByPID returns the full (untruncated) query text of the session with
// the given pid from the raw session result.
func (m *SessionMonitor) fullQueryByPID(pid any) string {
	target, ok := sInt64(pid)
	if !ok {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.currSessResult {
		if p, ok := row.Int(0); ok && p == target {
			return row.Str(5)
		}
	}
	return ""
}

// appendPlan runs a plan query and writes up to 20 non-empty lines, or a fallback.
func (m *SessionMonitor) appendPlan(w *lineWriter, query, fallback string) {
	rows := m.deps.DB.Query(query)
	if len(rows) == 0 || rows[0].Str(0) == "" {
		m.deps.Logger.Error("Exec query %s failed.", query)
		w.line(fallback, model.Normal)
		return
	}
	lines := strings.Split(rows[0].Str(0), "\n")
	for i, line := range lines {
		if i >= 20 {
			break
		}
		if line != "" {
			w.line(line, model.Normal)
		}
	}
}

// printDetailSQLText shows a session's full SQL text in an overlay. Port of
// print_detail_sql_text.
func (m *SessionMonitor) printDetailSQLText(screen *tui.Screen, sessionID any) {
	pad := tui.NewPad(m.height+detailsExtraRows, m.width)
	w := &lineWriter{pad: pad, y: 0, height: m.height + detailsExtraRows}
	header := "SESSION DETAILS"
	w.line(header+strings.Repeat(" ", max0(m.width-1-len(header))), model.Style{Pair: model.PairReverse})
	w.line("[THE SQL TEXT]", model.Normal)

	if text := m.sqlFullTextBySessionID(sessionID); text != "" {
		for _, line := range strings.Split(text, "\n") {
			w.line(line, model.Normal)
		}
	}
	w.line("", model.Normal)
	w.line("Press any key to continue...", model.Normal)
	pad.Blit(screen.Raw(), m.beginX, m.beginY)
	screen.Show()
	screen.GetKey(tui.BlockForever)
	screen.FlushInput()
}

// sqlFullTextBySessionID returns the full query text for a session id from the
// raw result. Port of get_sql_full_text_by_session_id.
func (m *SessionMonitor) sqlFullTextBySessionID(sessionID any) string {
	target := model.DisplayValue(sessionID)
	for _, row := range m.currSessResult {
		if row.Str(3) == target {
			return row.Str(5)
		}
	}
	return ""
}

// sqlTextByPID returns the displayed SQL for a pid from the current rows. Port of
// get_sql_text_by_pid.
func (m *SessionMonitor) sqlTextByPID(pid any) string {
	target, ok := sInt64(pid)
	if !ok {
		return ""
	}
	for _, row := range m.values {
		if p, ok := sInt64(row.Get(model.SIdxPID)); ok && p == target {
			return row.Display(model.SIdxSQL)
		}
	}
	return ""
}

func fmtInt64(n int64) string { return fmt.Sprintf("%d", n) }

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
