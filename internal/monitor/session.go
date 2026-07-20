package monitor

import (
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

const (
	sessionName   = "session"
	sessionHeight = 31
	sessionConfig = "session.cfg"
)

// Lock status codes for the BLK column.
const (
	lockHolder       = "H"
	lockWaiter       = "W"
	lockHolderWaiter = "H&W"
)

// blkPriority orders blocked/blocking sessions to the top; everything else sinks.
var blkPriority = map[string]int{lockHolder: 1, lockHolderWaiter: 2, lockWaiter: 3}

// SessionMonitor renders per-session detail and drives the session sub-view
// (selection, sorting, termination, detail panel). Port of monitor/session.py.
type SessionMonitor struct {
	base

	items  []string
	widths []int

	values          []model.SessionRow
	currSessResult  []dbconn.Row
	currPrintLoc    int
	currPadLength   int
	currOrderByCol  int // -1 == unsorted; else column index
	emergencySQLIDs []int64
	emergencyPIDs   []int64
	cursorY         int // app-tracked selection row; -1 when not selecting
}

// NewSessionMonitor builds the session panel.
func NewSessionMonitor(deps Deps) *SessionMonitor {
	return &SessionMonitor{
		base:           newBase(sessionName, sessionHeight, deps),
		currOrderByCol: -1,
		cursorY:        -1,
	}
}

// Init lays out the panel and parses session.cfg.
func (m *SessionMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse session.cfg failed: %v", err)
	}
}

func (m *SessionMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, sessionConfig))
	if err != nil {
		return err
	}
	for _, line := range lines {
		f := strings.SplitN(line, ":", 2)
		if len(f) < 2 {
			continue
		}
		m.items = append(m.items, f[0])
		m.widths = append(m.widths, atoiOrZero(f[1]))
	}
	return nil
}

// Session returns the last raw session result, used by the emergency subsystem.
func (m *SessionMonitor) Session() []dbconn.Row {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currSessResult
}

// SetEmergencySQLIDs / SetEmergencyPIDs receive highlight targets from the
// emergency modules (plan-change SQL ids, cpu/io/threadpool pids).
func (m *SessionMonitor) SetEmergencySQLIDs(ids []int64) {
	m.mu.Lock()
	m.emergencySQLIDs = ids
	m.mu.Unlock()
}

func (m *SessionMonitor) SetEmergencyPIDs(pids []int64) {
	m.mu.Lock()
	m.emergencyPIDs = pids
	m.mu.Unlock()
}

// SetCursor records the app's current selection row so Draw can highlight it.
func (m *SessionMonitor) SetCursor(y int) { m.cursorY = y }

// Refresh runs the memory, soft-parse, and session queries and rebuilds the rows.
func (m *SessionMonitor) Refresh() {
	var memResult []dbconn.Row
	if m.deps.Cfg.GetBool("main.dynamic_mem_enable", false) {
		memResult = m.deps.DB.Query(sessionMemQuery)
		if memResult == nil {
			m.deps.Logger.Error("Exec session memory query failed.")
			return
		}
	}

	statementResult := m.deps.DB.Query(sessionStatementQuery)
	if statementResult == nil {
		m.deps.Logger.Error("Exec session statement query failed.")
		return
	}

	sessResult := m.deps.DB.Query(sessionQueryFor(m.deps.DB.Kind()))
	if sessResult == nil {
		m.deps.Logger.Error("Exec session query failed.")
		return
	}

	m.handleSQLResult(memResult, statementResult, sessResult)
}

// handleSQLResult assembles the 24-element rows, runs block analysis, and sorts.
func (m *SessionMonitor) handleSQLResult(memResult, statementResult, sessResult []dbconn.Row) {
	m.mu.Lock()
	m.currSessResult = sessResult
	m.mu.Unlock()

	memDict := buildMemDict(memResult)
	stmtDict := buildStatementDict(statementResult)

	var rows []model.SessionRow
	var blockers []any
	for _, row := range sessResult {
		mvpl, blocker := buildSessionRow(row, memDict, stmtDict)
		if blocker != nil {
			blockers = append(blockers, blocker)
		}
		rows = append(rows, mvpl)
	}

	if len(blockers) != 0 {
		rows = m.analyzeBlockStatus(rows, blockers)
	}
	m.sortSession(rows)

	m.mu.Lock()
	m.values = rows
	m.mu.Unlock()
}

