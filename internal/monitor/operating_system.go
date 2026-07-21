package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/model"
)

const (
	osName   = "os"
	osHeight = 2
	osConfig = "operating_system.cfg"

	// diskFields is the number of /proc/diskstats counters tracked per sample:
	// reads, sectors read, writes, sectors written, read time, write time.
	diskFields = 6
	// cpuFields is the number of /proc/stat cpu columns tracked (user, nice,
	// system, idle, iowait, irq, softirq).
	cpuFields = 7
	// osCmdCount is the number of base shell probes (diskstats, stat, meminfo,
	// uptime).
	osCmdCount = 4
	// defaultInterval mirrors the gstop.cfg main.interval default of 3 seconds,
	// used as the iostat sampling period.
	defaultInterval = 3
)

// OSMonitor renders the two-row operating-system panel: a header row of item
// names over a value row of load, CPU%, memory%, and per-device IO rates.
// Port of monitor/operating_system.py, including the /proc diff sampling and the
// background iostat reader that tracks average queue size (aqu-sz).
type OSMonitor struct {
	base

	items      []string
	widths     []int
	valueTypes []string
	logItems   []string
	logWidths  []int

	values []string

	physicalDevices []string
	multiDisk       bool
	osCmd           []string

	osValues [osCmdCount]string
	osOK     [osCmdCount]bool

	prevDiskstats [diskFields]int64
	curDiskstats  [diskFields]int64
	lastDiskTime  time.Time
	hasDiskTime   bool

	lastCPUInfo [cpuFields]int64
	curCPUInfo  [cpuFields]int64
	lastCPUTime time.Time
	hasCPUTime  bool

	aquMu sync.Mutex
	aquSz float64
}

// NewOSMonitor builds the operating-system panel.
func NewOSMonitor(deps Deps) *OSMonitor {
	return &OSMonitor{base: newBase(osName, osHeight, deps)}
}

// Init lays out the panel, parses operating_system.cfg, discovers the physical
// disks backing the data directory, builds the probe commands, and starts the
// background iostat reader.
func (m *OSMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse operating_system.cfg failed: %v", err)
	}
	m.values = make([]string, len(m.items))
	for i := range m.values {
		m.values[i] = "0"
	}

	m.initPhysicalDevices()
	if len(m.physicalDevices) == 0 {
		m.deps.Logger.Error("Get GaussDB physical device failed")
	}
	m.buildCommands()

	if len(m.physicalDevices) > 0 {
		interval := m.deps.Cfg.GetInt("main.interval", defaultInterval)
		go m.ioRefresher(interval)
	}
}

// parseConfig reads operating_system.cfg lines of the form
// item:width:value_type:log_item:log_width.
func (m *OSMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, osConfig))
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
		m.valueTypes = append(m.valueTypes, f[2])
		m.logItems = append(m.logItems, f[3])
		m.logWidths = append(m.logWidths, atoiOrZero(f[4]))
	}
	return nil
}

// initPhysicalDevices resolves the gaussdb data directory to its backing block
// device, then walks lsblk parents down to the physical disks. On any failure it
// logs and leaves physicalDevices empty so the panel degrades to zeros.
func (m *OSMonitor) initPhysicalDevices() {
	dataPath, ok := m.dataPath()
	if !ok {
		return
	}
	dfCmd := fmt.Sprintf("df -P %s | awk 'NR==2 {print $1}'", dataPath)
	device, ok := m.deps.OS.Run(dfCmd, true)
	if !ok {
		m.deps.Logger.Error("Get GaussDB device failed, cmd: %s", dfCmd)
		return
	}
	devices, err := m.resolvePhysicalDevices(device)
	if err != nil {
		m.deps.Logger.Error("Get GaussDB physical device failed: %v", err)
		return
	}
	m.physicalDevices = devices
}

