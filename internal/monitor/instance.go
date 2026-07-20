package monitor

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

const (
	instanceName   = "instance"
	instanceHeight = 2
	instanceConfig = "instance.cfg"

	// getMaxConnectionsSQL is queried once at Init to size the CONNECTION cell.
	getMaxConnectionsSQL = "SHOW max_connections;"

	// snValueIndex is the fixed column holding the SN (total session) count,
	// which the CONNECTION cell reads back, matching the Python monitor_value[1].
	snValueIndex = 1
)

// threadpoolRe extracts the "actual: N idle: M" pair from each worker_info row of
// GLOBAL_THREADPOOL_STATUS. Kept identical to the Python regex.
var threadpoolRe = regexp.MustCompile(`actual:\s*(\d+)\s+idle:\s*(\d+)`)

// InstanceMonitor renders the two-row instance summary (sessions, TPS/QPS,
// response-time percentiles, xlog throughput, connection and thread-pool usage,
// disk MB/s). Port of monitor/instance.py.
type InstanceMonitor struct {
	base

	items     []string
	widths    []int
	methods   []string
	logItems  []string
	logWidths []int

	values []string

	maxConnections int

	// Per-metric delta caches. These are only ever touched on the single refresh
	// goroutine, so they need no locking; they mirror the Python tmp_value slots.
	lastTime time.Time
	interval float64
	prevTPS  int64
	prevQPS  int64
	prevXlog uint64

	// ioRecord is the latest MB/s sample produced by the background pidstat
	// goroutine; guarded by ioMu because it crosses goroutines.
	ioMu     sync.Mutex
	ioRecord string
}

// NewInstanceMonitor builds the instance panel.
func NewInstanceMonitor(deps Deps) *InstanceMonitor {
	return &InstanceMonitor{base: newBase(instanceName, instanceHeight, deps)}
}

// Init lays out the panel, parses instance.cfg, reads max_connections once, and
// starts the background disk-IO sampler.
func (m *InstanceMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse instance.cfg failed: %v", err)
	}
	m.values = make([]string, len(m.items))
	for i := range m.values {
		m.values[i] = "0"
	}
	m.ioRecord = "None"
	m.initMaxConnections()

	go m.ioRefresher(m.deps.Cfg.GetInt("main.interval", 3))
}

// parseConfig reads instance.cfg lines of the form
// item:width:sql:log_item:log_width (five fields; the SQL never contains a colon).
func (m *InstanceMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, instanceConfig))
	if err != nil {
		return err
	}
	for _, line := range lines {
		f := strings.SplitN(line, ":", 5)
		for len(f) < 5 {
			f = append(f, "")
		}
		m.items = append(m.items, f[0])
		m.widths = append(m.widths, atoiOrZero(f[1]))
		m.methods = append(m.methods, f[2])
		m.logItems = append(m.logItems, f[3])
		m.logWidths = append(m.logWidths, atoiOrZero(f[4]))
	}
	return nil
}

// initMaxConnections runs SHOW max_connections once. The Python raised and aborted
// startup on failure; Init cannot return an error, so we log and leave it at zero,
// which the CONNECTION cell then guards against dividing by.
func (m *InstanceMonitor) initMaxConnections() {
	rows := m.deps.DB.Query(getMaxConnectionsSQL)
	if len(rows) == 0 {
		m.deps.Logger.Error("Failed to get max connections")
		return
	}
	if n, ok := rows[0].Int(0); ok {
		m.maxConnections = int(n)
	}
}

// Refresh recomputes every cell for the current cycle. A failed query aborts the
// whole cycle (as in the Python refresh), leaving the previous snapshot intact.
func (m *InstanceMonitor) Refresh() {
	values := make([]string, len(m.items))
	for i := range values {
		values[i] = "0"
	}

	var sessionRow dbconn.Row
	for i, item := range m.items {
		if !m.refreshCell(i, item, values, &sessionRow) {
			return
		}
	}

	m.mu.Lock()
	m.values = values
	m.mu.Unlock()

	m.checkAndReportAlarm(m.items, values)
}

// refreshCell resolves one cell, returning false when a query failed and the whole
// refresh must be abandoned.
func (m *InstanceMonitor) refreshCell(i int, item string, values []string, sessionRow *dbconn.Row) bool {
	switch item {
	case "time":
		return m.refreshTime(i)
	case "SN":
		return m.refreshSession(i, values, sessionRow)
	case "AN", "ASC", "ASI", "IDL":
		values[i] = sessionRow.Str(sessionIndex(item))
		return true
	case "MBPS":
		values[i] = m.readIORecord()
		return true
	case "TPS", "QPS":
		return m.refreshCounter(i, item, values)
	case "P80(ms)", "P95(ms)":
		return m.refreshPercentile(i, values)
	case "XLOG(kB/s)":
		return m.refreshXlog(i, values)
	case "CONNECTION(c/m)":
		m.setConnection(i, values)
		return true
	case "THREADPOOL":
		return m.refreshThreadpool(i, values)
	default:
		return true
	}
}

