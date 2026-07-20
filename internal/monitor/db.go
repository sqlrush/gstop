package monitor

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

const (
	dbName   = "db"
	dbHeight = 1
	dbConfig = "db.cfg"
	// priRefreshEvery is how many refreshes the primary node is cached before a
	// re-query, matching the counter <= 1000 guard.
	priRefreshEvery = 1000
)

// busyResult holds one sample of (cpu_time, db_time, wall clock) from
// GS_INSTANCE_TIME, used to derive db% and WTR% from consecutive differences.
type busyResult struct {
	cpuTime float64
	dbTime  float64
	ts      time.Time
	valid   bool
}

// DBMonitor renders the single-row database summary: version, user, time,
// uptime, primary node, dynamic memory, plan cache, DB busy % and wait ratio.
// Port of monitor/db.py.
type DBMonitor struct {
	base

	items     []string
	widths    []int
	methods   []string
	osMethods []string
	logItems  []string
	logWidths []int

	values []string

	version     string
	user        string
	pcache      string
	primaryNode string
	counter     int

	busy     busyResult
	lastBusy []busyResult

	dbInfo *model.DBInfo
}

// NewDBMonitor builds the database panel.
func NewDBMonitor(deps Deps) *DBMonitor {
	return &DBMonitor{base: newBase(dbName, dbHeight, deps)}
}

// SetDBInfo attaches the shared version/user/role container filled during refresh.
func (m *DBMonitor) SetDBInfo(info *model.DBInfo) { m.dbInfo = info }

// Init lays out the panel and parses db.cfg.
func (m *DBMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse db.cfg failed: %v", err)
	}
	m.values = make([]string, len(m.items))
	m.lastBusy = make([]busyResult, len(m.items))
	for i := range m.values {
		m.values[i] = "0"
	}
}

// parseConfig reads db.cfg lines of the form
// item:width:sql:os_command:log_item:log_width.
func (m *DBMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, dbConfig))
	if err != nil {
		return err
	}
	for _, line := range lines {
		f := strings.SplitN(line, ":", 6)
		for len(f) < 6 {
			f = append(f, "")
		}
		m.items = append(m.items, f[0])
		m.widths = append(m.widths, atoiOrZero(f[1]))
		m.methods = append(m.methods, f[2])
		m.osMethods = append(m.osMethods, f[3])
		m.logItems = append(m.logItems, f[4])
		m.logWidths = append(m.logWidths, atoiOrZero(f[5]))
	}
	return nil
}

// Refresh recomputes every cell using the three-phase (SQL, shell, post-process)
// pipeline from the original.
func (m *DBMonitor) Refresh() {
	m.counter++
	tmp := make([]string, len(m.items))
	for i := range tmp {
		tmp[i] = "0"
	}

	for i, item := range m.items {
		if m.shortCircuit(i, item, tmp) {
			continue
		}
		processData := m.phaseQuery(i, item)
		m.phaseCommand(i, processData, tmp)
		m.phasePost(i, item, tmp)
	}

	m.mu.Lock()
	m.values = tmp
	m.mu.Unlock()
}

// shortCircuit handles the cached/short-cut cells and returns true when cell i is
// already resolved for this cycle.
func (m *DBMonitor) shortCircuit(i int, item string, tmp []string) bool {
	switch {
	case i == 0 && m.version != "":
		tmp[i] = m.version
		return true
	case i == 1 && m.user != "":
		tmp[i] = m.user
		return true
	case i == 2:
		tmp[i] = time.Now().Format("2006-01-02 15:04:05")
		return true
	case item == "PRI" && m.primaryNode != "" && m.counter <= priRefreshEvery:
		tmp[i] = m.primaryNode
		return true
	case item == "MB pcache":
		if !m.deps.Cfg.GetBool("main.dynamic_mem_enable", false) {
			tmp[i] = "0"
			return true
		}
		if !m.deps.Health.ShouldRefreshMemory("pcache") {
			tmp[i] = m.pcache
			return true
		}
	case item == "MB dyn":
		if !m.deps.Cfg.GetBool("main.dynamic_mem_enable", false) {
			tmp[i] = "0"
			return true
		}
	}
	return false
}

// phaseQuery runs the cell's SQL (if any) and returns the scalar text to feed the
// shell phase. For db% it caches the (cpu,db,ts) sample instead.
func (m *DBMonitor) phaseQuery(i int, item string) string {
	if m.methods[i] == "" {
		return ""
	}
	rows := m.deps.DB.Query(m.methods[i])
	if rows == nil {
		return ""
	}
	if item == "db%" {
		if len(rows) > 0 {
			m.busy = parseBusy(rows[0])
		}
		return ""
	}
	var processData string
	for _, row := range rows {
		processData = row.Str(0)
	}
	return processData
}

