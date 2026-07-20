package monitor

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"gstop/internal/dbconn"
	"gstop/internal/model"
	"gstop/internal/tui"
)

// snapshot deep-copies a panel so callers can read it without holding the lock.
func (p memPanel) snapshot() MemPanelData {
	d := MemPanelData{
		Title:  p.title,
		Header: append([]string(nil), p.header...),
		Width:  append([]int(nil), p.width...),
	}
	d.Value = make([][]any, len(p.value))
	for i, row := range p.value {
		d.Value[i] = append([]any(nil), row...)
	}
	return d
}

// memFloat reads column i of a row as float64, defaulting to 0.
func memFloat(r dbconn.Row, i int) float64 {
	f, _ := r.Float(i)
	return f
}

// memRowNames collects column idx of every row, used to size context columns.
func memRowNames(rows []dbconn.Row, idx int) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Str(idx)
	}
	return out
}

// refreshSummaryInfo builds Panel0: process/dynamic/shared limits, usage, and
// usage percentages. Any missing or zero limit yields the all-zero row. Port of
// refresh_summary_info.
func (m *MemoryMonitor) refreshSummaryInfo() memPanel {
	p := memPanel{
		title:  "",
		header: append([]string(nil), m.items...),
		width:  append([]int(nil), m.widths...),
	}
	now := time.Now().Format("2006-01-02 15:04:05")
	zeroRow := []any{now, 0, 0, 0, 0, 0, 0, 0, 0}

	rows := m.deps.DB.Query(memSummaryQuery)
	if rows == nil {
		m.deps.Logger.Error("Exec query failed.")
		p.value = [][]any{zeroRow}
		return p
	}

	raw := map[string]any{}
	num := map[string]float64{}
	for _, r := range rows {
		t := r.Str(0)
		raw[t] = r.Col(1)
		num[t] = memFloat(r, 1)
	}

	maxProcess := num["max_process_memory"]
	maxDynamic := num["max_dynamic_memory"]
	maxShared := num["max_shared_memory"]
	if maxProcess == 0 || maxDynamic == 0 || maxShared == 0 {
		m.deps.Logger.Error("Invalid value, max_process_memory = %v, max_dynamic_memory = %v, max_shared_memory = %v.",
			maxProcess, maxDynamic, maxShared)
		p.value = [][]any{zeroRow}
		return p
	}

	rawOr0 := func(k string) any {
		if v, ok := raw[k]; ok {
			return v
		}
		return 0
	}
	p.value = [][]any{{
		now,
		rawOr0("max_process_memory"), round2(num["process_used_memory"] / maxProcess * 100),
		rawOr0("max_dynamic_memory"), rawOr0("dynamic_used_memory"), round2(num["dynamic_used_memory"] / maxDynamic * 100),
		rawOr0("max_shared_memory"), rawOr0("shared_used_memory"), round2(num["shared_used_memory"] / maxShared * 100),
	}}
	return p
}

// refreshDynamicInfo builds Panel1 (TOP 5 dynamic global memory) as a transposed
// table: a label column, a SUM column, then up to five context columns. Value has
// exactly two rows (TOTAL, FREE). Port of refresh_dynamic_info.
func (m *MemoryMonitor) refreshDynamicInfo() memPanel {
	p := memPanel{title: memPanel1Title, value: [][]any{{}, {}}}

	rows := m.deps.DB.Query(memDynamicQuery)
	if rows == nil {
		m.deps.Logger.Error("Exec query failed.")
		memAddColumn(&p, 10, []any{"", "TOTAL", "FREE"})
		memAddColumn(&p, 10, []any{"SUM", 0, 0})
		return p
	}

	delta := m.calcDelta(20, memRowNames(rows, 0))
	memAddColumn(&p, 10, []any{"", "TOTAL", "FREE"})

	totalSum, freeSum := 0.0, 0.0
	for _, r := range rows {
		totalSum += round2(memFloat(r, 1))
		freeSum += round2(memFloat(r, 2))
	}
	memAddColumn(&p, 10, []any{"SUM", round2(totalSum), round2(freeSum)})

	for idx, r := range rows {
		if idx >= 5 {
			break
		}
		d := delta
		if idx == 4 {
			d = 0
		}
		name := r.Str(0)
		memAddColumn(&p, len([]rune(name))+d, []any{name, round2(memFloat(r, 1)), round2(memFloat(r, 2))})
	}
	return p
}