// buildMemDict maps session id text to its memory total (MB), rounded.
func buildMemDict(memResult []dbconn.Row) map[string]float64 {
	out := map[string]float64{}
	for _, row := range memResult {
		if v, ok := row.Float(1); ok {
			out[row.Str(0)] = round2(v)
		}
	}
	return out
}

// buildStatementDict maps unique_sql_id to its soft-parse rate (%).
func buildStatementDict(statementResult []dbconn.Row) map[int64]float64 {
	out := map[int64]float64{}
	for _, row := range statementResult {
		id, _ := row.Int(0)
		calls, _ := row.Int(1)
		soft, _ := row.Int(2)
		if calls != 0 {
			out[id] = round2(float64(soft) / float64(calls) * 100)
		} else {
			out[id] = 0
		}
	}
	return out
}

// buildSessionRow maps a raw session query row to the 24-element display row and
// returns the blocker value (nil when the BLOCKER column was SQL NULL).
func buildSessionRow(row dbconn.Row, memDict map[string]float64, stmtDict map[int64]float64) (model.SessionRow, any) {
	mvpl := make(model.SessionRow, 0, model.SessionRowLen)
	var blocker any
	for colID := 0; colID < len(row); colID++ {
		cv := row.Col(colID)
		switch colID {
		case 3: // PGA: look up memory by session id text
			mvpl = append(mvpl, memLookup(memDict, cv))
		case 5: // SQL: first line only
			mvpl = append(mvpl, sFirstLine(cv))
		case 7: // BLOCKER
			blocker = cv
			mvpl = append(mvpl, cv)
		case 8: // E/T: microseconds -> milliseconds
			mvpl = append(mvpl, elapsedMS(cv))
		case 11: // EVENT, then the derived SParse, BLK, session id
			mvpl = append(mvpl, cv)
			id, _ := row.Int(4)
			mvpl = append(mvpl, stmtLookup(stmtDict, id))
			mvpl = append(mvpl, "")
			mvpl = append(mvpl, row.Col(3))
		default:
			mvpl = append(mvpl, cv)
		}
	}
	return mvpl, blocker
}

func memLookup(memDict map[string]float64, sessionID any) any {
	if v, ok := memDict[model.DisplayValue(sessionID)]; ok {
		return v
	}
	return 0
}

func stmtLookup(stmtDict map[int64]float64, id int64) any {
	if v, ok := stmtDict[id]; ok {
		return v
	}
	return 0
}

// sFirstLine returns the first line of a query value, or "" when empty/null.
func sFirstLine(v any) string {
	s := model.DisplayValue(v)
	if v == nil || s == "" {
		return ""
	}
	return strings.SplitN(s, "\n", 2)[0]
}

// elapsedMS converts an E/T microsecond value to milliseconds rounded to 2 dp,
// or float 0 when the source was null/empty.
func elapsedMS(v any) any {
	if v == nil {
		return float64(0)
	}
	if f, ok := sFloat(v); ok {
		return round2(f / 1000)
	}
	return float64(0)
}

// Draw renders the header and the visible window of session rows.
func (m *SessionMonitor) Draw(screen tcell.Screen) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pad == nil {
		return
	}
	m.pad.Clear()
	m.drawHeader()
	m.drawRows()
	m.blit(screen)
}

func (m *SessionMonitor) drawHeader() {
	header := model.Style{Pair: model.PairReverse}
	x := 0
	for i, item := range m.items {
		m.pad.AddStr(0, x, item, header)
		x += m.widths[i]
	}
}