// phaseCommand runs the cell's shell command (if any), optionally piping the SQL
// scalar into it, and stores the raw output.
func (m *DBMonitor) phaseCommand(i int, processData string, tmp []string) {
	osMethod := m.osMethods[i]
	if processData != "" {
		if osMethod != "" {
			if out, ok := m.deps.OS.Run(fmt.Sprintf("echo \"%s\" | %s", processData, osMethod), true); ok {
				tmp[i] = out
			} else {
				tmp[i] = ""
			}
		} else {
			tmp[i] = processData
		}
		return
	}
	if osMethod != "" {
		if out, ok := m.deps.OS.Run(osMethod, true); ok {
			tmp[i] = out
		} else {
			tmp[i] = ""
		}
	}
}

// phasePost applies the per-cell Python post-processing: memory formatting,
// primary-node role detection, version/user caching, and the db%/WTR% deltas.
func (m *DBMonitor) phasePost(i int, item string, tmp []string) {
	switch {
	case item == "MB dyn":
		tmp[i] = formatMB(tmp[i])
	case item == "MB pcache":
		tmp[i] = formatMB(tmp[i])
		m.pcache = tmp[i]
	case item == "PRI":
		m.primaryNode = tmp[i]
		if m.dbInfo != nil {
			if m.isPrimary(m.primaryNode) {
				m.dbInfo.SetRole("primary")
			} else {
				m.dbInfo.SetRole("standby")
			}
		}
		m.counter = 0
	case i == 0:
		m.version = tmp[i]
		if m.dbInfo != nil {
			m.dbInfo.SetVersion(tmp[i])
		}
	case i == 1:
		m.user = tmp[i]
		if m.dbInfo != nil {
			m.dbInfo.SetUser(tmp[i])
		}
	case item == "db%" || item == "WTR%":
		tmp[i] = m.busyPercent(i, item, tmp[i])
	}
}

// busyPercent computes db% or WTR% from the delta against the previous sample.
func (m *DBMonitor) busyPercent(i int, item, current string) string {
	prev := m.lastBusy[i]
	result := "0"
	if prev.valid && m.busy.valid {
		cpu := m.busy.cpuTime - prev.cpuTime
		db := m.busy.dbTime - prev.dbTime
		tsDiffUS := m.busy.ts.Sub(prev.ts).Seconds() * 1_000_000
		switch {
		case item == "db%" && cpu > 0 && tsDiffUS > 0:
			if nproc, err := strconv.Atoi(strings.TrimSpace(current)); err == nil && nproc > 0 {
				result = pyFloat(round2(cpu / (tsDiffUS * float64(nproc)) * 100))
			}
		case item == "WTR%" && db > 0:
			result = pyFloat(round2((db - cpu) / db * 100))
		}
	}
	m.lastBusy[i] = m.busy
	return result
}

// isPrimary reports whether this host owns the primary node's address.
func (m *DBMonitor) isPrimary(node string) bool {
	if node == "" {
		return false
	}
	out, ok := m.deps.OS.Run("ifconfig | grep -w "+node, true)
	return ok && strings.TrimSpace(out) != ""
}

// Draw renders the single row of "value item" pairs.
func (m *DBMonitor) Draw(screen tcell.Screen) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pad == nil {
		return
	}
	m.pad.Clear()
	x := 0
	for i, value := range m.values {
		item := ""
		if i < len(m.items) {
			item = m.items[i]
		}
		m.pad.AddStr(0, x, value+" "+item, model.Normal)
		if i < len(m.widths) {
			x += m.widths[i]
		}
	}
	m.blit(screen)
}

// MonitorValues returns a copy of the current cell values, exposing the panel's
// state to the emergency subsystem (get_monitor_value).
func (m *DBMonitor) MonitorValues() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.values...)
}

// LogFields returns the loggable columns (from the PRI cell onward) for the
// persistence thread, matching print's message_queue.put of log_item[4:] etc.
func (m *DBMonitor) LogFields() (items []string, values []string, widths []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	const skip = 4
	if len(m.logItems) <= skip {
		return nil, nil, nil
	}
	return append([]string(nil), m.logItems[skip:]...),
		append([]string(nil), m.values[skip:]...),
		append([]int(nil), m.logWidths[skip:]...)
}

func parseBusy(row dbconn.Row) busyResult {
	cpu, _ := row.Float(0)
	db, _ := row.Float(1)
	ts, _ := row.Time(2)
	return busyResult{cpuTime: cpu, dbTime: db, ts: ts, valid: true}
}

// formatMB rounds a raw MB value to two decimals and renders it with six
// significant figures, matching '{:.6g}'.format(round(x, 2)).
func formatMB(raw string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return "0"
	}
	return fmt.Sprintf("%.6g", round2(f))
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// pyFloat renders a float the way Python's str() does, always keeping a decimal
// point for whole numbers (e.g. 3 -> "3.0"), so db%/WTR% cells read as before.
func pyFloat(x float64) string {
	s := strconv.FormatFloat(x, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}