// refreshSessionInfo builds Panel2 (TOP 10 session memory). Port of refresh_session_info.
func (m *MemoryMonitor) refreshSessionInfo() memPanel {
	rows := m.deps.DB.Query(memSessionQuery)
	if rows == nil {
		m.deps.Logger.Error("Exec query failed.")
		return memPanel{title: memPanel2Title}
	}
	return m.buildTopPanel(memPanel2Title, "SESSION_ID", rows, memSessionKey, 2, 5)
}

// refreshThreadInfo builds Panel3 (TOP 10 thread memory). Port of refresh_thread_info.
func (m *MemoryMonitor) refreshThreadInfo() memPanel {
	rows := m.deps.DB.Query(memThreadQuery)
	if rows == nil {
		m.deps.Logger.Error("Exec query failed.")
		return memPanel{title: memPanel3Title}
	}
	return m.buildTopPanel(memPanel3Title, "TID", rows, memThreadKey, 3, 6)
}

// memRowKey identifies which entity (session or thread) a raw row belongs to and
// what to display/terminate it with. ok is false for malformed rows, which are
// skipped (the Python code would raise and abort the whole panel).
type memRowKey struct {
	display any
	group   string
	ok      bool
}

// memSessionKey groups by the substringed session id (row[0]).
func memSessionKey(r dbconn.Row) memRowKey {
	sid := r.Str(0)
	return memRowKey{display: sid, group: sid, ok: true}
}

// memThreadKey groups by the thread id parsed from "timestamp.tid" (row[0]).
func memThreadKey(r dbconn.Row) memRowKey {
	parts := strings.Split(r.Str(0), ".")
	if len(parts) < 2 {
		return memRowKey{ok: false}
	}
	tid, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return memRowKey{ok: false}
	}
	return memRowKey{display: tid, group: strconv.FormatInt(tid, 10), ok: true}
}

// memCtxSum is a context name and its summed size, used to pick the TOP 5 columns.
type memCtxSum struct {
	key   string
	total float64
}

// buildTopPanel groups rows by entity, aggregates each entity's total memory (MB)
// and per-context memory, then lays out the TOP 10 entities (by total, desc) as
// rows and the TOP 5 contexts (by summed size, desc) as columns. Shared by
// refresh_session_info and refresh_thread_info. First-seen order is preserved for
// stable-sort tie-breaking, matching Python dict insertion order.
func (m *MemoryMonitor) buildTopPanel(title, idHeader string, rows []dbconn.Row,
	keyOf func(dbconn.Row) memRowKey, ctxIdx, sizeIdx int) memPanel {
	p := memPanel{title: title}

	var order []string
	groups := map[string][]dbconn.Row{}
	display := map[string]any{}
	var ctxOrder []string
	ctxGroups := map[string][]dbconn.Row{}

	for _, r := range rows {
		k := keyOf(r)
		if !k.ok {
			continue
		}
		if _, ok := groups[k.group]; !ok {
			order = append(order, k.group)
			display[k.group] = k.display
		}
		groups[k.group] = append(groups[k.group], r)

		cn := r.Str(ctxIdx)
		if _, ok := ctxGroups[cn]; !ok {
			ctxOrder = append(ctxOrder, cn)
		}
		ctxGroups[cn] = append(ctxGroups[cn], r)
	}

	ctxSums := memSumGroups(ctxOrder, ctxGroups, sizeIdx)
	aggs := memEntityAggs(order, groups, display, ctxIdx, sizeIdx)

	valueLines := min(len(aggs), 10)
	p.value = make([][]any, valueLines)

	memAppendHeader(&p, 20, idHeader)
	memAppendHeader(&p, 10, "SUM")
	delta := m.calcDelta(30, memGroupKeys(ctxSums))
	for i, g := range ctxSums {
		if i >= 5 {
			break
		}
		d := delta
		if i == 4 {
			d = 0
		}
		memAppendHeader(&p, len([]rune(g.key))+d, g.key)
	}

	for i := 0; i < valueLines; i++ {
		a := aggs[i]
		row := make([]any, 0, len(p.header))
		row = append(row, a.id, a.sum)
		for _, h := range p.header {
			if h == idHeader || h == "SUM" {
				continue
			}
			if v, ok := a.ctx[h]; ok {
				row = append(row, v)
			} else {
				row = append(row, 0)
			}
		}
		p.value[i] = row
	}
	return p
}

// memEntityAgg is one session/thread with its total (MB) and per-context memory.
type memEntityAgg struct {
	id  any
	sum float64
	ctx map[string]float64
}

