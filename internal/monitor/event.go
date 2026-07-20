package monitor

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

const (
	eventName   = "event"
	eventHeight = 7
	eventConfig = "event.cfg"
)

// eventQuery samples dbe_perf.wait_events plus the instance CPU_TIME scalar. Copied
// verbatim from monitor/event.py. Column order per row is
// [0]CPU_TIME [1]event [2]wait [3]total_wait_time [4]avg_wait_time [5]type.
const eventQuery = `
    SELECT
        (SELECT value FROM GS_INSTANCE_TIME WHERE stat_name = 'CPU_TIME') AS "CPU_TIME",
        event,
        wait,
        total_wait_time,
        avg_wait_time,
        type
    FROM dbe_perf.wait_events
    WHERE event != 'none' AND wait != 0 AND event != 'wait cmd'
    ORDER BY wait DESC;
    `

// eventSample is the previous refresh's raw counters for one event, keyed by event
// name, used to derive the realtime (RT) deltas.
type eventSample struct {
	waits  int64
	timeUs int64
}

// eventLine is one rendered table row: six display cells plus the numeric PCT used
// as the (stable) sort key. cols[4] is the pre-formatted PCT string.
type eventLine struct {
	cols [6]string
	pct  float64
}

// EventMonitor renders the wait-event table with a per-cycle DB CPU synthetic row.
// It supports two modes, toggled at runtime by the main loop:
//   - realtime (RT): each cell is the delta against the previous sample;
//   - cumulative (C): each cell is the absolute value since instance start.
//
// Port of monitor/event.py.
type EventMonitor struct {
	base

	items  []string
	widths []int

	lines []eventLine

	// immediate is the desired mode (true = RT), set by SetImmediate; currImmediate
	// is the snapshot taken at the start of a refresh and used to draw the header.
	immediate     bool
	currImmediate bool

	// Previous-sample caches; touched only on the (single) refresh goroutine.
	lastCPU    int64
	lastTotal  int64
	lastEvents map[string]eventSample
}

// NewEventMonitor builds the wait-event panel (height 7: one header + six rows).
func NewEventMonitor(deps Deps) *EventMonitor {
	return &EventMonitor{base: newBase(eventName, eventHeight, deps)}
}

// SetImmediate switches the panel between realtime (true) and cumulative (false),
// matching the main loop's r/c keybindings. Guarded so the refresh goroutine reads
// a consistent flag.
func (m *EventMonitor) SetImmediate(v bool) {
	m.mu.Lock()
	m.immediate = v
	m.mu.Unlock()
}

// Init lays out the panel, parses event.cfg, and defaults to realtime mode
// (self.immediate = True in the original constructor).
func (m *EventMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse event.cfg failed: %v", err)
	}
	m.immediate = true
	m.currImmediate = true
}

// parseConfig reads event.cfg lines of the form item:width.
func (m *EventMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, eventConfig))
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

// Refresh snapshots the mode flag (so the header and data stay in step for this
// cycle) and re-samples the wait events.
func (m *EventMonitor) Refresh() {
	m.mu.Lock()
	curr := m.immediate
	m.currImmediate = curr
	m.mu.Unlock()

	m.refreshEvent(curr)
}

// refreshEvent runs the query, computes the DB CPU row and per-event rows for the
// active mode, sorts by PCT, and publishes the result. On any early return the
// previously published rows are left in place, exactly as the original did.
func (m *EventMonitor) refreshEvent(realtime bool) {
	rows := m.deps.DB.Query(eventQuery)
	if rows == nil {
		m.deps.Logger.Error("Exec query failed.")
		return
	}
	if len(rows) == 0 {
		m.deps.Logger.Error("No wait events returned.")
		return
	}

	curCPU, _ := rows[0].Int(0)
	curTotal := sumTotalWaitTime(rows)
	if curTotal == 0 || curCPU == 0 {
		m.deps.Logger.Error("Invalid total_time: %d or cpu_time: %d.", curTotal, curCPU)
		return
	}

	cpuDiff, totalDiff := m.computeDiffs(realtime, curCPU, curTotal)
	if totalDiff == 0 {
		// Guards the division below; the original would ZeroDivisionError here.
		m.deps.Logger.Error("Invalid total_time_diff: 0.")
		return
	}

	lines := make([]eventLine, 0, len(rows)+1)
	lines = append(lines, buildDBCPULine(cpuDiff, totalDiff))

	// Save the raw CPU/total counters after the DB CPU row, before the per-event
	// rows, matching the ordering in refresh_event.
	m.lastCPU = curCPU
	m.lastTotal = curTotal

	if realtime {
		lines = append(lines, buildRealtimeLines(rows, totalDiff, m.lastEvents)...)
	} else {
		lines = append(lines, buildCumulativeLines(rows, totalDiff)...)
	}

	// Stable sort keeps the SQL's wait-desc order among equal PCT values, matching
	// Python's Timsort.
	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].pct > lines[j].pct
	})

	m.mu.Lock()
	m.lines = lines
	m.mu.Unlock()

	// Cache this sample for the next realtime diff (both modes update it, as the
	// original saved last_event_result unconditionally).
	m.lastEvents = snapshotEvents(rows)
}