// dataPath finds the gaussdb -D data directory, applying the /var/chroot fallback
// used when the process runs in a chroot.
func (m *OSMonitor) dataPath() (string, bool) {
	cmd := "ps -ux | grep bin/gaussdb | grep -v grep | awk 'match($0, /-D ([^ ]+)/, a) {print a[1]}'"
	out, ok := m.deps.OS.Run(cmd, true)
	if !ok || out == "" {
		m.deps.Logger.Error("Get GaussDB data path failed, cmd: %s", cmd)
		return "", false
	}
	if osPathExists(out) {
		return out, true
	}
	chroot := "/var/chroot" + out
	if osPathExists(chroot) {
		return chroot, true
	}
	m.deps.Logger.Error("Get GaussDB real data path failed, check path: %s", chroot)
	return "", false
}

// resolvePhysicalDevices returns the sorted physical disks that back devicePath,
// walking the lsblk NAME/TYPE/PKNAME graph (which handles LVM/dm layering).
func (m *OSMonitor) resolvePhysicalDevices(devicePath string) ([]string, error) {
	devName := filepath.Base(devicePath)
	out, ok := m.deps.OS.Run("lsblk -nl -o NAME,TYPE,PKNAME", true)
	if !ok {
		return nil, fmt.Errorf("lsblk command returned none")
	}
	childToParents, nameToType := parseLsblk(out)
	return traverseDisks(devName, childToParents, nameToType), nil
}

// parseLsblk turns `lsblk -nl -o NAME,TYPE,PKNAME` output into a child->parents
// adjacency map and a name->type lookup.
func parseLsblk(output string) (map[string][]string, map[string]string) {
	childToParents := map[string][]string{}
	nameToType := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(strings.TrimSpace(line))
		var name, typ, pkname string
		switch len(parts) {
		case 2:
			name, typ = parts[0], parts[1]
		case 3:
			name, typ, pkname = parts[0], parts[1], parts[2]
		default:
			continue
		}
		nameToType[name] = typ
		if pkname != "" {
			childToParents[name] = append(childToParents[name], pkname)
		}
	}
	return childToParents, nameToType
}

// traverseDisks performs the breadth-first parent walk from start, collecting
// every ancestor whose type is "disk".
func traverseDisks(start string, childToParents map[string][]string, nameToType map[string]string) []string {
	queue := []string{start}
	visited := map[string]bool{}
	disks := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current] {
			continue
		}
		visited[current] = true

		parents := childToParents[current]
		if len(parents) == 0 {
			if nameToType[current] == "disk" {
				disks[current] = true
			}
			continue
		}
		for _, parent := range parents {
			if nameToType[parent] == "disk" {
				disks[parent] = true
			} else {
				queue = append(queue, parent)
			}
		}
	}
	result := make([]string, 0, len(disks))
	for d := range disks {
		result = append(result, d)
	}
	sort.Strings(result)
	return result
}

// buildCommands assembles the four base probes and records the multi-disk flag.
func (m *OSMonitor) buildCommands() {
	m.multiDisk = len(m.physicalDevices) > 1
	m.osCmd = make([]string, osCmdCount)
	if len(m.physicalDevices) > 0 {
		m.osCmd[0] = diskstatsCommand(m.physicalDevices)
		m.deps.Logger.Info("GaussDB physical device: %s", m.osCmd[0])
	}
	m.osCmd[1] = "head -n 1 /proc/stat"
	m.osCmd[2] = "awk '/MemTotal/ {total=$2} /MemAvailable/ {available=$2} END {used=total-available; print (used/total)*100}' /proc/meminfo"
	m.osCmd[3] = "uptime | awk -F'load average: ' '{print $2}' | awk '{print $1}' | sed 's/,//'"
}