// memEntityAggs aggregates each entity's total memory and per-context memory,
// sorted by total descending (stable, keeping first-seen order on ties).
func memEntityAggs(order []string, groups map[string][]dbconn.Row, display map[string]any,
	ctxIdx, sizeIdx int) []memEntityAgg {
	aggs := make([]memEntityAgg, 0, len(order))
	for _, k := range order {
		sumBytes := 0.0
		ctx := map[string]float64{}
		for _, r := range groups[k] {
			cn := r.Str(ctxIdx)
			ts := memFloat(r, sizeIdx)
			ctx[cn] = round2(ts / 1024 / 1024)
			sumBytes += ts
		}
		aggs = append(aggs, memEntityAgg{id: display[k], sum: round2(sumBytes / 1024 / 1024), ctx: ctx})
	}
	sort.SliceStable(aggs, func(a, b int) bool { return aggs[a].sum > aggs[b].sum })
	return aggs
}

// memSumGroups sums sizeIdx over each context group and returns them sorted by
// total descending (stable, first-seen order on ties).
func memSumGroups(order []string, groups map[string][]dbconn.Row, sizeIdx int) []memCtxSum {
	out := make([]memCtxSum, 0, len(order))
	for _, k := range order {
		sum := 0.0
		for _, r := range groups[k] {
			sum += memFloat(r, sizeIdx)
		}
		out = append(out, memCtxSum{key: k, total: sum})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].total > out[b].total })
	return out
}

// memGroupKeys extracts the ordered context names from a sorted sum list.
func memGroupKeys(s []memCtxSum) []string {
	keys := make([]string, len(s))
	for i, g := range s {
		keys[i] = g.key
	}
	return keys
}

// memAddColumn appends a fully-formed column: cells[0] is the header, the rest are
// the value-row cells top to bottom. Port of panel_add_column.
func memAddColumn(p *memPanel, width int, cells []any) {
	p.width = append(p.width, width)
	for i, c := range cells {
		if i == 0 {
			p.header = append(p.header, model.DisplayValue(c))
			continue
		}
		if i-1 < len(p.value) {
			p.value[i-1] = append(p.value[i-1], c)
		}
	}
}

// memAppendHeader appends one header column (name + width) with no value cells,
// matching panel_append_column called with row_id == 0.
func memAppendHeader(p *memPanel, width int, name string) {
	p.header = append(p.header, name)
	p.width = append(p.width, width)
}

// calcDelta spreads the leftover width across the (up to five) context columns
// after reserving the fixed columns and the context-name lengths. Port of
// calculate_delta; col_num <= 1 (where Python would divide by zero) yields 0.
func (m *MemoryMonitor) calcDelta(reserved int, names []string) int {
	colNum := 0
	nameLen := 0
	for i, n := range names {
		if i >= 5 {
			break
		}
		colNum++
		nameLen += len([]rune(n))
	}
	if colNum <= 1 {
		return 0
	}
	return (m.width - 1 - reserved - nameLen) / (colNum - 1)
}

// TerminateSessionOrThread kills the session (Panel2) or thread (Panel3) under the
// cursor after a second confirmation, reproducing the selected-index arithmetic of
// terminate_session_or_thread. selected_index = cursorY - beginY - 1; rows
// 9<idx<=9+len(panel2) are sessions, and rows 9+len(panel2)+3<idx<=... are threads
// (the +3 skips the panel gap, Panel3 title, and Panel3 header).
func (m *MemoryMonitor) TerminateSessionOrThread(screen *tui.Screen, cursorY int) {
	selectedIndex := cursorY - m.beginY - 1
	if selectedIndex < 0 {
		return
	}
	if !tui.TerminateConfirmPassed(screen, model.MemoryCursorXStart, cursorY) {
		return
	}

	m.mu.Lock()
	panel2 := m.panels[2]
	panel3 := m.panels[3]
	m.mu.Unlock()

	l2 := len(panel2.value)
	l3 := len(panel3.value)
	switch {
	case selectedIndex > 9 && selectedIndex <= 9+l2:
		idx := selectedIndex - 10
		if idx >= 0 && idx < l2 {
			m.terminateSession(0, panel2.value[idx][0])
		}
	case selectedIndex > 9+l2+3 && selectedIndex <= 9+l2+3+l3:
		idx := selectedIndex - (9 + l2 + 3 + 1)
		if idx >= 0 && idx < l3 {
			m.terminateBackend(panel3.value[idx][0])
		}
	}
}