// computeDiffs returns (cpu_time_diff, total_time_diff) for the active mode.
// Cumulative uses absolute values; realtime differences against the last sample.
func (m *EventMonitor) computeDiffs(realtime bool, curCPU, curTotal int64) (int64, int64) {
	if !realtime {
		return curCPU, curTotal + curCPU
	}
	cpuDiff := curCPU - m.lastCPU
	return cpuDiff, curTotal - m.lastTotal + cpuDiff
}

// sumTotalWaitTime sums column 3 (total_wait_time) over every row, the analogue of
// sum(zip(*sql_result)[3]).
func sumTotalWaitTime(rows []dbconn.Row) int64 {
	var total int64
	for _, row := range rows {
		v, _ := row.Int(3)
		total += v
	}
	return total
}

// buildDBCPULine synthesises the "DB CPU" row: waits/time/class are blank, PCT is
// cpu_time_diff / total_time_diff rounded to four decimals.
func buildDBCPULine(cpuDiff, totalDiff int64) eventLine {
	pct := round4(float64(cpuDiff) / float64(totalDiff))
	return eventLine{
		cols: [6]string{"DB CPU", "", intStr(cpuDiff), "", formatPct(pct), ""},
		pct:  pct,
	}
}

// buildCumulativeLines maps each row to absolute waits/time/avg columns. PCT here is
// rounded to two decimals, matching the original's round(..., 2) in cumulative mode.
func buildCumulativeLines(rows []dbconn.Row, totalDiff int64) []eventLine {
	lines := make([]eventLine, 0, len(rows))
	for _, row := range rows {
		event := row.Str(1)
		waits, _ := row.Int(2)
		timeUs, _ := row.Int(3)
		avg, _ := row.Float(4)
		class := row.Str(5)

		pct := round2(float64(timeUs) / float64(totalDiff))
		lines = append(lines, eventLine{
			cols: [6]string{event, intStr(waits), intStr(timeUs), pyFloat(avg), formatPct(pct), class},
			pct:  pct,
		})
	}
	return lines
}

// buildRealtimeLines maps each row to its delta against the previous sample. AVG is
// Δtime/Δwaits and PCT is Δtime/total_time_diff (four decimals); when either delta is
// non-positive both collapse to zero, as in the original.
func buildRealtimeLines(rows []dbconn.Row, totalDiff int64, last map[string]eventSample) []eventLine {
	lines := make([]eventLine, 0, len(rows))
	for _, row := range rows {
		event := row.Str(1)
		curWaits, _ := row.Int(2)
		curTime, _ := row.Int(3)
		class := row.Str(5)

		prev := last[event] // zero value when the event is new, i.e. diff against 0
		waitsDiff := curWaits - prev.waits
		timeDiff := curTime - prev.timeUs

		avg := "0"
		pct := 0.0
		if timeDiff > 0 && waitsDiff > 0 {
			avg = pyFloat(round2(float64(timeDiff) / float64(waitsDiff)))
			pct = round4(float64(timeDiff) / float64(totalDiff))
		}
		lines = append(lines, eventLine{
			cols: [6]string{event, intStr(waitsDiff), intStr(timeDiff), avg, formatPct(pct), class},
			pct:  pct,
		})
	}
	return lines
}

// snapshotEvents captures the current raw counters per event for the next realtime
// diff, replacing (never mutating) the previous cache.
func snapshotEvents(rows []dbconn.Row) map[string]eventSample {
	out := make(map[string]eventSample, len(rows))
	for _, row := range rows {
		event := row.Str(1)
		waits, _ := row.Int(2)
		timeUs, _ := row.Int(3)
		out[event] = eventSample{waits: waits, timeUs: timeUs}
	}
	return out
}

// Draw paints the reverse-video header (with the RT/C suffix on the first column)
// and up to height-1 data rows.
func (m *EventMonitor) Draw(screen tcell.Screen) {
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

// drawHeader writes row 0. The first item gets a "(RT)" or "(C)" suffix per mode.
func (m *EventMonitor) drawHeader() {
	header := model.Style{Pair: model.PairReverse}
	x := 0
	for i, item := range m.items {
		text := item
		if i == 0 {
			if m.currImmediate {
				text = item + "(RT)"
			} else {
				text = item + "(C)"
			}
		}
		m.pad.AddStr(0, x, text, header)
		if i < len(m.widths) {
			x += m.widths[i]
		}
	}
}

// drawRows writes the data rows starting at row 1, capped at height-1 rows, each
// cell aligned to its column and truncated to width-1 so it cannot bleed into the
// next column.
func (m *EventMonitor) drawRows() {
	limit := m.height - 1
	for i, line := range m.lines {
		if i >= limit {
			break
		}
		x := 0
		for j, cell := range line.cols {
			if j >= len(m.widths) {
				break
			}
			w := m.widths[j]
			m.pad.AddStr(i+1, x, truncateCell(cell, w-1), model.Normal)
			x += w
		}
	}
}

// truncateCell clips text to at most w runes, returning empty for a non-positive w.
func truncateCell(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w])
}

// formatPct renders a fraction as a percentage with two decimals, matching the
// original's f"{value:.2%}".
func formatPct(v float64) string {
	return fmt.Sprintf("%.2f%%", v*100)
}

// intStr renders an integer counter without a decimal point, matching f"{int}".
func intStr(v int64) string {
	return strconv.FormatInt(v, 10)
}

// round4 rounds to four decimal places, the companion of db.go's round2.
func round4(x float64) float64 {
	return math.Round(x*10000) / 10000
}
