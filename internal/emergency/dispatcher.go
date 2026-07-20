package emergency

import (
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"

	"gstop/internal/dbconn"
	"gstop/internal/model"
	"gstop/internal/timing"
	"gstop/internal/tui"
)

// windowHeight is the emergency panel height (EMERGENCY_WINDOW_HEIGHT).
const windowHeight = 20

// Fixed registration order of the scenarios, matching emergency_module_array.
const (
	idxPlanChange = iota
	idxMemoryFull
	idxIOFull
	idxCPUFull
	idxThreadPoolFull
	idxConnectionsFull
	idxPerfJitter
	idxSlowSQL
	idxInspection
	scenarioCount
)

// memoryFullTyper is implemented by the memory-full scenario to expose its
// highlight mode to the memory dashboard.
type memoryFullTyper interface {
	MemoryFullType() int
}

// emerLine is one rendered emergency line and whether it is interactive.
type emerLine struct {
	scenario Scenario
	text     string
	visible  bool
}

// scenarioResult is a published snapshot of one scenario's analysis output. The
// render path reads only these snapshots, never the scenarios' live state, so
// drawing can proceed while the next analysis runs.
type scenarioResult struct {
	scenario  Scenario
	header    string
	info      []string
	triggered bool
}

// EmergencyMain drives all scenarios each cycle, renders the emergency panel, and
// persists snapshots. Port of emergency.EmergencyMain.
type EmergencyMain struct {
	deps      Deps
	scenarios []Scenario

	beginX, beginY, width, height int
	pad                           *tui.Pad

	// mu guards the published render state (results, lines, pad, snapshot data).
	// It is only ever held for fast in-memory work so the UI loop never blocks.
	mu           sync.Mutex
	snapshotDict map[int]snapshotData
	currSnapID   int
	currSnapTS   time.Time
	currSession  []dbconn.Row
	lines        []emerLine
	results      []scenarioResult
	triggered    bool

	// analysisMu serialises the slow work: scenario analysis, interactive
	// remediation, and per-trigger persistence, all of which touch the scenarios'
	// live state. The UI loop only ever TryLocks it, so a slow analysis can never
	// stall drawing or key handling.
	analysisMu sync.Mutex
}

// NewEmergencyMain builds the dispatcher over scenarios given in registration
// order. daemon controls whether a drawable pad is allocated.
func NewEmergencyMain(deps Deps, beginX, beginY, width int, scenarios []Scenario, daemon bool) *EmergencyMain {
	e := &EmergencyMain{
		deps:         deps,
		scenarios:    scenarios,
		beginX:       beginX,
		beginY:       beginY,
		width:        width,
		height:       windowHeight,
		snapshotDict: map[int]snapshotData{},
	}
	e.pad = tui.NewPad(windowHeight, width)
	return e
}

// Height returns the emergency panel height.
func (e *EmergencyMain) Height() int { return e.height }

// Triggered reports whether any scenario is currently firing.
func (e *EmergencyMain) Triggered() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.triggered
}

// Main injects the snapshot into every scenario and analyses them concurrently,
// then publishes a render snapshot of the results. The render mutex is held only
// for the fast bookkeeping at either end — never across the (potentially slow)
// analysis — so the UI loop keeps drawing and handling keys throughout. Port of
// emergency_main.
func (e *EmergencyMain) Main(snap Snapshot) {
	e.analysisMu.Lock()
	defer e.analysisMu.Unlock()

	e.mu.Lock()
	snap.SnapID = e.deps.Persist.GetSnapID()
	snap.SnapTS = time.Now()
	e.currSnapID = snap.SnapID
	e.currSnapTS = snap.SnapTS
	e.currSession = snap.Session
	e.mu.Unlock()

	thresh := time.Duration(e.deps.Cfg.GetFloat("main.refresh_analyze_time_thresh", 3) * float64(time.Second))
	done := make(chan struct{}, len(e.scenarios))
	for _, s := range e.scenarios {
		s.Common().Reset()
		s.Common().Inject(snap)
		go func(sc Scenario) {
			timing.RefreshAnalyze(e.deps.Logger, sc.Common().Name(), thresh, sc.Analyze)
			done <- struct{}{}
		}(s)
	}
	for range e.scenarios {
		<-done
	}

	e.publishResults()
}

// publishResults copies each scenario's outcome into the render snapshot.
func (e *EmergencyMain) publishResults() {
	results := make([]scenarioResult, 0, len(e.scenarios))
	triggered := false
	for _, s := range e.scenarios {
		b := s.Common()
		r := scenarioResult{
			scenario:  s,
			header:    b.Header(),
			info:      append([]string(nil), b.Info()...),
			triggered: b.Triggered(),
		}
		if r.triggered {
			triggered = true
		}
		results = append(results, r)
	}

	e.mu.Lock()
	e.results = results
	e.triggered = triggered
	e.mu.Unlock()
}

// GetTriggerSQLIDs returns the plan-change scenario's highlighted SQL ids.
func (e *EmergencyMain) GetTriggerSQLIDs() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.scenarios[idxPlanChange].Common().SQLIDs()
}

