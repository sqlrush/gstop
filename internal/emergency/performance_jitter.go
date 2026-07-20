package emergency

import (
	"fmt"
	"strconv"
	"time"
)

const (
	perfJitterName    = "PerformanceJitter"
	perfJitterHeader  = "[EMER02 - PerformanceJitter]"
	perfJitterPersist = 120

	// perfJitterCfg is the config key prefix for every jitter threshold.
	perfJitterCfg = "emergency.performance_jitter."

	// perfJitterCollectTimeout bounds the background perf collector, matching
	// util.run_command_background's default of 10 seconds.
	perfJitterCollectTimeout = 10 * time.Second

	// jitterTSLayout renders snapshot timestamps as the Python str(datetime) form.
	jitterTSLayout = "2006-01-02 15:04:05"

	// jitterSnapshotHeader is the Prev/Curr comparison table header, verbatim.
	jitterSnapshotHeader = "Snapshot:  ID       TIMESTAMP            INS_ACS  CPU%     R_AWAIT  W_AWAIT  AQU_SZ  P80(ms)"
)

// jitterSnap is one retained per-cycle sample compared across the sliding
// window. Port of the data_dict values in performance_jitter.py.
type jitterSnap struct {
	snapID int
	snapTS time.Time
	asc    int     // active session count
	cpu    float64 // OS[1] %CPU
	rps    float64 // OS[3] r/s
	wps    float64 // OS[4] w/s
	rAwait float64 // OS[7] r_await
	wAwait float64 // OS[8] w_await
	aquSz  float64 // OS[11] aqu-sz
	p80    float64 // Instance[9] P80(ms)
}

// field renders one metric by its Python dict key for the alarm message.
func (s jitterSnap) field(item string) string {
	switch item {
	case "asc":
		return strconv.Itoa(s.asc)
	case "cpu":
		return pyFloat(s.cpu)
	case "r_await":
		return pyFloat(s.rAwait)
	case "w_await":
		return pyFloat(s.wAwait)
	case "aqu_sz":
		return pyFloat(s.aquSz)
	case "p80":
		return pyFloat(s.p80)
	}
	return ""
}

// PerformanceJitter detects sudden (instantaneous) and sustained (P80) database
// performance jitter by comparing the newest snapshot against a sliding window
// of recent ones. Port of emergency/performance_jitter.py.
type PerformanceJitter struct {
	*Base
	dataWindow []jitterSnap
	first      bool
}

// NewPerformanceJitter builds the jitter scenario.
func NewPerformanceJitter(deps Deps) *PerformanceJitter {
	return &PerformanceJitter{
		Base:  NewBase(perfJitterName, perfJitterHeader, deps, perfJitterPersist),
		first: true,
	}
}

// Analyze records the current snapshot into the sliding window then checks for a
// sustained P80 breach followed by instantaneous jitter.
func (s *PerformanceJitter) Analyze() {
	// The initial statistical data is inaccurate; discard the first snapshot.
	if s.first {
		s.first = false
		return
	}

	// Trim the oldest sample before appending, keeping at most the compare scope.
	s.evictOldest()

	if len(s.snap.Session) == 0 {
		s.deps.Logger.Warning("missing full session data for jitter analysis")
		return
	}
	asc := s.countActiveSessions()

	if len(s.snap.OS) == 0 || len(s.snap.Instance) == 0 {
		s.deps.Logger.Warning("missing OS or Instance data for jitter analysis")
		return
	}

	curr := jitterSnap{
		snapID: s.snapID,
		snapTS: s.snap.SnapTS,
		asc:    asc,
		cpu:    dbFloat(s.osCell(1)),
		rps:    dbFloat(s.osCell(3)),
		wps:    dbFloat(s.osCell(4)),
		rAwait: dbFloat(s.osCell(7)),
		wAwait: dbFloat(s.osCell(8)),
		aquSz:  dbFloat(s.osCell(11)),
		p80:    dbFloat(s.instanceCell(9)),
	}
	s.dataWindow = jitterAppend(s.dataWindow, curr)

	if s.analyzeSustainedP80(curr) {
		return
	}
	s.analyzeInstantaneous(curr)
}

// evictOldest drops the oldest sample once the window reaches the compare scope,
// mirroring the del-min-key trim performed before a new sample is stored.
func (s *PerformanceJitter) evictOldest() {
	scope := s.deps.Cfg.GetInt(perfJitterCfg+"snapshot_compare_scope", 10)
	if len(s.dataWindow) == scope {
		s.dataWindow = jitterDropFirst(s.dataWindow)
	}
}

