package healthdash

import (
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/model"
	"gstop/internal/tui"
)

// Selection maps one selectable dashboard row to the SQL detail target.
type Selection struct {
	SQLID   int64
	Query   string
	Row     int
	Section string
}

type viewLine struct {
	text  string
	style model.Style
}

// View renders a health Snapshot into a scrollable pad and records which rows
// can open SQL details.
type View struct {
	width      int
	pad        *tui.Pad
	lines      []viewLine
	selections []Selection
}

// NewView builds a dashboard renderer at the monitor's fixed width.
func NewView(width int) *View {
	if width <= 0 {
		width = model.MonitorWidth
	}
	return &View{width: width}
}

// Render rebuilds the off-screen page. selected is an index into SelectableSQL;
// selecting controls whether the selected line is highlighted.
func (v *View) Render(snapshot Snapshot, selected int, selecting bool) {
	v.lines = nil
	v.selections = nil
	v.add("HEALTH DASHBOARD", model.Style{Pair: model.PairReverse, Bold: true})
	v.add(v.refreshStatus(snapshot), model.Normal)
	if snapshot.FastError != "" {
		v.add("FAST ERROR: "+snapshot.FastError, model.Style{Pair: model.PairAlarmRed, Bold: true})
	}
	v.blank()

	v.renderAverage(snapshot.AverageSQL)
	v.renderExecutions(snapshot.ExecutionSQL)
	v.renderMemory(snapshot)
	v.renderPlanChanges(snapshot.PlanChanges)
	v.renderAnalyze(snapshot)
	v.renderIndexes(snapshot)
	v.renderWaits(snapshot)

	v.blank()
	v.add("Keys: q exit | Esc back | arrows scroll | s select SQL | p SQL details/plan | r refresh cross-database checks", model.Style{Pair: model.PairReverse})

	if selecting && selected >= 0 && selected < len(v.selections) {
		row := v.selections[selected].Row
		v.lines[row].style.Reverse = true
	}
	v.pad = tui.NewPad(maxInt(1, len(v.lines)), v.width)
	for row, line := range v.lines {
		v.pad.AddStr(row, 0, line.text, line.style)
	}
}

// RenderDetail builds the SQL detail document with wrapped full SQL and plan
// lines so horizontal clipping never hides content.
func (v *View) RenderDetail(detail Detail) {
	v.lines = nil
	v.selections = nil
	v.add("SQL DETAILS", model.Style{Pair: model.PairReverse, Bold: true})
	v.add(fmt.Sprintf("SQL_ID: %d", detail.SQLID), model.Normal)
	if detail.RunningPID != 0 {
		v.add(fmt.Sprintf("RUNNING_PID: %d", detail.RunningPID), model.Normal)
	}
	v.blank()
	v.section("[THE FULL SQL]")
	if strings.TrimSpace(detail.SQLText) == "" {
		v.add("没有可用的完整SQL文本", model.Normal)
	} else {
		v.addWrapped(detail.SQLText)
	}
	v.blank()
	if detail.PlanSource != "" {
		v.section("[EXECUTION PLAN: " + detail.PlanSource + "]")
	} else {
		v.section("[EXECUTION PLAN]")
	}
	if detail.Error != "" {
		v.addWrapped(detail.Error)
	}
	if len(detail.PlanLines) == 0 && detail.Error == "" {
		v.add("没有可用的执行计划", model.Normal)
	}
	for _, line := range detail.PlanLines {
		v.addWrapped(line)
	}
	v.blank()
	v.add("Keys: q exit | Esc back to health dashboard | arrows scroll", model.Style{Pair: model.PairReverse})
	v.pad = tui.NewPad(maxInt(1, len(v.lines)), v.width)
	for row, line := range v.lines {
		v.pad.AddStr(row, 0, line.text, line.style)
	}
}

func (v *View) refreshStatus(snapshot Snapshot) string {
	return fmt.Sprintf("Started: %s | Fast: %s | Memory: %s | Cross-DB: %s%s",
		formatTimestamp(snapshot.StartedAt), formatTimestamp(snapshot.FastRefreshedAt),
		formatTimestamp(snapshot.MemoryRefreshedAt), formatTimestamp(snapshot.SlowRefreshedAt),
		map[bool]string{true: " (refreshing)", false: ""}[snapshot.SlowRefreshing])
}

func (v *View) renderAverage(rows []SQLMetric) {
	v.section("1. AVG ELAPSED TOP3 SQL")
	v.add("#  SQL_ID              AVG_ELAPSED       CALLS      ACTIVE  SQL", model.Normal)
	if len(rows) == 0 {
		v.add("暂无可用SQL统计", model.Normal)
	}
	for i, row := range rows {
		text := fmt.Sprintf("%-2d %-19d %-17s %-10d %-7d %s", i+1, row.SQLID,
			formatMicroseconds(row.AverageUS), row.Calls, row.ActiveSessions, oneLine(row.Query))
		v.sqlLine("average", row.SQLID, row.Query, text)
	}
	v.blank()
}

