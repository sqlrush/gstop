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
	SQLID                    int64
	Query                    string
	Databases                []string
	Users                    []string
	Row                      int
	Section                  string
	RepresentativePID        int64
	RepresentativeSessionID  string
	RepresentativeElapsedUS  float64
	RepresentativeQueryStart time.Time
	CapturedAt               time.Time
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

	v.renderActiveElapsed(snapshot.ActiveElapsedSQL)
	v.renderAverage(snapshot.AverageSQL)
	v.renderExecutions(snapshot.ExecutionSQL)
	v.renderMemory(snapshot)
	v.renderPlanChanges(snapshot.PlanChanges)
	v.renderAnalyze(snapshot)
	v.renderIndexes(snapshot)
	v.renderWaits(snapshot)
	v.renderLocks(snapshot)
	v.renderReplication(snapshot)
	v.renderCluster(snapshot)

	v.blank()
	v.add("Keys: q exit | Esc back | arrows/PgUp/PgDn/Home/End scroll | s select SQL | p SQL details/plan | r refresh slow checks", model.Style{Pair: model.PairReverse})

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
	v.add(fmt.Sprintf("SQL_ID: %d | DATABASE: %s | SCHEMA: %s | USER: %s",
		detail.SQLID, joinedIdentity(detail.Databases), joinedIdentity(detail.Schemas),
		joinedIdentity(detail.Users)), model.Normal)
	if detail.Loading {
		v.add("正在加载执行计划、运行证据和目录信息；按 Esc 可取消并返回……",
			model.Style{Pair: model.PairConfirmYellow, Bold: true})
	}
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
	if detail.Error != "" && len(detail.Stages) == 0 {
		v.addWrapped(detail.Error)
	}
	if len(detail.PlanLines) == 0 && (detail.Error == "" || len(detail.Stages) > 0) {
		v.add("没有可用的执行计划", model.Normal)
	}
	for _, line := range detail.PlanLines {
		v.addWrapped(line)
	}
	v.renderPlanStageStatuses(detail)
	v.blank()
	v.renderPlanHotspot(detail)
	v.blank()
	v.renderAccessAndStatistics(detail)
	if len(detail.Notices) > 0 {
		v.blank()
		v.section("[EVIDENCE LIMITATIONS]")
		for _, notice := range detail.Notices {
			if notice.Area == "plan" && detail.PlanQuality >= PlanQualityPreflight &&
				strings.Contains(notice.Message, "绑定变量") {
				continue
			}
			v.addWrapped(fmt.Sprintf("%s: %s", notice.Area, notice.Message))
		}
	}
	v.blank()
	v.add("Keys: q exit | Esc back to health dashboard | arrows/PgUp/PgDn/Home/End scroll", model.Style{Pair: model.PairReverse})
	v.pad = tui.NewPad(maxInt(1, len(v.lines)), v.width)
	for row, line := range v.lines {
		v.pad.AddStr(row, 0, line.text, line.style)
	}
}

func detailStageLine(label string, status StageStatus) string {
	if status.State == "" {
		status.State = StageLoading
	}
	if status.Message == "" {
		return fmt.Sprintf("%s: %s", label, status.State)
	}
	return fmt.Sprintf("%s: %s - %s", label, status.State, status.Message)
}

func (v *View) renderPlanStageStatuses(detail Detail) {
	for _, stage := range []struct {
		label string
		stage DetailStage
	}{
		{label: "当前会话", stage: StageCaptured},
		{label: "历史计划", stage: StageHistory},
		{label: "重新定位", stage: StageRelocate},
		{label: "场景预检", stage: StagePreflight},
		{label: "估算计划", stage: StageEstimate},
	} {
		if stage.stage == StageEstimate && detail.PlanQuality >= PlanQualityPreflight {
			continue
		}
		status := detail.Stages[stage.stage]
		if (status.State == StageReady || status.State == StageDone) && status.Message == "" {
			continue
		}
		v.addWrapped(detailStageLine(stage.label, status))
	}
}