// countActiveSessions counts sessions in the 'active' state across full_session.
func (s *PerformanceJitter) countActiveSessions() int {
	count := 0
	for _, row := range s.snap.Session {
		if row.Str(9) == "active" {
			count++
		}
	}
	return count
}

// analyzeSustainedP80 fires when P80 exceeds the threshold for a run of
// consecutive snapshots ending at the current one. Returns true when triggered.
func (s *PerformanceJitter) analyzeSustainedP80(curr jitterSnap) bool {
	p80Thresh := s.deps.Cfg.GetFloat(perfJitterCfg+"p80_thresh", 10)
	streakTarget := s.deps.Cfg.GetInt(perfJitterCfg+"p80_breach_streak", 3)

	streak := 0
	for i := len(s.dataWindow) - 1; i >= 0; i-- {
		last := s.dataWindow[i]
		if last.p80 <= p80Thresh {
			break
		}
		streak++
		if streak != streakTarget {
			continue
		}
		start := last.snapID
		if _, ok := jitterFind(s.dataWindow, last.snapID-1); ok {
			start = last.snapID - 1
		}
		s.triggerEmergency("p80", start, curr.snapID)
		s.runP80Collector()
		return true
	}
	return false
}

// runP80Collector launches the configured performance-data collector once the
// P80 alarm passes its own suppression gate. The gate key is mixed-case, so it
// is distinct from the (lower-cased) key CheckAndReport used, matching Python.
func (s *PerformanceJitter) runP80Collector() {
	if !s.deps.Alarm.ShouldReport(perfJitterName+"_p80", false) {
		return
	}
	switch s.deps.Cfg.GetString(perfJitterCfg+"p80_collector_type", "none") {
	case "perf_command":
		cmd := s.deps.Cfg.GetString(perfJitterCfg+"p80_alert_execute_command", "")
		if cmd != "" {
			s.deps.OS.RunBackground(cmd, perfJitterCollectTimeout)
		}
	case "system_func":
		query := s.deps.Cfg.GetString(perfJitterCfg+"p80_alert_execute_func", "")
		if query != "" {
			s.deps.DB.BackgroundQuery(query)
		}
	}
}

// analyzeInstantaneous compares the current snapshot against each earlier one in
// the window (oldest first) and fires on the first metric that jitters.
func (s *PerformanceJitter) analyzeInstantaneous(curr jitterSnap) {
	for _, last := range s.dataWindow {
		if last.snapID == curr.snapID {
			break
		}
		if s.checkJitter(curr, last) {
			return
		}
	}
}

// checkJitter evaluates the metrics in priority order and triggers the first
// that jitters, returning whether any fired.
func (s *PerformanceJitter) checkJitter(curr, last jitterSnap) bool {
	switch {
	case s.jitterASC(curr, last):
		s.triggerEmergency("asc", last.snapID, curr.snapID)
	case s.jitterCPU(curr, last):
		s.triggerEmergency("cpu", last.snapID, curr.snapID)
	case s.jitterRAwait(curr, last):
		s.triggerEmergency("r_await", last.snapID, curr.snapID)
	case s.jitterWAwait(curr, last):
		s.triggerEmergency("w_await", last.snapID, curr.snapID)
	case s.jitterAquSz(curr, last):
		s.triggerEmergency("aqu_sz", last.snapID, curr.snapID)
	default:
		return false
	}
	return true
}

// jitterASC reports an active-session-count surge past both the ratio and the
// absolute-increase thresholds.
func (s *PerformanceJitter) jitterASC(curr, last jitterSnap) bool {
	pct := s.deps.Cfg.GetFloat(perfJitterCfg+"ins_acs_pct_thresh", 2)
	abs := s.deps.Cfg.GetInt(perfJitterCfg+"ins_acs_abs_thresh", 20)
	return float64(curr.asc) > float64(last.asc)*pct && curr.asc-last.asc > abs
}

// jitterCPU reports a CPU spike, considered only when the prior value is non-zero.
func (s *PerformanceJitter) jitterCPU(curr, last jitterSnap) bool {
	if last.cpu == 0 {
		return false
	}
	pct := s.deps.Cfg.GetFloat(perfJitterCfg+"os_cpu_pct_thresh", 2)
	thresh := s.deps.Cfg.GetFloat(perfJitterCfg+"os_cpu_thresh", 50)
	return curr.cpu > last.cpu*pct && curr.cpu > thresh
}