func (v *View) renderExecutions(rows []SQLMetric) {
	v.section("2. EXECUTIONS SINCE GSTOP TOP3 SQL")
	v.add("#  SQL_ID              EXEC_DELTA  ACTIVE  SQL", model.Normal)
	if len(rows) == 0 {
		v.add("启动后暂无已完成执行增量", model.Normal)
	}
	for i, row := range rows {
		text := fmt.Sprintf("%-2d %-19d %-11d %-7d %s", i+1, row.SQLID, row.CallsDelta, row.ActiveSessions, oneLine(row.Query))
		v.sqlLine("executions", row.SQLID, row.Query, text)
	}
	v.blank()
}

func (v *View) renderMemory(snapshot Snapshot) {
	v.section("3. ACTIVE SQL DYNAMIC MEMORY TOP3")
	if !snapshot.MemoryEnabled {
		v.add("动态内存采集未启用（检查 main.dynamic_mem_enable / main.mem_interval）", model.Style{Pair: model.PairConfirmYellow})
		v.blank()
		return
	}
	if snapshot.MemoryError != "" {
		v.add("MEMORY ERROR: "+snapshot.MemoryError, model.Style{Pair: model.PairAlarmRed, Bold: true})
	}
	v.add("#  SQL_ID              SESSIONS  TOTAL_MB      MAX_SESSION_MB  SQL", model.Normal)
	if len(snapshot.MemorySQL) == 0 {
		v.add("当前无活跃SQL动态内存数据", model.Normal)
	}
	for i, row := range snapshot.MemorySQL {
		text := fmt.Sprintf("%-2d %-19d %-9d %-13.2f %-15.2f %s", i+1, row.SQLID,
			row.ActiveSessions, row.TotalMemoryMB, row.MaxMemoryMB, oneLine(row.Query))
		v.sqlLine("memory", row.SQLID, row.Query, text)
	}
	v.blank()
}

func (v *View) renderPlanChanges(events []PlanChangeEvent) {
	v.section("4. PLAN CHANGES SINCE GSTOP")
	v.add("STATUS     SQL_ID              FIRST_SEEN          LAST_SEEN           ACS(prev/curr)  LATENCY(prev/curr)  SQL", model.Normal)
	if len(events) == 0 {
		v.add("本次gstop启动后未检测到计划跳变", model.Normal)
	}
	for _, event := range events {
		status := "ACTIVE"
		if event.Recovered {
			status = "RECOVERED"
		}
		text := fmt.Sprintf("%-10s %-19d %-19s %-19s %d/%-11d %s/%-16s %s",
			status, event.SQLID, formatTimestamp(event.FirstSeen), formatTimestamp(event.LastSeen),
			event.PreviousAcs, event.CurrentAcs, formatMicroseconds(event.PreviousLatUS),
			formatMicroseconds(event.CurrentLatUS), oneLine(event.Query))
		v.sqlLine("plan-change", event.SQLID, event.Query, text)
	}
	v.blank()
}

func (v *View) renderAnalyze(snapshot Snapshot) {
	v.section("5. ANALYZE HISTORY")
	v.add("TIME                SOURCE       DATABASE.SCHEMA.TABLE", model.Normal)
	if len(snapshot.AnalyzeHistory) == 0 {
		v.add("暂无可用历史数据", model.Normal)
	}
	for _, record := range snapshot.AnalyzeHistory {
		v.add(fmt.Sprintf("%-19s %-12s %s.%s.%s", formatTimestamp(record.At), record.Source,
			record.Database, record.Schema, record.Table), model.Normal)
	}
	v.renderDatabaseErrors(snapshot.DatabaseErrors, "统计信息")
	v.blank()
}

func (v *View) renderIndexes(snapshot Snapshot) {
	v.section("6. INVALID INDEXES")
	v.add("DATABASE.SCHEMA.TABLE.INDEX                                      USABLE READY VALID", model.Normal)
	if len(snapshot.InvalidIndexes) == 0 {
		v.add("未发现失效索引", model.Normal)
	}
	for _, index := range snapshot.InvalidIndexes {
		name := fmt.Sprintf("%s.%s.%s.%s", index.Database, index.Schema, index.Table, index.Index)
		v.add(fmt.Sprintf("%-68s %-6t %-5t %-5t", name, index.Usable, index.Ready, index.Valid), model.Normal)
	}
	v.renderDatabaseErrors(snapshot.DatabaseErrors, "失效索引")
	v.blank()
}

