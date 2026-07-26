package app

import (
	"sync/atomic"
	"time"

	"gstop/internal/alarm"
	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/emergency"
	"gstop/internal/health"
	"gstop/internal/healthdash"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/monitor"
	"gstop/internal/oscmd"
	"gstop/internal/persist"
	"gstop/internal/tui"
)

// App coordinates the monitors, the background refresher, and the TUI event loop.
// It is the top-level equivalent of gstop.py's gstop_main_routine.
type App struct {
	cfg    *config.Config
	logger *logging.Logger
	alarm  *alarm.Alarm
	health *health.Health
	db     *dbconn.DB
	os     *oscmd.Runner

	screen   *tui.Screen // nil in daemon mode
	daemon   bool
	monitors []monitor.Monitor
	dbMon    *monitor.DBMonitor
	osMon    *monitor.OSMonitor
	insMon   *monitor.InstanceMonitor
	event    *monitor.EventMonitor
	session  *monitor.SessionMonitor
	memory   *monitor.MemoryMonitor // nil unless the memory dashboard is enabled

	emergency       *emergency.EmergencyMain // nil unless the emergency feature is enabled
	emergencyBeginY int
	planChange      *emergency.PlanChange
	planPersist     emergency.Persister

	dbInfo     *model.DBInfo
	dataLogger *persist.DataLogger // nil unless persistence is enabled

	showMemoryView     bool
	showHealthView     bool
	healthCollector    *healthdash.Collector
	healthView         *healthdash.View
	healthDetailLoader *healthdash.DetailLoader
	healthState        healthViewState
	refresher          *Refresher
	exitRequested      atomic.Bool
}

// New builds the app, its monitors, and their layout. screen is nil for daemon
// mode, where panels still render into their pads (for persistence) but never
// blit.
func New(deps monitor.Deps, screen *tui.Screen) *App {
	a := &App{
		cfg:    deps.Cfg,
		logger: deps.Logger,
		alarm:  deps.Alarm,
		health: deps.Health,
		db:     deps.DB,
		os:     deps.OS,
		screen: screen,
		daemon: deps.Cfg.GetBool("main.daemon", false),
	}
	if screen != nil {
		screen.SetQuitHandler(a.requestExit)
	}
	a.buildMonitors(deps)
	return a
}

// buildMonitors constructs the five resident panels and stacks them vertically,
// matching the monitors_array order and begin_y accumulation in gstop.py.
func (a *App) buildMonitors(deps monitor.Deps) {
	dbMon := monitor.NewDBMonitor(deps)
	osMon := monitor.NewOSMonitor(deps)
	insMon := monitor.NewInstanceMonitor(deps)
	evMon := monitor.NewEventMonitor(deps)
	sessMon := monitor.NewSessionMonitor(deps)

	a.monitors = []monitor.Monitor{dbMon, osMon, insMon, evMon, sessMon}
	a.dbMon = dbMon
	a.osMon = osMon
	a.insMon = insMon
	a.event = evMon
	a.session = sessMon

	// The database monitor fills this shared container with the connected
	// instance's version/user/role, used to name the persistence log files.
	a.dbInfo = model.NewDBInfo()
	dbMon.SetDBInfo(a.dbInfo)

	beginY := 0
	for _, m := range a.monitors {
		m.Init(0, beginY, model.MonitorWidth)
		beginY += m.Height()
	}
	a.emergencyBeginY = beginY

	a.buildMemory(deps)
	a.buildEmergency(deps)
	a.buildHealthDashboard(deps)
}

// buildMemory creates the memory dashboard only when both memory monitoring and
// dynamic memory collection are enabled, matching the guard in gstop.py. It
// starts hidden (print_to_screen = False) and overlays the resident panels when
// the memory view is toggled on.
func (a *App) buildMemory(deps monitor.Deps) {
	if deps.Cfg.GetInt("main.mem_interval", 30) == 0 || !deps.Cfg.GetBool("main.dynamic_mem_enable", false) {
		return
	}
	a.memory = monitor.NewMemoryMonitor(deps)
	a.memory.Init(0, 0, model.MonitorWidth)
	a.memory.SetVisible(false)
}

// Run executes the interactive TUI loop until the user quits or the refresh loop
// is detected as stalled.
func (a *App) Run() error {
	a.startRefresher()
	defer a.stop()

	a.session.SetCursor(-1)
	interval := a.interval()
	for {
		if a.exitRequested.Load() {
			return nil
		}
		if a.checkStalled() {
			return nil
		}
		a.drawAll()
		if !a.showHealthView {
			a.renderEmergency(a.screen.Raw())
		}
		a.screen.Show()

		key, ok := a.screen.GetKey(interval)
		if !ok {
			continue
		}
		if a.exitRequested.Load() || key.IsRune('q') {
			a.requestExit()
			return nil
		}
		if a.showHealthView {
			a.handleHealthViewKey(key)
			if a.exitRequested.Load() {
				return nil
			}
			continue
		}
		if a.showMemoryView {
			a.handleMemoryViewKey(key)
			if a.exitRequested.Load() {
				return nil
			}
			continue
		}
		if a.dispatch(key) {
			return nil
		}
	}
}

// RunDaemon executes the headless loop: it keeps panels refreshed and their pads
// populated (for persistence) without drawing to a terminal.
func (a *App) RunDaemon() error {
	a.startRefresher()
	defer a.stop()

	interval := a.interval()
	for {
		if a.exitRequested.Load() {
			return nil
		}
		if a.checkStalled() {
			return nil
		}
		for _, m := range a.monitors {
			m.Draw(nil)
		}
		a.renderEmergency(nil)
		time.Sleep(interval)
	}
}