func (v *View) renderPlanHotspot(detail Detail) {
	v.section("[PLAN HOTSPOT]")
	if detail.Plan.Hotspot == nil {
		status := detail.Stages[StagePlan]
		if status.State == "" && detail.Loading {
			status.State = StageLoading
		}
		if status.State == StageLoading {
			v.add(detailStageLine("计划热点", status), model.Style{Pair: model.PairConfirmYellow})
		} else {
			v.add("无法从当前文本计划确定 cost 热点", model.Style{Pair: model.PairCyan})
		}
	} else {
		hotspot := detail.Plan.Hotspot
		target := hotspot.NodeType
		if hotspot.Relation != "" {
			target += " on " + hotspot.Relation
		}
		v.addWrapped(fmt.Sprintf("Operation: %s | self cost=%.2f | total cost=%.2f | cost share=%.2f%%",
			target, hotspot.SelfCost, hotspot.TotalCost, hotspot.CostShare*100))
		if hotspot.Explanation != "" {
			v.addWrapped("Analysis: " + hotspot.Explanation)
		}
	}

	if detail.Runtime.CPU.Available {
		cpu := detail.Runtime.CPU
		line := fmt.Sprintf("CPU history (statement_history, %d samples): newest=%.2fms | median=%.2fms | max=%.2fms",
			cpu.Samples, cpu.NewestMS, cpu.MedianMS, cpu.MaxMS)
		if cpu.CPUToDBAvailable {
			line += fmt.Sprintf(" | CPU/DB=%.2f%%", cpu.CPUToDBRatio*100)
		} else {
			line += " | CPU/DB=unavailable"
		}
		v.addWrapped(line)
		v.addWrapped("CPU note: parallel worker CPU may be accumulated by the database time model")
	} else {
		v.addWrapped(detailStageLine("CPU history", detail.Stages[StageCPU]))
	}
	if detail.Runtime.ASH.Available {
		ash := detail.Runtime.ASH
		v.addWrapped(fmt.Sprintf(
			"ASH inference (active samples only): on-CPU=%d/%d (%.2f%%); idle in transaction excluded",
			ash.OnCPUSamples, ash.ActiveSamples, ash.OnCPUShare*100,
		))
		for _, wait := range ash.Waits {
			v.addWrapped(fmt.Sprintf("ASH wait: %s samples=%d", wait.Event, wait.Samples))
		}
	} else {
		v.addWrapped(detailStageLine("ASH inference", detail.Stages[StageASH]))
	}
}

func (v *View) renderAccessAndStatistics(detail Detail) {
	v.section("[ACCESS & STATISTICS]")
	if detail.CatalogSource != "" {
		v.addWrapped("诊断依据: " + detail.CatalogSource + " + 当前索引目录")
	}
	if len(detail.Tables) == 0 {
		status := detail.Stages[StageCatalog]
		if status.State == "" && !detail.Loading {
			v.add("没有可可靠解析的表访问证据", model.Style{Pair: model.PairCyan})
		} else {
			v.addWrapped(detailStageLine("索引与统计信息", status))
		}
		return
	}
	for i, table := range detail.Tables {
		if i > 0 {
			v.add(strings.Repeat("-", minInt(v.width-1, 72)), model.Normal)
		}
		name := table.Access.Table
		if table.Access.Schema != "" {
			name = table.Access.Schema + "." + name
		}
		hotspot := ""
		if table.Access.InHotspotSubtree {
			hotspot = " | HOTSPOT SUBTREE"
		}
		v.addWrapped(fmt.Sprintf("Table: %s | access=%s%s", name, table.Access.ScanType, hotspot))
		v.add("索引策略: "+string(table.Index.Assessment), indexAssessmentStyle(table.Index.Assessment))
		for _, reason := range table.Index.Reasons {
			v.addWrapped("  Reason: " + reason)
		}
		if len(table.Index.Existing) == 0 {
			v.add("  Existing indexes: none visible", model.Normal)
		} else {
			for _, index := range table.Index.Existing {
				v.addWrapped(fmt.Sprintf("  Existing index: %s (%s) valid=%t ready=%t usable=%t",
					index.Name, strings.Join(index.Columns, ", "), index.Valid, index.Ready, index.Usable))
			}
		}
		if table.Index.SuggestedDDL != "" {
			v.addWrapped("  Display-only suggestion: " + table.Index.SuggestedDDL)
		}

		stats := table.Statistics
		v.add("统计信息: "+string(stats.State), freshnessStyle(stats.State))
		if !stats.LastDataChanged.IsZero() {
			v.addWrapped("  Last data changed: " + formatTimestamp(stats.LastDataChanged))
		}
		if !stats.TriggerAvailable {
			v.addWrapped(fmt.Sprintf("  Last analyze: %s | trigger=unavailable | due=unavailable",
				formatTimestamp(stats.LastAnalyze)))
		} else {
			v.addWrapped(fmt.Sprintf("  Last analyze: %s | trigger=%.0f | due=%.2f%%",
				formatTimestamp(stats.LastAnalyze), stats.Trigger, stats.DueRatio*100))
		}
		for _, reason := range stats.Reasons {
			v.addWrapped("  Reason: " + reason)
		}
	}
}