func (m *SessionMonitor) drawRows() {
	m.currPadLength = 0
	y := 1
	for i := m.currPrintLoc; i < len(m.values); i++ {
		if y-1 == sessionHeight-1 {
			break
		}
		row := m.values[i]
		selected := m.cursorY >= 0 && m.beginY+y == m.cursorY
		style := rowStyle(row, m.emergencySQLIDs, m.emergencyPIDs, selected)
		if selected {
			m.pad.AddStr(y, 0, strings.Repeat(" ", m.width-1), model.Style{Pair: model.PairReverse})
		}
		x := 0
		for idx := range m.items {
			cell := sTruncate(row.Display(idx), m.widths[idx]-1)
			m.pad.AddStr(y, x, cell, style)
			x += m.widths[idx]
		}
		y++
		m.currPadLength++
	}
}

// rowStyle picks the row color from its block status and emergency membership,
// adding reverse video when selected. Emergency SQL ids take precedence over pids.
func rowStyle(row model.SessionRow, sqlIDs, pids []int64, selected bool) model.Style {
	style := model.Normal
	switch row.Display(model.SIdxBLK) {
	case lockWaiter:
		style = model.Style{Pair: model.PairGreen, Bold: true}
	case lockHolder:
		style = model.Style{Pair: model.PairAlarmRed, Bold: true}
	case lockHolderWaiter:
		style = model.Style{Pair: model.PairCyan, Bold: true}
	}

	if len(sqlIDs) != 0 {
		if id, ok := sInt64(row.Get(model.SIdxSQLID)); ok && containsInt64(sqlIDs, id) {
			style = model.Style{Pair: model.PairConfirmYellow, Bold: true}
		}
	} else if len(pids) != 0 {
		if pid, ok := sInt64(row.Get(model.SIdxPID)); ok && containsInt64(pids, pid) {
			style = model.Style{Pair: model.PairConfirmYellow, Bold: true}
		}
	}

	if selected {
		style.Reverse = true
	}
	return style
}

// sortSession applies the optional column sort then the stable BLK-priority sort
// that keeps lock holders/waiters pinned to the top.
func (m *SessionMonitor) sortSession(rows []model.SessionRow) {
	if m.currOrderByCol >= 0 {
		col := m.currOrderByCol
		sort.SliceStable(rows, func(a, b int) bool {
			return sortKeyGreater(rows[a].Get(col), rows[b].Get(col))
		})
	}
	sort.SliceStable(rows, func(a, b int) bool {
		return blkRank(rows[a]) < blkRank(rows[b])
	})
}

func blkRank(row model.SessionRow) int {
	if p, ok := blkPriority[row.Display(model.SIdxBLK)]; ok {
		return p
	}
	return 999
}

// RefreshByPGA / RefreshByElapsedTime / RefreshByEvent re-sort by a column.
func (m *SessionMonitor) RefreshByPGA()         { m.reorder(model.SIdxPGA) }
func (m *SessionMonitor) RefreshByElapsedTime() { m.reorder(model.SIdxElapsedMS) }
func (m *SessionMonitor) RefreshByEvent()       { m.reorder(model.SIdxEvent) }

func (m *SessionMonitor) reorder(col int) {
	m.currOrderByCol = col
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sortSession(m.values)
}

// ResetPrintLocation returns the view to the top of the list.
func (m *SessionMonitor) ResetPrintLocation() { m.currPrintLoc = 0 }

// PadLength reports how many rows the last Draw actually rendered.
func (m *SessionMonitor) PadLength() int { return m.currPadLength }

// SelectedIndex maps the app cursor row to an index into values, or -1.
func (m *SessionMonitor) SelectedIndex(cursorY int) int {
	return cursorY - m.beginY - 1 + m.currPrintLoc
}

// CheckHighlightLocation scrolls the view by step rows in the given direction,
// returning false when the move would go out of bounds. Port of
// check_highlight_location.
func (m *SessionMonitor) CheckHighlightLocation(direction, step int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	target := m.currPrintLoc + direction*step
	if (direction == -1 && m.currPrintLoc == 0) || target >= len(m.values) {
		return false
	}
	if target < 0 {
		target = 0
	}
	m.currPrintLoc = target
	return true
}