// startRefresher does one synchronous refresh so the first frame has data, then
// launches the background refresh goroutine.
func (a *App) startRefresher() {
	a.refresher = NewRefresher(a.monitors, a.health, a.cfg, a.logger)
	if a.emergency != nil || a.healthCollector != nil {
		a.refresher.SetAfterRefresh(a.afterMonitorRefresh)
	}
	a.refresher.RefreshOnce()
	if a.healthCollector != nil {
		a.healthCollector.RefreshFast()
	}
	a.startDataLogger()
	go a.refresher.Run()
}

// startDataLogger launches monitoring-data persistence when a positive
// log_interval is configured. It runs after the initial refresh so the database
// version is known before the first log file is named.
func (a *App) startDataLogger() {
	logInterval := a.cfg.GetInt("main.log_interval", 0)
	if logInterval == 0 {
		return
	}
	interval := time.Duration(logInterval) * time.Second
	a.dataLogger = persist.New(a.osMon, a.dbMon, a.insMon, a.dbInfo, a.cfg, a.logger, interval)
	a.dataLogger.Start()
}

// checkStalled exits (with an alarm) when the refresh loop has been silent for
// too long, so an external supervisor can relaunch the process.
func (a *App) checkStalled() bool {
	if !a.health.ShouldExit() {
		return false
	}
	a.logger.Error("Gstop needs to exit because GstopRefresher has not refreshed data for 5 minutes.")
	a.alarm.CheckAndReport(a.logger, "GstopRefresher",
		"Gausstop检测到工具的刷新线程超过5分钟未刷新数据，现在自动退出进程等待重新拉起", false)
	return true
}

// drawAll blits every resident panel to the screen; in the memory view the
// resident panels are hidden and the memory dashboard is drawn over them.
func (a *App) drawAll() {
	if a.showHealthView {
		a.drawHealthView()
		return
	}
	raw := a.screen.Raw()
	for _, m := range a.monitors {
		m.Draw(raw)
	}
	if a.showMemoryView && a.memory != nil {
		a.memory.Draw(raw)
	}
}

// dispatch routes a top-level keypress, returning true to quit.
func (a *App) dispatch(key tui.Key) bool {
	switch {
	case key.IsRune('q'):
		a.requestExit()
		return true
	case key.IsRune('r'):
		a.event.SetImmediate(true)
	case key.IsRune('c'):
		a.event.SetImmediate(false)
	case key.IsRune('s'):
		a.sessionMode()
	case key.IsRune('m'):
		a.enterMemoryView()
	case key.IsRune('h'):
		a.enterHealthView()
	case key.IsRune('e'):
		a.emergencyMode()
	default:
		a.screen.FlushInput()
	}
	return a.exitRequested.Load()
}

// enterMemoryView switches to the memory dashboard if it is enabled, hiding the
// resident panels. Port of the 'm' branch + switch_to_memory_view.
func (a *App) enterMemoryView() {
	if a.memory == nil {
		return
	}
	a.setResidentVisible(false)
	a.memory.SetVisible(true)
	a.showMemoryView = true
	a.memory.Draw(a.screen.Raw())
	a.screen.Show()
}

// exitMemoryView returns to the resident panels. Port of switch_to_normal_view.
func (a *App) exitMemoryView() {
	a.setResidentVisible(true)
	if a.memory != nil {
		a.memory.SetVisible(false)
	}
	a.showMemoryView = false
}

func (a *App) setResidentVisible(v bool) {
	for _, m := range a.monitors {
		m.SetVisible(v)
	}
}

// handleMemoryViewKey routes keys while the memory view is active: Escape returns to
// the normal view, m enters the memory selection sub-mode, anything else is
// discarded. Port of the show_memory_view branch in gstop.py.
func (a *App) handleMemoryViewKey(key tui.Key) {
	switch {
	case key.Kind == tui.KeyEscape:
		a.exitMemoryView()
	case key.IsRune('m'):
		a.memoryMode()
	default:
		a.screen.FlushInput()
	}
}

// memoryMode runs the memory selection sub-view (terminate by k).
func (a *App) memoryMode() {
	a.runMemoryKeys()
}

// sessionMode pauses refreshing while the session selection sub-view owns input.
func (a *App) sessionMode() {
	a.refresher.Pause()
	defer a.refresher.Resume()
	a.runSessionKeys()
}

// stop tears down the background loop and alarm on exit.
func (a *App) stop() {
	a.requestExit()
	a.logger.Warning("Gstop is starting to exit.")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if a.refresher != nil {
			a.refresher.Stop()
		}
		if a.dataLogger != nil {
			a.dataLogger.Stop()
		}
		if a.memory != nil {
			a.memory.Stop()
		}
		if a.healthCollector != nil {
			a.healthCollector.Stop()
		}
		a.alarm.Stop()
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		a.logger.Warning("Shutdown cleanup exceeded 500ms; exiting without further wait.")
	}
}

// requestExit is the highest-priority command path. It records the process exit
// request before canceling data sources, ensuring no subsequent refresh can be
// mistaken for useful work.
func (a *App) requestExit() {
	if !a.exitRequested.CompareAndSwap(false, true) {
		return
	}
	if a.refresher != nil {
		a.refresher.RequestStop()
	}
	if a.healthCollector != nil {
		a.healthCollector.Cancel()
	}
	if a.db != nil {
		a.db.Cancel()
	}
	if a.os != nil {
		a.os.Cancel()
	}
}

func (a *App) interval() time.Duration {
	return time.Duration(a.cfg.GetInt("main.interval", 3)) * time.Second
}