func freshnessStyle(state Freshness) model.Style {
	switch state {
	case FreshnessExpired:
		return model.Style{Pair: model.PairAlarmRed, Bold: true}
	case FreshnessSuspect:
		return model.Style{Pair: model.PairConfirmYellow, Bold: true}
	case FreshnessNormal:
		return model.Style{Pair: model.PairGreen, Bold: true}
	default:
		return model.Style{Pair: model.PairCyan}
	}
}

func indexAssessmentStyle(assessment IndexAssessment) model.Style {
	switch assessment {
	case IndexUnreasonable:
		return model.Style{Pair: model.PairAlarmRed, Bold: true}
	case IndexReasonable:
		return model.Style{Pair: model.PairGreen, Bold: true}
	default:
		return model.Style{Pair: model.PairCyan}
	}
}

func (v *View) refreshStatus(snapshot Snapshot) string {
	return fmt.Sprintf("Started: %s | Fast: %s | Memory: %s | Cross-DB: %s%s",
		formatTimestamp(snapshot.StartedAt), formatTimestamp(snapshot.FastRefreshedAt),
		formatTimestamp(snapshot.MemoryRefreshedAt), formatTimestamp(snapshot.SlowRefreshedAt),
		map[bool]string{true: " (refreshing)", false: ""}[snapshot.SlowRefreshing])
}

func (v *View) renderAverage(rows []SQLMetric) {
	v.section("2. AVG ELAPSED SINCE GSTOP TOP5 SQL")
	v.add("#  SQL_ID              AVG_ELAPSED       CALLS      ACTIVE  SQL", model.Normal)
	if len(rows) == 0 {
		v.add("暂无可用SQL统计", model.Normal)
	}
	for i, row := range rows {
		text := fmt.Sprintf("%-2d %-19d %-17s %-10d %-7d %s", i+1, row.SQLID,
			formatMicroseconds(row.AverageUS), row.Calls, row.ActiveSessions, oneLine(row.Query))
		v.sqlLine("average", row, text)
	}
	v.blank()
}

func (v *View) renderActiveElapsed(rows []SQLMetric) {
	v.section("1. CURRENT ACTIVE SQL ELAPSED TOP5")
	v.add("#  SQL_ID              MAX_ELAPSED       SESSIONS  SQL", model.Normal)
	if len(rows) == 0 {
		v.add("当前无活跃 SQL", model.Normal)
	}
	for i, row := range rows {
		text := fmt.Sprintf("%-2d %-19d %-17s %-9d %s", i+1, row.SQLID,
			formatMicroseconds(row.RepresentativeElapsedUS), row.ActiveSessions, oneLine(row.Query))
		v.sqlLine("active-elapsed", row, text)
	}
	v.blank()
}

func (v *View) renderExecutions(rows []SQLMetric) {
	v.section("3. EXECUTIONS SINCE GSTOP TOP5 SQL")
	v.add("#  SQL_ID              EXEC_DELTA  ACTIVE  SQL", model.Normal)
	if len(rows) == 0 {
		v.add("启动后暂无已完成执行增量", model.Normal)
	}
	for i, row := range rows {
		text := fmt.Sprintf("%-2d %-19d %-11d %-7d %s", i+1, row.SQLID, row.CallsDelta, row.ActiveSessions, oneLine(row.Query))
		v.sqlLine("executions", row, text)
	}
	v.blank()
}

func (v *View) renderMemory(snapshot Snapshot) {
	v.section("4. ACTIVE SQL DYNAMIC MEMORY TOP5")
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
		v.sqlLine("memory", row, text)
	}
	v.blank()
}