// refreshTime updates the sampling interval from consecutive current_timestamp(3)
// readings. The time cell itself carries no display value.
func (m *InstanceMonitor) refreshTime(i int) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	if len(rows) == 0 {
		return true
	}
	ts, ok := rows[len(rows)-1].Time(0)
	if !ok {
		return true
	}
	if !m.lastTime.IsZero() {
		m.interval = ts.Sub(m.lastTime).Seconds()
	}
	m.lastTime = ts
	return true
}

// refreshSession runs the combined session-count query and fills the SN cell; the
// AN/ASC/ASI/IDL cells later read the same row.
func (m *InstanceMonitor) refreshSession(i int, values []string, sessionRow *dbconn.Row) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	if len(rows) == 0 {
		m.deps.Logger.Error("Exec query for monitor_item %s returned no rows.", m.items[i])
		return false
	}
	*sessionRow = rows[0]
	values[i] = rows[0].Str(sessionIndex("SN"))
	return true
}

// refreshCounter turns a cumulative counter (TPS or QPS) into a per-second rate.
func (m *InstanceMonitor) refreshCounter(i int, item string, values []string) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	var cur int64
	if len(rows) > 0 {
		cur, _ = rows[len(rows)-1].Int(0)
	}
	prev := m.prevTPS
	if item == "QPS" {
		prev = m.prevQPS
	}
	values[i] = "0"
	if prev != 0 && cur >= prev && m.interval > 0 {
		values[i] = pyFloat(round2(float64(cur-prev) / m.interval))
	}
	if item == "QPS" {
		m.prevQPS = cur
	} else {
		m.prevTPS = cur
	}
	return true
}

// refreshPercentile renders a response-time percentile in milliseconds with five
// significant figures.
func (m *InstanceMonitor) refreshPercentile(i int, values []string) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	var v float64
	if len(rows) > 0 {
		v, _ = rows[len(rows)-1].Float(0)
	}
	values[i] = fmt.Sprintf("%.5g", v/1000)
	return true
}

// refreshXlog turns the current xlog insert LSN into a kB/s throughput.
func (m *InstanceMonitor) refreshXlog(i int, values []string) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	raw := ""
	if len(rows) > 0 {
		raw = rows[len(rows)-1].Str(0)
	}
	cur, ok := parseLSN(raw)
	if !ok {
		m.deps.Logger.Warning("Unparseable xlog LSN %q for %s", raw, m.items[i])
		values[i] = "0"
		return true
	}
	values[i] = "0"
	if m.prevXlog != 0 && cur >= m.prevXlog && m.interval > 0 {
		values[i] = pyFloat(round2(float64(cur-m.prevXlog) / 1024 / m.interval))
	}
	m.prevXlog = cur
	return true
}

// setConnection formats the connection-usage cell as "pct%(used/max)".
func (m *InstanceMonitor) setConnection(i int, values []string) {
	sn := values[snValueIndex]
	pct := 0.0
	if m.maxConnections > 0 {
		if snf, err := strconv.ParseFloat(strings.TrimSpace(sn), 64); err == nil {
			pct = round2(snf / float64(m.maxConnections) * 100)
		}
	}
	values[i] = pyFloat(pct) + "%(" + sn + "/" + strconv.Itoa(m.maxConnections) + ")"
}

// refreshThreadpool aggregates every worker group's actual/idle counts into a busy
// percentage, or "None" when no group reports.
func (m *InstanceMonitor) refreshThreadpool(i int, values []string) bool {
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		m.deps.Logger.Error("Exec query for monitor_item %s returned None.", m.items[i])
		return false
	}
	var actual, idle int64
	for _, row := range rows {
		match := threadpoolRe.FindStringSubmatch(row.Str(0))
		if match == nil {
			continue
		}
		a, _ := strconv.ParseInt(match[1], 10, 64)
		d, _ := strconv.ParseInt(match[2], 10, 64)
		actual += a
		idle += d
	}
	if actual != 0 {
		values[i] = pyFloat(round2(float64(actual-idle)/float64(actual)*100)) + "%"
	} else {
		values[i] = "None"
	}
	return true
}

// readIORecord returns the latest MB/s sample under the io lock.
func (m *InstanceMonitor) readIORecord() string {
	m.ioMu.Lock()
	defer m.ioMu.Unlock()
	return m.ioRecord
}

// setIORecord stores a new MB/s sample under the io lock.
func (m *InstanceMonitor) setIORecord(v string) {
	m.ioMu.Lock()
	m.ioRecord = v
	m.ioMu.Unlock()
}

