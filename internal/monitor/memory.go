package monitor

import (
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
)

const (
	memoryName   = "memory"
	memoryHeight = 43
	memoryConfig = "memory.cfg"

	memPanel1Title = "TOP 5 DYNAMIC GLOBAL MEMORY"
	memPanel2Title = "TOP 10 SESSION MEMORY"
	memPanel3Title = "TOP 10 THREAD MEMORY"

	// memDefaultInterval is the fallback refresh cadence when main.mem_interval is
	// missing or non-positive (which the original never created the monitor for).
	memDefaultInterval = 30
)

// Emergency memory-full types shared with the emergency subsystem, matching the
// EMER_* constants in monitor/memory.py.
const (
	EmerNull                    = 0
	EmerDynamicGlobalMemoryFull = 1
	EmerSessionThreadMemoryFull = 2
)

// Memory dashboard SQL, preserved verbatim from monitor/memory.py.
const (
	memSummaryQuery = `SELECT memorytype, memorymbytes FROM pg_catalog.pv_total_memory_detail();`

	memDynamicQuery = `
            SELECT
                contextname,
                SUM(totalsize)/1024/1024 AS "TOTAL",
                SUM(freesize)/1024/1024 AS "FREE"
            FROM GS_SHARED_MEMORY_DETAIL
            WHERE totalsize <> 0
            GROUP BY contextname ORDER BY 2 DESC;`

	memSessionQuery = `SELECT substring(sessid FROM '\.(.*)$') AS "sessid",
        threadid, contextname, level, parent, totalsize, freesize, usedsize
        FROM GS_SESSION_MEMORY_CONTEXT;
        `

	memThreadQuery = `SELECT * FROM GS_THREAD_MEMORY_CONTEXT;`
)

// memPanel is one dashboard panel: a title plus a transposed table of columns
// (header/width) and rows (value). Port of the per-panel dict
// {title, header, width, value} used in monitor/memory.py.
type memPanel struct {
	title  string
	header []string
	width  []int
	value  [][]any
}

// MemPanelData is an immutable snapshot of one panel for the emergency subsystem
// (get_monitor_panels). Value cells keep their native types so numeric thresholds
// can be evaluated without re-parsing.
type MemPanelData struct {
	Title  string
	Header []string
	Width  []int
	Value  [][]any
}

// MemoryMonitor is the self-refreshing memory dashboard: four panels driven by a
// dedicated goroutine at main.mem_interval. Unlike the resident panels it is not
// part of the app's monitors slice. Port of monitor/memory.py.
type MemoryMonitor struct {
	base

	items  []string
	widths []int

	panels         [4]memPanel
	memoryFullType int
	cursorY        int
	screen         tcell.Screen

	done     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// NewMemoryMonitor builds the memory dashboard.
func NewMemoryMonitor(deps Deps) *MemoryMonitor {
	return &MemoryMonitor{
		base:    newBase(memoryName, memoryHeight, deps),
		cursorY: -1,
	}
}

// Init lays out the panel, parses memory.cfg, seeds the four panels, and starts
// the background refresh goroutine. Port of MemoryMonitor.init.
func (m *MemoryMonitor) Init(beginX, beginY, width int) {
	m.base.Init(beginX, beginY, width)
	if err := m.parseConfig(); err != nil {
		m.deps.Logger.Error("parse memory.cfg failed: %v", err)
	}
	m.panels = [4]memPanel{
		{title: ""},
		{title: memPanel1Title},
		{title: memPanel2Title},
		{title: memPanel3Title},
	}
	m.done = make(chan struct{})
	m.wg.Add(1)
	go m.loop()
}

// parseConfig reads memory.cfg lines of the form item:width.
func (m *MemoryMonitor) parseConfig() error {
	lines, err := readCfgLines(cfgPath(m.deps, memoryConfig))
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

// loop refreshes and renders every mem_interval until Stop closes done. It mirrors
// MemoryMonitor.wrapper, including the elapsed-time compensation and the
// swallow-and-log behaviour around each cycle.
func (m *MemoryMonitor) loop() {
	defer m.wg.Done()
	for {
		start := time.Now()
		m.cycle()
		select {
		case <-m.done:
			return
		default:
		}
		sleep := m.memInterval() - time.Since(start)
		if sleep < 0 {
			sleep = 0
		}
		timer := time.NewTimer(sleep)
		select {
		case <-m.done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// cycle runs one refresh+render, recovering from panics the way the Python wrapper
// caught every exception so the loop keeps running.
func (m *MemoryMonitor) cycle() {
	defer func() {
		if r := recover(); r != nil {
			m.deps.Logger.Error("Memory Monitor recovered from panic: %v", r)
		}
	}()
	m.Refresh()
	m.renderAndShow()
}

// Refresh rebuilds the summary and dynamic panels every cycle and the session and
// thread panels only when the dynamic-memory throttle allows it. Port of
// MemoryMonitor.refresh.
func (m *MemoryMonitor) Refresh() {
	p0 := m.refreshSummaryInfo()
	p1 := m.refreshDynamicInfo()
	m.mu.Lock()
	m.panels[0] = p0
	m.panels[1] = p1
	m.mu.Unlock()

	if m.deps.Health.ShouldRefreshMemory("memory") {
		p2 := m.refreshSessionInfo()
		p3 := m.refreshThreadInfo()
		m.mu.Lock()
		m.panels[2] = p2
		m.panels[3] = p3
		m.mu.Unlock()
	}
}

// Draw attaches screen and renders once, letting the app force a frame (e.g. when
// switching into the memory view). The app is responsible for the subsequent
// Show; the background goroutine flushes on its own via renderAndShow.
func (m *MemoryMonitor) Draw(screen tcell.Screen) {
	m.mu.Lock()
	m.screen = screen
	m.mu.Unlock()
	m.render()
}

// GetPanels returns a deep-copied snapshot of the four panels for the emergency
// subsystem. Port of get_monitor_panels.
func (m *MemoryMonitor) GetPanels() [4]MemPanelData {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [4]MemPanelData
	for i := range m.panels {
		out[i] = m.panels[i].snapshot()
	}
	return out
}

// SetMemoryFullType records the emergency highlight mode. Port of set_memory_full_type.
func (m *MemoryMonitor) SetMemoryFullType(t int) {
	m.mu.Lock()
	m.memoryFullType = t
	m.mu.Unlock()
}

// SetCursor records the app's current selection row so render can highlight it.
func (m *MemoryMonitor) SetCursor(y int) {
	m.mu.Lock()
	m.cursorY = y
	m.mu.Unlock()
}

// Stop signals the refresh goroutine to exit and waits for it. Port of MemoryMonitor.stop.
func (m *MemoryMonitor) Stop() {
	m.stopOnce.Do(func() {
		m.deps.Logger.Warning("The memory monitor refresh thread is starting to exit.")
		if m.done != nil {
			close(m.done)
		}
		m.wg.Wait()
		m.deps.Logger.Warning("The memory monitor refresh thread has exited.")
	})
}

// memInterval is the refresh cadence (main.mem_interval seconds), guarded against
// a non-positive value that would otherwise spin the loop.
func (m *MemoryMonitor) memInterval() time.Duration {
	secs := m.deps.Cfg.GetInt("main.mem_interval", memDefaultInterval)
	if secs <= 0 {
		secs = memDefaultInterval
	}
	return time.Duration(secs) * time.Second
}