func (v *View) renderPlanChanges(events []PlanChangeEvent) {
	v.section("5. PLAN CHANGES SINCE GSTOP")
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
		v.sqlLine("plan-change", SQLMetric{SQLID: event.SQLID, Query: event.Query}, text)
	}
	v.blank()
}

func (v *View) renderAnalyze(snapshot Snapshot) {
	v.section("6. ANALYZE HISTORY")
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
	v.section("7. INVALID INDEXES")
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
	v.section("8. WAIT EVENTS SINCE GSTOP TOP5")
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
	v.blank()
}

func (v *View) renderLocks(snapshot Snapshot) {
	v.section("9. CURRENT LOCK CHAINS TOP5")
	if snapshot.LockError != "" {
		v.add("LOCK ERROR: "+snapshot.LockError, model.Style{Pair: model.PairAlarmRed, Bold: true})
		v.blank()
		return
	}
	if len(snapshot.Lock.Chains) == 0 {
		v.add("No current lock waits", model.Style{Pair: model.PairGreen})
		v.blank()
		return
	}
	v.add(fmt.Sprintf("waiters=%d blockers=%d longest=%s", snapshot.Lock.Waiters,
		snapshot.Lock.Blockers, formatMicroseconds(snapshot.Lock.LongestWaitUS)), model.Style{Pair: model.PairConfirmYellow, Bold: true})
	for i, chain := range snapshot.Lock.Chains {
		v.add(fmt.Sprintf("%d BLOCKER pid=%d session=%s lock=%s mode=%s tag=%s", i+1,
			chain.BlockerPID, chain.BlockerSession, chain.LockType, chain.Mode, chain.LockTag),
			model.Style{Pair: model.PairAlarmRed, Bold: true})
		v.add(fmt.Sprintf("  WAITER  pid=%d session=%s elapsed=%s SQL_ID=%d SQL=%s", chain.WaiterPID,
			chain.WaiterSession, formatMicroseconds(chain.ElapsedUS), chain.SQLID, oneLine(chain.Query)),
			model.Style{Pair: model.PairConfirmYellow})
	}
	v.blank()
}

func (v *View) renderReplication(snapshot Snapshot) {
	v.section("10. PRIMARY/STANDBY REPLICATION")
	if snapshot.ReplicationError != "" {
		v.add("REPLICATION ERROR: "+snapshot.ReplicationError, model.Style{Pair: model.PairAlarmRed, Bold: true})
		v.blank()
		return
	}
	v.add("LOCAL ROLE: "+displayOrDash(snapshot.Replication.LocalRole), model.Style{Pair: model.PairCyan, Bold: true})
	if len(snapshot.Replication.Channels) == 0 {
		v.add("No replication channels configured", model.Style{Pair: model.PairGreen})
		v.blank()
		return
	}
	v.add("DIR       LOCAL      PEER       PEER_STATE  STATE        SYNC       PCT      LAG        CHANNEL", model.Normal)
	for _, channel := range snapshot.Replication.Channels {
		style := model.Normal
		if explicitlyUnhealthy(channel.State) || explicitlyUnhealthy(channel.PeerState) {
			style = model.Style{Pair: model.PairAlarmRed, Bold: true}
		}
		v.add(fmt.Sprintf("%-9s %-10s %-10s %-11s %-12s %-10s %-8.2f %-10s %s",
			channel.Direction, channel.LocalRole, channel.PeerRole, channel.PeerState, channel.State,
			channel.SyncState, channel.SyncPercent, formatBytes(channel.LagBytes), channel.Channel), style)
	}
	v.blank()
}