// GetTriggerPIDs returns the pids of the first triggered io/cpu/thread-pool
// scenario, matching get_trigger_emergency_pids.
func (e *EmergencyMain) GetTriggerPIDs() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, idx := range []int{idxIOFull, idxCPUFull, idxThreadPoolFull} {
		if b := e.scenarios[idx].Common(); b.Triggered() {
			return b.PIDs()
		}
	}
	return nil
}

// GetMemoryFullType returns the memory-full scenario's highlight mode.
func (e *EmergencyMain) GetMemoryFullType() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if t, ok := e.scenarios[idxMemoryFull].(memoryFullTyper); ok {
		return t.MemoryFullType()
	}
	return 0
}

// Draw renders the emergency panel from the last published results and rebuilds
// the interactive line list. A cursorY of -1 disables selection highlighting.
// It reads only the render snapshot, so it never waits on a running analysis.
// Port of emergency_print_entry.
func (e *EmergencyMain) Draw(screen tcell.Screen, cursorY int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pad.Clear()
	e.lines = nil

	if !e.triggered {
		e.blit(screen)
		return
	}

	y := e.drawHeader()
	e.drawScenarios(y, cursorY)
	e.blit(screen)
}

// drawHeader paints the reverse-video "EMERGENCY TRIGGERED" banner.
func (e *EmergencyMain) drawHeader() int {
	e.pad.AddStr(0, 0, spaces(e.width-1), model.Style{Pair: model.PairReverse})
	e.pad.AddStr(0, 0, "EMERGENCY TRIGGERED", model.Style{Pair: model.PairAlarmRedSel})
	e.lines = append(e.lines, emerLine{scenario: nil, text: "EMERGENCY TRIGGERED", visible: true})
	return 1
}

// drawScenarios paints each triggered scenario's header and info lines from the
// published snapshot. Only the first triggered scenario is interactive.
func (e *EmergencyMain) drawScenarios(y, cursorY int) {
	interactive := true
	for _, r := range e.results {
		if !r.triggered {
			continue
		}
		if y >= windowHeight {
			break
		}
		y = e.drawLine(r.scenario, r.header, y, cursorY, interactive)
		for _, info := range r.info {
			if y >= windowHeight {
				break
			}
			y = e.drawLine(r.scenario, info, y, cursorY, interactive)
		}
		interactive = false
		y = e.drawLine(r.scenario, "", y, cursorY, false)
	}
}

// drawLine draws one panel line, recording it in the interactive list.
func (e *EmergencyMain) drawLine(s Scenario, text string, y, cursorY int, visible bool) int {
	style := model.Normal
	if cursorY >= 0 && e.beginY+y+1 == cursorY {
		style.Reverse = true
		e.pad.AddStr(y, 0, spaces(e.width-1), model.Style{Pair: model.PairReverse})
	}
	e.pad.AddStr(y, 0, text, style)
	e.lines = append(e.lines, emerLine{scenario: s, text: text, visible: visible})
	return y + 1
}

// HandleCommand runs the interactive remediation for the selected line, if it is
// visible. The line is resolved under the render lock, but the scenario's
// interactive handler (which blocks on keyboard prompts) runs outside it. A
// running analysis owns the scenarios' live state, so the keypress is ignored
// while one is in flight rather than blocking the UI. Port of
// emergency_handle_command_entry.
func (e *EmergencyMain) HandleCommand(screen *tui.Screen, cursorY int) {
	e.mu.Lock()
	idx := cursorY - e.beginY - 1
	var line emerLine
	ok := idx > 1 && idx < len(e.lines)
	if ok {
		line = e.lines[idx]
	}
	e.mu.Unlock()
	if !ok || !line.visible || line.scenario == nil {
		return
	}

	if !e.analysisMu.TryLock() {
		return // analysis in flight; ignore this keypress
	}
	defer e.analysisMu.Unlock()
	line.scenario.HandleCommand(newCommand(screen, e.beginY), line.text)
}

// Persist retains the current snapshot (bounded) and lets each scenario write its
// per-trigger logs. Scenario persistence touches the scenarios' live state, so
// the frame is skipped (harmlessly — the next frame persists) when an analysis is
// in flight, keeping the UI loop non-blocking. Port of emergency_persist.
func (e *EmergencyMain) Persist(monitorsDumpData []model.DumpData) {
	if !e.analysisMu.TryLock() {
		return
	}
	defer e.analysisMu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.currSession) == 0 {
		return
	}
	if len(e.snapshotDict) == e.deps.Cfg.GetInt("emergency.max_snapshot_number", 30) {
		if k, ok := minKeySnapshot(e.snapshotDict); ok {
			delete(e.snapshotDict, k)
		}
	}
	dumps := append(monitorsDumpData, e.pad.DumpData())
	e.snapshotDict[e.currSnapID] = snapshotData{
		snapTS:      e.currSnapTS,
		dumpData:    dumps,
		fullSession: e.currSession,
	}
	for _, s := range e.scenarios {
		s.Common().Persist(e.snapshotDict)
	}
}

func (e *EmergencyMain) blit(screen tcell.Screen) {
	if screen != nil {
		e.pad.Blit(screen, e.beginX, e.beginY)
	}
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

func minKeySnapshot(m map[int]snapshotData) (int, bool) {
	first := true
	var min int
	for k := range m {
		if first || k < min {
			min, first = k, false
		}
	}
	return min, !first
}