// diskstatsCommand builds the awk filter selecting the tracked devices' lines
// from /proc/diskstats (matching on field $3, the device name).
func diskstatsCommand(devices []string) string {
	conds := make([]string, 0, len(devices))
	for _, dev := range devices {
		conds = append(conds, fmt.Sprintf("$3 == \"%s\"", filepath.Base(dev)))
	}
	pattern := strings.Join(conds, " || ")
	return fmt.Sprintf("awk '%s' /proc/diskstats", pattern)
}

// Refresh runs the base probes, updates the /proc diffs, recomputes every cell,
// and reports any threshold breaches.
func (m *OSMonitor) Refresh() {
	m.RefreshContext(context.Background())
}

func (m *OSMonitor) RefreshContext(ctx context.Context) {
	now := time.Now()
	m.runCommands(ctx)
	m.buildDiskstats()
	m.buildCPUInfo()

	tmp := make([]string, len(m.items))
	for i := range m.items {
		tmp[i] = m.computeValue(i, now)
	}

	m.mu.Lock()
	m.values = tmp
	m.mu.Unlock()

	if m.osOK[0] {
		m.lastDiskTime = now
		m.hasDiskTime = true
		m.prevDiskstats = m.curDiskstats
	}
	if m.osOK[1] {
		m.lastCPUTime = now
		m.hasCPUTime = true
		m.lastCPUInfo = m.curCPUInfo
	}

	m.checkAndReportAlarm(m.items, tmp)
}

// runCommands executes each base probe, recording its trimmed output and success
// flag. An empty command (no devices discovered) is treated as a failed probe.
func (m *OSMonitor) runCommands(ctx context.Context) {
	for i, cmd := range m.osCmd {
		if cmd == "" {
			m.osValues[i] = ""
			m.osOK[i] = false
			continue
		}
		out, ok := m.deps.OS.RunContext(ctx, cmd, true)
		m.osValues[i] = out
		m.osOK[i] = ok
	}
}

// buildDiskstats parses the current /proc/diskstats sample into curDiskstats,
// summing across all devices when multiple disks back the data directory.
func (m *OSMonitor) buildDiskstats() {
	var stats [diskFields]int64
	if m.osOK[0] {
		if m.multiDisk {
			for _, line := range strings.Split(m.osValues[0], "\n") {
				addDiskstatsRow(&stats, strings.Fields(line))
			}
		} else {
			setDiskstatsRow(&stats, strings.Fields(m.osValues[0]))
		}
	}
	m.curDiskstats = stats
}

// addDiskstatsRow accumulates one /proc/diskstats line into stats.
func addDiskstatsRow(stats *[diskFields]int64, f []string) {
	if len(f) < 11 {
		return
	}
	stats[0] += osParseInt64(f[3])  // reads completed
	stats[1] += osParseInt64(f[5])  // sectors read
	stats[2] += osParseInt64(f[7])  // writes completed
	stats[3] += osParseInt64(f[9])  // sectors written
	stats[4] += osParseInt64(f[6])  // time spent reading (ms)
	stats[5] += osParseInt64(f[10]) // time spent writing (ms)
}

// setDiskstatsRow stores one /proc/diskstats line into stats (single disk).
func setDiskstatsRow(stats *[diskFields]int64, f []string) {
	if len(f) < 11 {
		return
	}
	stats[0] = osParseInt64(f[3])
	stats[1] = osParseInt64(f[5])
	stats[2] = osParseInt64(f[7])
	stats[3] = osParseInt64(f[9])
	stats[4] = osParseInt64(f[6])
	stats[5] = osParseInt64(f[10])
}

// buildCPUInfo parses the `cpu` aggregate line of /proc/stat into curCPUInfo.
func (m *OSMonitor) buildCPUInfo() {
	var info [cpuFields]int64
	if m.osOK[1] {
		f := strings.Fields(m.osValues[1])
		for j := 0; j < cpuFields; j++ {
			if j+1 < len(f) {
				info[j] = osParseInt64(f[j+1])
			}
		}
	}
	m.curCPUInfo = info
}