func (v *View) renderCluster(snapshot Snapshot) {
	v.section("11. DISTRIBUTED CLUSTER HEALTH")
	if !snapshot.Cluster.SQLAvailable && !snapshot.Cluster.CMAvailable {
		v.add("No distributed cluster topology available (normal for standalone openGauss)", model.Normal)
		return
	}
	if snapshot.Cluster.SQLAvailable {
		v.add("PGXC_NODE TOPOLOGY", model.Style{Pair: model.PairCyan, Bold: true})
		v.add("ROLE        NAME                     PRIMARY  PREFERRED CENTRAL ACTIVE HOST:PORT / STANDBY", model.Normal)
		for _, node := range snapshot.Cluster.Nodes {
			active := "-"
			style := model.Normal
			if node.ActiveKnown {
				active = fmt.Sprintf("%t", node.Active)
				if !node.Active {
					style = model.Style{Pair: model.PairAlarmRed, Bold: true}
				}
			}
			v.add(fmt.Sprintf("%-11s %-24s %-8t %-9t %-7t %-6s %s:%d / %s:%d",
				clusterNodeRole(node.Type), node.Name, node.NodePrimary || node.HostPrimary, node.Preferred,
				node.Central, active, node.Host, node.Port, node.StandbyHost, node.StandbyPort), style)
		}
	}
	if snapshot.Cluster.CMAvailable {
		v.add("CM RUNTIME STATE", model.Style{Pair: model.PairCyan, Bold: true})
		v.add("COMPONENT    NODE                     INSTANCE   ROLE             STATE", model.Normal)
		for _, component := range snapshot.Cluster.Components {
			style := model.Normal
			if explicitlyUnhealthy(component.State) {
				style = model.Style{Pair: model.PairAlarmRed, Bold: true}
			}
			v.add(fmt.Sprintf("%-12s %-24s %-10s %-16s %s", component.Kind, component.Node,
				component.Instance, component.Role, component.State), style)
		}
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

func (v *View) sqlLine(section string, metric SQLMetric, text string) {
	row := len(v.lines)
	v.selections = append(v.selections, Selection{
		SQLID: metric.SQLID, Query: metric.Query, Row: row, Section: section,
		Databases:                append([]string(nil), metric.Databases...),
		Users:                    append([]string(nil), metric.Users...),
		RepresentativePID:        metric.RepresentativePID,
		RepresentativeSessionID:  metric.RepresentativeSessionID,
		RepresentativeElapsedUS:  metric.RepresentativeElapsedUS,
		RepresentativeQueryStart: metric.RepresentativeQueryStart,
		CapturedAt:               metric.CapturedAt,
	})
	v.add(text, model.Normal)
}

func joinedIdentity(values []string) string {
	if len(values) == 0 {
		return "--"
	}
	return strings.Join(values, ",")
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
	if v.pad == nil || screen == nil {
		return
	}
	screenWidth, viewportHeight := screen.Size()
	scroll = v.ClampScroll(scroll, viewportHeight)
	v.pad.BlitViewport(screen, 0, 0, scroll)
	v.drawScrollbar(screen, screenWidth, viewportHeight, scroll)
}

func (v *View) drawScrollbar(screen tcell.Screen, screenWidth, viewportHeight, scroll int) {
	geometry := ComputeScrollbar(v.Height(), viewportHeight, scroll)
	if !geometry.Visible || screenWidth <= 0 {
		return
	}
	x := screenWidth - 1
	style := tcell.StyleDefault.Reverse(true)
	if geometry.Arrows {
		screen.SetContent(x, 0, '^', nil, style)
		screen.SetContent(x, viewportHeight-1, 'v', nil, style)
	}
	for y := geometry.TrackStart; y <= geometry.TrackEnd; y++ {
		screen.SetContent(x, y, '|', nil, style)
	}
	for y := geometry.ThumbStart; y <= geometry.ThumbEnd; y++ {
		screen.SetContent(x, y, '#', nil, style)
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

func displayOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "--"
	}
	return value
}

func formatBytes(value int64) string {
	if value >= 1024*1024*1024 {
		return fmt.Sprintf("%.2fGB", float64(value)/(1024*1024*1024))
	}
	if value >= 1024*1024 {
		return fmt.Sprintf("%.2fMB", float64(value)/(1024*1024))
	}
	if value >= 1024 {
		return fmt.Sprintf("%.2fKB", float64(value)/1024)
	}
	return fmt.Sprintf("%dB", value)
}

func explicitlyUnhealthy(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"down", "abnormal", "unavailable", "disconnected", "fault", "failed", "need repair", "disk damaged"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func clusterNodeRole(value string) string {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "C":
		return "CN"
	case "D":
		return "DN"
	case "S":
		return "STANDBY DN"
	default:
		return value
	}
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
