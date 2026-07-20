package app

import (
	"github.com/gdamore/tcell/v2"

	"gstop/internal/emergency"
	"gstop/internal/logging"
	"gstop/internal/model"
	"gstop/internal/monitor"
	"gstop/internal/tui"
)

// buildEmergency constructs the emergency subsystem when enabled, registering the
// nine scenarios in the fixed dispatcher order. Port of the EmergencyMain setup
// in gstop.py.
func (a *App) buildEmergency(deps monitor.Deps) {
	if !deps.Cfg.GetBool("emergency.enable", false) {
		return
	}
	edeps := emergency.Deps{
		Cfg:     deps.Cfg,
		DB:      deps.DB,
		OS:      deps.OS,
		Logger:  logging.New("emergency", "gstop_emergency_run.log"),
		Alarm:   deps.Alarm,
		Persist: emergency.NewMemPersist(deps.Cfg),
	}
	scenarios := []emergency.Scenario{
		emergency.NewPlanChange(edeps),
		emergency.NewMemoryFull(edeps),
		emergency.NewIOFull(edeps),
		emergency.NewCPUFull(edeps),
		emergency.NewThreadPoolFull(edeps),
		emergency.NewConnectionsFull(edeps),
		emergency.NewPerformanceJitter(edeps),
		emergency.NewSlowSQL(edeps),
		emergency.NewInspection(edeps),
	}
	a.emergency = emergency.NewEmergencyMain(edeps, 0, a.emergencyBeginY, model.MonitorWidth, scenarios, a.daemon)
}

// emergencyAnalyze runs the emergency scenarios against the current snapshot and
// pushes their highlight targets back to the session and memory panels. Invoked
// as the refresher's after-refresh hook, matching GstopRefresher.run.
func (a *App) emergencyAnalyze() {
	if a.emergency == nil {
		return
	}
	a.emergency.Main(a.buildSnapshot())
	a.session.SetEmergencySQLIDs(a.emergency.GetTriggerSQLIDs())
	a.session.SetEmergencyPIDs(a.emergency.GetTriggerPIDs())
	if a.memory != nil {
		a.memory.SetMemoryFullType(a.emergency.GetMemoryFullType())
	}
}

// buildSnapshot assembles the current monitor values and full session set for the
// scenarios, replacing the Python monitors_value dict + full_session.
func (a *App) buildSnapshot() emergency.Snapshot {
	return emergency.Snapshot{
		DB:       a.dbMon.MonitorValues(),
		OS:       a.osMon.MonitorValues(),
		Instance: a.insMon.MonitorValues(),
		Memory:   a.memoryPanels(),
		Session:  a.session.Session(),
	}
}

// memoryPanels converts the memory dashboard's panels into the emergency form, or
// nil when the dashboard is disabled.
func (a *App) memoryPanels() []emergency.MemPanel {
	if a.memory == nil {
		return nil
	}
	panels := a.memory.GetPanels()
	out := make([]emergency.MemPanel, len(panels))
	for i, p := range panels {
		out[i] = emergency.MemPanel{Title: p.Title, Header: p.Header, Width: p.Width, Value: p.Value}
	}
	return out
}

// renderEmergency draws the emergency panel and persists the current snapshot. It
// is called every frame (with a nil screen in daemon mode).
func (a *App) renderEmergency(raw tcell.Screen) {
	if a.emergency == nil {
		return
	}
	a.emergency.Draw(raw, -1)
	a.emergency.Persist(a.collectDumps())
}

// collectDumps gathers each resident panel's last-drawn screen snapshot for
// persistence.
func (a *App) collectDumps() []model.DumpData {
	dumps := make([]model.DumpData, 0, len(a.monitors))
	for _, m := range a.monitors {
		dumps = append(dumps, m.DumpData())
	}
	return dumps
}

// emergencyMode enters the emergency selection sub-view, only when a scenario is
// currently triggered. Port of the 'e' branch in gstop.py.
func (a *App) emergencyMode() {
	if a.emergency == nil || !a.emergency.Triggered() {
		return
	}
	a.refresher.Pause()
	defer a.refresher.Resume()
	a.runEmergencyKeys()
}

// runEmergencyKeys implements handle_emergency_related_keys: arrow navigation and
// k to run the selected scenario's remediation.
func (a *App) runEmergencyKeys() {
	cursorY, cursorX := model.EmergencyCursorYStart, model.EmergencyCursorXStart
	screenW, screenH := a.screen.Size()
	raw := a.screen.Raw()

	for {
		key, ok := a.screen.GetKey(-1)
		if !ok {
			continue
		}
		a.screen.FlushInput()
		if a.handleEmergencyKey(key, &cursorY, &cursorX, screenH, screenW) {
			return
		}
		a.emergency.Draw(raw, cursorY)
		a.screen.Show()
	}
}

// handleEmergencyKey applies one keypress, returning true to leave the sub-view.
func (a *App) handleEmergencyKey(key tui.Key, cursorY, cursorX *int, screenH, screenW int) bool {
	maxRow := minInt(model.EmergencyCursorYStart+a.emergency.Height(), screenH-1)
	switch {
	case key.IsRune('e'):
		// stay in the sub-view
	case key.Kind == tui.KeyUp:
		if *cursorY > model.EmergencyCursorYStart {
			*cursorY--
		}
	case key.Kind == tui.KeyDown:
		if *cursorY < maxRow {
			*cursorY++
		}
	case key.Kind == tui.KeyLeft:
		if *cursorX > 0 {
			*cursorX--
		}
	case key.Kind == tui.KeyRight:
		if *cursorX < screenW-1 {
			*cursorX++
		}
	case key.IsRune('k') && a.supportTerminate():
		a.emergency.HandleCommand(a.screen, *cursorY)
	default:
		return true
	}
	return false
}