// jitterRAwait reports a read-latency spike, only when both read rates are non-zero.
func (s *PerformanceJitter) jitterRAwait(curr, last jitterSnap) bool {
	if curr.rps == 0 || last.rps == 0 {
		return false
	}
	return s.awaitJitter(curr.rAwait, last.rAwait)
}

// jitterWAwait reports a write-latency spike, only when both write rates are non-zero.
func (s *PerformanceJitter) jitterWAwait(curr, last jitterSnap) bool {
	if curr.wps == 0 || last.wps == 0 {
		return false
	}
	return s.awaitJitter(curr.wAwait, last.wAwait)
}

// awaitJitter applies the shared IO-latency ratio and absolute-increase test.
func (s *PerformanceJitter) awaitJitter(currAwait, lastAwait float64) bool {
	pct := s.deps.Cfg.GetFloat(perfJitterCfg+"rw_await_pct_thresh", 2)
	abs := s.deps.Cfg.GetFloat(perfJitterCfg+"rw_await_abs_thresh", 5)
	return currAwait > lastAwait*pct && currAwait-lastAwait > abs
}

// jitterAquSz reports a request-queue-length spike past both thresholds.
func (s *PerformanceJitter) jitterAquSz(curr, last jitterSnap) bool {
	pct := s.deps.Cfg.GetFloat(perfJitterCfg+"aqu_sz_pct_thresh", 2)
	abs := s.deps.Cfg.GetFloat(perfJitterCfg+"aqu_sz_abs_thresh", 5)
	return curr.aquSz > last.aquSz*pct && curr.aquSz-last.aquSz > abs
}

// triggerEmergency marks the fault, records the persistence start snapshot,
// renders the Prev/Curr comparison, and reports the alarm. Port of the
// scenario's trigger_emergency method.
func (s *PerformanceJitter) triggerEmergency(item string, lastSnapID, currSnapID int) {
	s.Trigger()
	s.startPersistSnapID = lastSnapID

	last, ok := jitterFind(s.dataWindow, lastSnapID)
	if !ok {
		return
	}
	curr, ok := jitterFind(s.dataWindow, currSnapID)
	if !ok {
		return
	}

	s.AddInfo(jitterSnapshotHeader)
	s.AddInfo(jitterRow("  Prev ->", last))
	s.AddInfo(jitterRow("  Curr ->", curr))

	key := perfJitterName + "_" + item
	value := fmt.Sprintf("Gausstop检测到性能抖动，发生抖动的指标为：%s，抖动前的值：%s，抖动前的时间：%s，抖动后的值：%s",
		item, last.field(item), last.snapTS.Format(jitterTSLayout), curr.field(item))
	s.deps.Alarm.CheckAndReport(s.deps.Logger, key, value, true)
}

// jitterRow formats one comparison row aligned under jitterSnapshotHeader.
func jitterRow(label string, snap jitterSnap) string {
	return fmt.Sprintf("%s  %-7d  %s  %-7d  %-5s    %-7s  %-7s  %-6s  %s",
		label,
		snap.snapID,
		snap.snapTS.Format(jitterTSLayout),
		snap.asc,
		pyFloat(snap.cpu),
		pyFloat(snap.rAwait),
		pyFloat(snap.wAwait),
		pyFloat(snap.aquSz),
		pyFloat(snap.p80),
	)
}

// HandleCommand has no interactive remediation for the jitter scenario.
func (s *PerformanceJitter) HandleCommand(cmd *Command, line string) {}

// jitterFind returns the window sample with the given snap id.
func jitterFind(w []jitterSnap, snapID int) (jitterSnap, bool) {
	for _, snap := range w {
		if snap.snapID == snapID {
			return snap, true
		}
	}
	return jitterSnap{}, false
}

// jitterAppend returns a new window with snap appended (never mutates the input).
func jitterAppend(w []jitterSnap, snap jitterSnap) []jitterSnap {
	out := make([]jitterSnap, len(w)+1)
	copy(out, w)
	out[len(w)] = snap
	return out
}

// jitterDropFirst returns a new window without its oldest sample.
func jitterDropFirst(w []jitterSnap) []jitterSnap {
	if len(w) == 0 {
		return w
	}
	out := make([]jitterSnap, len(w)-1)
	copy(out, w[1:])
	return out
}