// computeValue formats cell i according to its value_type.
func (m *OSMonitor) computeValue(i int, now time.Time) string {
	switch m.valueTypes[i] {
	case "IO":
		return m.computeIO(i, now)
	case "CPU":
		return m.computeCPU()
	case "MEM":
		return m.computeScalar(2)
	case "LOAD":
		return m.computeScalar(3)
	default:
		return "0"
	}
}

// computeIO derives one IO rate from the diskstats diff over the refresh interval.
func (m *OSMonitor) computeIO(i int, now time.Time) string {
	if !m.osOK[0] || !m.hasDiskTime {
		return ""
	}
	interval := now.Sub(m.lastDiskTime).Seconds()
	return pyFloat(round2(m.ioStat(m.items[i], interval)))
}

// computeCPU derives %CPU from the /proc/stat diff and publishes it to Health for
// the dynamic-memory throttle. Returns "0" until two samples exist.
func (m *OSMonitor) computeCPU() string {
	if !m.osOK[1] || !m.hasCPUTime {
		return ""
	}
	var total, busy int64
	for j := 0; j < cpuFields; j++ {
		d := m.curCPUInfo[j] - m.lastCPUInfo[j]
		total += d
		if j != 3 { // index 3 is idle
			busy += d
		}
	}
	cpu := 0.0
	if total != 0 {
		cpu = round2(float64(busy) / float64(total) * 100)
	}
	m.deps.Health.UpdateCPUUsage(cpu)
	return pyFloat(cpu)
}

// computeScalar formats a probe whose stdout is already a percentage/load number.
func (m *OSMonitor) computeScalar(idx int) string {
	if !m.osOK[idx] {
		return ""
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(m.osValues[idx]), 64)
	if err != nil {
		return "0"
	}
	return pyFloat(round2(f))
}

// ioStat reproduces get_io_stat: the field diffs and unit conversions for each
// IO metric. Sector counts are 512-byte units.
func (m *OSMonitor) ioStat(item string, interval float64) float64 {
	if interval == 0 {
		return 0
	}
	d := func(a int) float64 { return float64(m.curDiskstats[a] - m.prevDiskstats[a]) }
	switch item {
	case "r/s":
		return d(0) / interval
	case "w/s":
		return d(2) / interval
	case "rkB/s":
		return d(1) * 512 / interval / 1024
	case "wkB/s":
		return d(3) * 512 / interval / 1024
	case "r_asize(kB)":
		if d(0) > 0 {
			return d(1) / d(0) * 512 / interval / 1024
		}
		return 0
	case "w_asize(kB)":
		if d(2) > 0 {
			return d(3) / d(2) * 512 / interval / 1024
		}
		return 0
	case "r_await":
		if d(0) > 0 {
			return d(4) / d(0)
		}
		return 0
	case "w_await":
		if d(2) > 0 {
			return d(5) / d(2)
		}
		return 0
	case "aqu-sz":
		return m.getAquSz()
	default:
		m.deps.Logger.Error("item not defined.")
		return 0
	}
}

// ioRefresher streams `iostat -x <interval>` and accumulates aqu-sz across the
// tracked devices per report, mirroring the io_refresher thread.
func (m *OSMonitor) ioRefresher(interval int) {
	command := fmt.Sprintf("iostat -x %d", interval)
	cmd := exec.Command("/bin/sh", "-c", command)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		m.deps.Logger.Error("iostat stdout pipe failed: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		m.deps.Logger.Error("start iostat failed: %v", err)
		return
	}
	m.scanIostat(stdout)
	if err := cmd.Wait(); err != nil {
		m.deps.Logger.Warning("iostat exited: %v", err)
	}
}