// ioRefresher continuously reads pidstat output for the gaussdb process and keeps
// ioRecord updated with (kB_rd/s + kB_wr/s) in MB/s. Runs as a daemon goroutine.
func (m *InstanceMonitor) ioRefresher(interval int) {
	cmd := exec.Command("/bin/sh", "-c", ioCommand(interval))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.deps.Logger.Error("io refresh pipe failed: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		m.deps.Logger.Error("io refresh start failed: %v", err)
		return
	}
	defer func() {
		if err := cmd.Wait(); err != nil {
			m.deps.Logger.Warning("io refresh thread exit, err: %v", err)
		}
	}()

	m.scanIO(stdout, interval)
}

// scanIO parses each pidstat line, tracking the read/write columns from the header
// row and publishing every data row's throughput.
func (m *InstanceMonitor) scanIO(stdout io.Reader, interval int) {
	rdCol, wrCol := 0, 0
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) <= 7 {
			continue
		}
		if fields[len(fields)-1] == "Command" {
			rdCol, wrCol = findIOCols(fields, rdCol, wrCol)
			continue
		}
		rd, okr := parseFloatField(fields, rdCol)
		wr, okw := parseFloatField(fields, wrCol)
		if !okr || !okw {
			m.deps.Logger.Warning("io refresh parse failed: %v", fields)
			continue
		}
		m.setIORecord(pyFloat(round2((rd + wr) / 1024)))
		time.Sleep(time.Duration(interval) * time.Second)
	}
	if err := scanner.Err(); err != nil {
		m.deps.Logger.Warning("io refresh scanner error: %v", err)
	}
}

// Draw renders the header bar on row 0 (reverse video) and the values on row 1,
// each column left-justified within its width and clipped to width-1.
func (m *InstanceMonitor) Draw(screen tcell.Screen) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pad == nil {
		return
	}
	m.pad.Clear()
	header := model.Style{Pair: model.PairReverse}
	x := 0
	for i, item := range m.items {
		w := m.widths[i]
		m.pad.AddStr(0, x, padColumn(item, w), header)
		value := ""
		if i < len(m.values) {
			value = m.values[i]
		}
		m.pad.AddStr(1, x, padColumn(value, w), model.Normal)
		x += w
	}
	m.blit(screen)
}

// MonitorValues returns a copy of the current cell values for the emergency
// subsystem (get_monitor_value); index 12 is CONNECTION, 13 is THREADPOOL.
func (m *InstanceMonitor) MonitorValues() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.values...)
}

// LogFields returns the loggable columns (everything after the time cell) for the
// persistence thread, matching print's message_queue.put of log_item[1:] etc.
func (m *InstanceMonitor) LogFields() (items []string, values []string, widths []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	const skip = 1
	if len(m.logItems) <= skip || len(m.values) <= skip {
		return nil, nil, nil
	}
	return append([]string(nil), m.logItems[skip:]...),
		append([]string(nil), m.values[skip:]...),
		append([]int(nil), m.logWidths[skip:]...)
}

// sessionIndex maps a session cell name to its column in the combined SN query.
func sessionIndex(item string) int {
	switch item {
	case "SN":
		return 0
	case "AN":
		return 1
	case "ASC":
		return 2
	case "ASI":
		return 3
	case "IDL":
		return 4
	}
	return -1
}

// parseLSN parses a "high/low" hex LSN into a single 64-bit position.
func parseLSN(s string) (uint64, bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, false
	}
	high, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 16, 64)
	low, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 16, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return (high << 32) | low, true
}

// ioCommand builds the pidstat pipeline that samples the gaussdb process's disk IO.
func ioCommand(interval int) string {
	return fmt.Sprintf(
		"pidstat -d -p $(ps -ux | grep bin/gaussdb | grep -v grep | awk '{print $2}') %d",
		interval)
}

// findIOCols locates the kB_rd/s and kB_wr/s columns in a pidstat header row,
// keeping the previous index for any column not present.
func findIOCols(fields []string, rd, wr int) (int, int) {
	for i, name := range fields {
		switch name {
		case "kB_rd/s":
			rd = i
		case "kB_wr/s":
			wr = i
		}
	}
	return rd, wr
}

// parseFloatField parses fields[idx] as a float, reporting false when out of range
// or non-numeric.
func parseFloatField(fields []string, idx int) (float64, bool) {
	if idx < 0 || idx >= len(fields) {
		return 0, false
	}
	f, err := strconv.ParseFloat(fields[idx], 64)
	return f, err == nil
}

// padColumn left-justifies text within a column of the given width, clipping the
// content to width-1 so adjacent columns always keep a one-space separator.
func padColumn(text string, width int) string {
	if width <= 0 {
		return ""
	}
	r := []rune(text)
	if len(r) > width-1 {
		r = r[:width-1]
	}
	return string(r) + strings.Repeat(" ", width-len(r))
}