func (v *View) renderWaits(snapshot Snapshot) {
	v.section("7. WAIT EVENTS SINCE GSTOP TOP5")
	v.add(fmt.Sprintf("DB CPU (not ranked): time=%s share=%.2f%%", formatIntegerMicroseconds(snapshot.CPU.TimeUSDelta), snapshot.CPU.Share*100),
		model.Style{Pair: model.PairCyan, Bold: true})
	v.add("#  EVENT                              WAITS      TIME            AVG             SHARE     CLASS", model.Normal)
	if len(snapshot.Waits) == 0 {
		v.add("启动后暂无等待时长增量", model.Normal)
	}
	for i, wait := range snapshot.Waits {
		v.add(fmt.Sprintf("%-2d %-34s %-10d %-15s %-15s %-9.2f%% %s", i+1, wait.Event,
			wait.WaitsDelta, formatIntegerMicroseconds(wait.TimeUSDelta), formatMicroseconds(wait.AverageUS),
			wait.Share*100, wait.Type), model.Normal)
	}
}

func (v *View) renderDatabaseErrors(errors []DatabaseError, area string) {
	for _, item := range errors {
		if item.Area == area {
			v.add(fmt.Sprintf("[DB ERROR] %s: %s", item.Database, item.Message), model.Style{Pair: model.PairAlarmRed, Bold: true})
		}
	}
}

func (v *View) section(title string) { v.add(title, model.Style{Pair: model.PairReverse, Bold: true}) }
func (v *View) blank()               { v.add("", model.Normal) }

func (v *View) add(text string, style model.Style) {
	v.lines = append(v.lines, viewLine{text: text, style: style})
}

func (v *View) addWrapped(text string) {
	width := v.width - 1
	if width <= 0 {
		width = 1
	}
	for _, logical := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		runes := []rune(logical)
		if len(runes) == 0 {
			v.add("", model.Normal)
			continue
		}
		for len(runes) > width {
			v.add(string(runes[:width]), model.Normal)
			runes = runes[width:]
		}
		v.add(string(runes), model.Normal)
	}
}

func (v *View) sqlLine(section string, sqlID int64, query, text string) {
	row := len(v.lines)
	v.selections = append(v.selections, Selection{SQLID: sqlID, Query: query, Row: row, Section: section})
	v.add(text, model.Normal)
}

// Draw rebuilds and blits the page at scroll to the full terminal.
func (v *View) Draw(screen tcell.Screen, snapshot Snapshot, selected, scroll int, selecting bool) {
	v.Render(snapshot, selected, selecting)
	v.Blit(screen, scroll)
}

// DrawDetail rebuilds and blits the SQL detail page.
func (v *View) DrawDetail(screen tcell.Screen, detail Detail, scroll int) {
	v.RenderDetail(detail)
	v.Blit(screen, scroll)
}

// Blit draws the already-rendered page from the requested source row.
func (v *View) Blit(screen tcell.Screen, scroll int) {
	if v.pad != nil {
		v.pad.BlitViewport(screen, 0, 0, v.ClampScroll(scroll, screenHeight(screen)))
	}
}

// SelectableSQL returns a copy of the current SQL-row mapping.
func (v *View) SelectableSQL() []Selection { return append([]Selection(nil), v.selections...) }

// Lines returns the rendered text without styles, useful for diagnostics.
func (v *View) Lines() []string {
	out := make([]string, len(v.lines))
	for i, line := range v.lines {
		out[i] = line.text
	}
	return out
}

// Height returns the rendered document height.
func (v *View) Height() int { return len(v.lines) }

// EnsureVisible adjusts scroll so the selected SQL row is inside the viewport.
func (v *View) EnsureVisible(selected, scroll, viewportHeight int) int {
	if selected < 0 || selected >= len(v.selections) || viewportHeight <= 0 {
		return v.ClampScroll(scroll, viewportHeight)
	}
	row := v.selections[selected].Row
	if row < scroll {
		scroll = row
	} else if row >= scroll+viewportHeight {
		scroll = row - viewportHeight + 1
	}
	return v.ClampScroll(scroll, viewportHeight)
}

// ClampScroll bounds a source row to the current page and viewport.
func (v *View) ClampScroll(scroll, viewportHeight int) int {
	if scroll < 0 {
		return 0
	}
	maxScroll := v.Height() - viewportHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll > maxScroll {
		return maxScroll
	}
	return scroll
}

func formatTimestamp(value time.Time) string {
	if value.IsZero() {
		return "--"
	}
	return value.Format("2006-01-02 15:04:05")
}

func formatMicroseconds(value float64) string {
	if value >= 1_000_000 {
		return fmt.Sprintf("%.2fs", value/1_000_000)
	}
	if value >= 1_000 {
		return fmt.Sprintf("%.2fms", value/1_000)
	}
	return fmt.Sprintf("%.2fµs", value)
}

func formatIntegerMicroseconds(value int64) string { return formatMicroseconds(float64(value)) }

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func screenHeight(screen tcell.Screen) int {
	if screen == nil {
		return 0
	}
	_, height := screen.Size()
	return height
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