// scanIostat parses the iostat stream line by line, summing the aqu-sz column for
// tracked devices between the "Device" header and the blank line that ends each
// report, then publishing the total.
func (m *OSMonitor) scanIostat(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var sum float64
	dataStart := false
	headerLen := 0
	aquIdx := -1

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if dataStart {
			if line == "" {
				dataStart = false
				m.setAquSz(sum)
				sum = 0
				continue
			}
			sum += m.rowAquSz(line, headerLen, aquIdx)
			continue
		}
		if strings.HasPrefix(line, "Device") {
			dataStart = true
			header := strings.Fields(line)
			headerLen = len(header)
			aquIdx = osIndexOf(header, "aqu-sz")
		}
	}
}

// rowAquSz returns the aqu-sz contribution of one iostat data line, or 0 when the
// line is malformed or its device is not tracked.
func (m *OSMonitor) rowAquSz(line string, headerLen, aquIdx int) float64 {
	parts := strings.Fields(line)
	if len(parts) != headerLen || aquIdx < 0 || aquIdx >= len(parts) {
		return 0
	}
	if !m.isPhysicalDevice(parts[0]) {
		return 0
	}
	v, err := strconv.ParseFloat(parts[aquIdx], 64)
	if err != nil {
		return 0
	}
	return v
}

// isPhysicalDevice reports whether name is one of the tracked disks. The slice is
// written once during Init before the reader goroutine starts, so it needs no
// lock here.
func (m *OSMonitor) isPhysicalDevice(name string) bool {
	for _, d := range m.physicalDevices {
		if d == name {
			return true
		}
	}
	return false
}

func (m *OSMonitor) setAquSz(v float64) {
	m.aquMu.Lock()
	m.aquSz = v
	m.aquMu.Unlock()
}

func (m *OSMonitor) getAquSz() float64 {
	m.aquMu.Lock()
	defer m.aquMu.Unlock()
	return m.aquSz
}

// Draw renders the white-background header row over the value row.
func (m *OSMonitor) Draw(screen tcell.Screen) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pad == nil {
		return
	}
	m.pad.Clear()
	m.drawHeader()
	m.drawValues()
	m.blit(screen)
}

// drawHeader fills row 0 with a white bar and lays out the item names.
func (m *OSMonitor) drawHeader() {
	header := model.Style{Pair: model.PairReverse}
	if m.width > 1 {
		m.pad.AddStr(0, 0, strings.Repeat(" ", m.width-1), header)
	}
	x := 0
	for i, item := range m.items {
		w := m.widthAt(i)
		m.pad.AddStr(0, x, osTruncate(item, w-1), header)
		x += w
	}
}

// drawValues lays out the value row beneath the header, column-aligned.
func (m *OSMonitor) drawValues() {
	x := 0
	for i, value := range m.values {
		w := m.widthAt(i)
		m.pad.AddStr(1, x, osTruncate(value, w-1), model.Normal)
		x += w
	}
}

// MonitorValues returns a copy of the current cell values for the emergency
// subsystem (get_monitor_value); index 1 is %CPU, 3/4 r/s-w/s, 7/8 await, 11 aqu-sz.
func (m *OSMonitor) MonitorValues() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.values...)
}

// LogFields returns every loggable column (skip=0) for the persistence thread.
func (m *OSMonitor) LogFields() (items []string, values []string, widths []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.logItems...),
		append([]string(nil), m.values...),
		append([]int(nil), m.logWidths...)
}

// widthAt returns the configured column width for cell i, or 0 if out of range.
func (m *OSMonitor) widthAt(i int) int {
	if i >= 0 && i < len(m.widths) {
		return m.widths[i]
	}
	return 0
}

// truncate clips s to at most n runes, returning "" when n <= 0.
func osTruncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// indexOf returns the position of target in items, or -1 if absent.
func osIndexOf(items []string, target string) int {
	for i, v := range items {
		if v == target {
			return i
		}
	}
	return -1
}

// pathExists reports whether p exists on disk, standing in for os.path.exists.
func osPathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// parseInt64 parses a base-10 integer field, returning 0 on any error.
func osParseInt64(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
