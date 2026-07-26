package emergency

import (
	"math"
	"sort"
	"sync"
	"time"

	"gstop/internal/dbconn"
)

const (
	planChangeName   = "PlanChange"
	planChangeHeader = "[EMER01 - PlanChange] - select the line with 'SQL_ID' and press 'k' to terminate abnormal sessions"
)

// statementQuery samples per-SQL call counts and DB/CPU time, excluding
// transaction-control statements. Preserved verbatim from plan_change.py.
const statementQuery = `SELECT unique_sql_id, SUM(n_calls), SUM(db_time), SUM(cpu_time)
                    FROM dbe_perf.statement
                    WHERE query !~* '^\s*(start transaction|begin|commit|end)\s*;?\s*$'
                    GROUP BY unique_sql_id;
                    `

// PlanChange detects execution-plan regressions by comparing each hot SQL's
// active-session count and latency against historical snapshots. Port of
// emergency/plan_change.py.
type PlanChange struct {
	*Base

	lastStatement []dbconn.Row
	lastSnapTS    time.Time

	eventMu              sync.Mutex
	eventHistory         []PlanChangeEvent
	activeEvents         map[int64]int
	notificationsEnabled bool
}

// PlanChangeEvent is one retained plan-regression lifecycle record. Unlike the
// emergency panel's per-cycle state, recovered events remain available to the
// health dashboard for the lifetime of this gstop process.
type PlanChangeEvent struct {
	SQLID         int64
	Query         string
	FirstSeen     time.Time
	LastSeen      time.Time
	RecoveredAt   time.Time
	PreviousAcs   int
	CurrentAcs    int
	PreviousLatUS float64
	CurrentLatUS  float64
	Recovered     bool
}

// analyzeResult holds one cycle's derived instance and per-SQL statistics.
type analyzeResult struct {
	ins  InsInfo
	sqls map[int64]SQLInfo
}

// NewPlanChange builds the plan-change scenario. The statement baseline is
// established on the first Analyze rather than here, so startup never blocks on a
// database query.
func NewPlanChange(deps Deps) *PlanChange {
	persistNum := deps.Cfg.GetInt("emergency.plan_change.snapshot_persist_number", 300)
	return &PlanChange{
		Base:                 NewBase(planChangeName, planChangeHeader, deps, persistNum),
		activeEvents:         map[int64]int{},
		notificationsEnabled: true,
	}
}

// SetNotificationsEnabled controls alarms and remediation text without turning
// off snapshot comparison or event recording. Health-only mode disables it.
func (s *PlanChange) SetNotificationsEnabled(enabled bool) {
	s.eventMu.Lock()
	s.notificationsEnabled = enabled
	s.eventMu.Unlock()
}

// Events returns retained plan changes ordered by most recently observed first.
func (s *PlanChange) Events() []PlanChangeEvent {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	out := append([]PlanChangeEvent(nil), s.eventHistory...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

func (s *PlanChange) recordPlanChangeEvent(at time.Time, current, previous SQLInfo, query string) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if s.activeEvents == nil {
		s.activeEvents = map[int64]int{}
	}
	index, exists := s.activeEvents[current.UniqueSQLID]
	if !exists {
		event := PlanChangeEvent{
			SQLID:         current.UniqueSQLID,
			Query:         query,
			FirstSeen:     at,
			PreviousAcs:   previous.SQLAcsCnt,
			PreviousLatUS: previous.SQLLatency,
		}
		s.eventHistory = append(s.eventHistory, event)
		index = len(s.eventHistory) - 1
		s.activeEvents[current.UniqueSQLID] = index
	}
	event := s.eventHistory[index]
	event.LastSeen = at
	event.CurrentAcs = current.SQLAcsCnt
	event.CurrentLatUS = current.SQLLatency
	event.Recovered = false
	event.RecoveredAt = time.Time{}
	if event.Query == "" {
		event.Query = query
	}
	s.eventHistory[index] = event
}

func (s *PlanChange) recordPlanChangeRecovered(sqlID int64, at time.Time) {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	index, ok := s.activeEvents[sqlID]
	if !ok {
		return
	}
	event := s.eventHistory[index]
	event.Recovered = true
	event.RecoveredAt = at
	s.eventHistory[index] = event
	delete(s.activeEvents, sqlID)
}

func (s *PlanChange) notificationsAreEnabled() bool {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return s.notificationsEnabled
}

// Analyze samples the statement view, derives and persists per-SQL statistics,
// then looks for plan-change regressions.
func (s *PlanChange) Analyze() {
	curr := s.deps.DB.Query(statementQuery)
	if curr == nil {
		s.deps.Logger.Error("Query dbe_perf.statement failed.")
		return
	}
	if s.lastStatement == nil {
		// First cycle: establish the baseline; analysis begins next cycle.
		s.lastStatement = curr
		s.lastSnapTS = s.snap.SnapTS
		return
	}

	result, ok := s.analyzeStatement(curr)
	if !ok {
		s.lastStatement = curr
		s.lastSnapTS = s.snap.SnapTS
		return
	}
	s.analyzePlanChange(result)

	s.lastStatement = curr
	s.lastSnapTS = s.snap.SnapTS
}

// analyzeStatement computes QPS/latency/CPU deltas per SQL and the instance
// active-session total, persisting each. It returns ok=false when no time has
// elapsed. Port of analyze_statement.
func (s *PlanChange) analyzeStatement(curr []dbconn.Row) (analyzeResult, bool) {
	timeDiff := s.snap.SnapTS.Sub(s.lastSnapTS).Seconds()
	if timeDiff == 0 {
		return analyzeResult{}, false
	}

	lastByID := indexStatements(s.lastStatement)
	activeByID := s.activeSessionCounts()
	currCPU := dbFloat(s.osCell(1))

	sqls := map[int64]SQLInfo{}
	for _, row := range curr {
		id, _ := row.Int(0)
		acs := activeByID[id]
		last := lastByID[id]

		nCallsDiff := dbFloat(row.Col(1)) - statementField(last, 1)
		qps := pcRound1(nCallsDiff / timeDiff)

		var latency, cpuTime float64
		if nCallsDiff > 0 {
			latency = round2((dbFloat(row.Col(2)) - statementField(last, 2)) / nCallsDiff)
			cpuTime = round2((dbFloat(row.Col(3)) - statementField(last, 3)) / nCallsDiff)
		}
		if acs == 0 && latency == 0 {
			continue
		}

		info := SQLInfo{
			DBID: s.dbID, SnapID: s.snap.SnapID, SnapTS: s.snap.SnapTS,
			UniqueSQLID: id, SQLAcsCnt: acs, SQLLatency: latency, SQLCPUTime: cpuTime, SQLQPS: qps,
		}
		s.deps.Persist.PersistSQLInfo(info)
		sqls[id] = info
	}

	insAcs := 0
	for _, c := range activeByID {
		insAcs += c
	}
	ins := InsInfo{
		DBID: s.dbID, SnapID: s.snap.SnapID, SnapTS: s.snap.SnapTS,
		InsAcsCnt: insAcs, InsCPUUtl: currCPU,
	}
	s.deps.Persist.PersistInsInfo(ins)
	return analyzeResult{ins: ins, sqls: sqls}, true
}

// analyzePlanChange compares each hot SQL against its history and fires the
// emergency when both active sessions and latency have surged. Port of
// analyze_plan_change.
func (s *PlanChange) analyzePlanChange(result analyzeResult) {
	ins := result.ins
	if ins.InsCPUUtl < s.deps.Cfg.GetFloat("emergency.plan_change.os_cpu_thresh", 60) {
		s.judgeRecovered(nil)
		return
	}

	acsPct := s.deps.Cfg.GetFloat("emergency.plan_change.sql_acs_ins_pct_thresh", 0.3)
	var triggered []int64
	earliest := 0
	for id, sql := range result.sqls {
		if float64(sql.SQLAcsCnt) <= float64(ins.InsAcsCnt)*acsPct {
			continue
		}
		snapID, fired := s.evaluateSQL(ins, sql)
		if fired {
			triggered = append(triggered, id)
			if earliest == 0 || snapID < earliest {
				earliest = snapID
			}
		}
	}

	s.sqlIDs = triggered
	if earliest != 0 {
		s.startPersistSnapID = earliest
	}
	s.judgeRecovered(triggered)
}

// evaluateSQL decides whether one SQL is a plan-change regression, either
// against its recorded emergency baseline or against the comparison window,
// returning the baseline snap id and whether it fired.
func (s *PlanChange) evaluateSQL(ins InsInfo, sql SQLInfo) (int, bool) {
	if base, ok := s.deps.Persist.GetEmergencySQLInfoSnap(s.dbID, sql.UniqueSQLID); ok {
		if s.regressedAgainst(sql, base.SQLAcsCnt, base.SQLLatency, true) {
			s.triggerEmergency(ins, sql, emergencyToSQLInfo(base), true)
			return base.SnapID, true
		}
		return 0, false
	}

	snaps := s.deps.Persist.GetSQLInfoSnap(s.dbID, ins.SnapID, sql.UniqueSQLID)
	if snaps == nil {
		s.deps.Logger.Warning("Query sql snap return None, unique_sql_id: %d", sql.UniqueSQLID)
		return 0, false
	}
	for _, snap := range snaps {
		if !s.regressedAgainst(sql, snap.SQLAcsCnt, snap.SQLLatency, false) {
			continue
		}
		s.persistBaseline(ins, snap)
		s.triggerEmergency(ins, sql, snap, false)
		return snap.SnapID, true
	}
	return 0, false
}

// regressedAgainst applies the surge thresholds for active sessions and latency.
// When emergency is true the latency test only applies to a non-zero current
// latency, matching the two code paths in the original.
func (s *PlanChange) regressedAgainst(sql SQLInfo, lastAcs int, lastLat float64, emergency bool) bool {
	absThresh := s.deps.Cfg.GetFloat("emergency.plan_change.sql_acs_abs_thresh", 20)
	pctThresh := s.deps.Cfg.GetFloat("emergency.plan_change.sql_acs_pct_thresh", 2)
	latThresh := s.deps.Cfg.GetFloat("emergency.plan_change.sql_latency_pct_thresh", 2)

	if float64(sql.SQLAcsCnt) <= math.Max(float64(lastAcs)+absThresh, float64(lastAcs)*pctThresh) {
		return false
	}
	if emergency {
		if sql.SQLLatency != 0 && sql.SQLLatency <= lastLat*latThresh {
			return false
		}
	} else if sql.SQLLatency <= lastLat*latThresh {
		return false
	}
	return true
}

// persistBaseline records the pre-regression snapshot as the emergency baseline.
func (s *PlanChange) persistBaseline(ins InsInfo, snap SQLInfo) {
	s.deps.Persist.PersistEmergencySQLInfo(EmergencySQLInfo{
		DBID: snap.DBID, SnapID: snap.SnapID, SnapTS: snap.SnapTS,
		UniqueSQLID: snap.UniqueSQLID, SQLAcsCnt: snap.SQLAcsCnt, SQLLatency: snap.SQLLatency,
		SQLCPUTime: snap.SQLCPUTime, SQLQPS: snap.SQLQPS,
		EmergencyTS: ins.SnapTS, Recovered: false,
	})
}

// judgeRecovered marks emergency SQLs recovered once they have been quiet longer
// than the observation window and did not re-trigger this cycle. Port of
// judge_emergency_sql_recovered.
func (s *PlanChange) judgeRecovered(triggered []int64) {
	observation := s.deps.Cfg.GetFloat("emergency.plan_change.observation_time", 600)
	for _, info := range s.deps.Persist.GetEmergencySQLUnrecovered(s.dbID) {
		if s.lastSnapTS.Sub(info.EmergencyTS).Seconds() < observation {
			continue
		}
		if triggered == nil || !containsID(triggered, info.UniqueSQLID) {
			s.recordPlanChangeRecovered(info.UniqueSQLID, s.snap.SnapTS)
			s.deps.Persist.UpdateEmergencySQLRecovered(s.dbID, info.SnapID, info.UniqueSQLID)
			s.deps.Logger.Info("Plan change recovered: db_id = %d, snap_id = %d, unique_sql_id = %d",
				s.dbID, info.SnapID, info.UniqueSQLID)
		}
	}
}

// activeSessionCounts tallies active sessions per unique SQL id.
func (s *PlanChange) activeSessionCounts() map[int64]int {
	out := map[int64]int{}
	for _, row := range s.snap.Session {
		if row.Str(9) != "active" {
			continue
		}
		id, _ := row.Int(4)
		out[id]++
	}
	return out
}

func indexStatements(rows []dbconn.Row) map[int64]dbconn.Row {
	out := map[int64]dbconn.Row{}
	for _, row := range rows {
		id, _ := row.Int(0)
		out[id] = row
	}
	return out
}

// statementField reads a numeric column from a statement row, treating a missing
// row (nil) as zero, matching the [0]*STATEMENT_COL_NUM default.
func statementField(row dbconn.Row, i int) float64 {
	if row == nil {
		return 0
	}
	return dbFloat(row.Col(i))
}

func emergencyToSQLInfo(e EmergencySQLInfo) SQLInfo {
	return SQLInfo{
		DBID: e.DBID, SnapID: e.SnapID, SnapTS: e.SnapTS, UniqueSQLID: e.UniqueSQLID,
		SQLAcsCnt: e.SQLAcsCnt, SQLLatency: e.SQLLatency, SQLCPUTime: e.SQLCPUTime, SQLQPS: e.SQLQPS,
	}
}

func containsID(ids []int64, v int64) bool {
	for _, id := range ids {
		if id == v {
			return true
		}
	}
	return false
}

func pcRound1(x float64) float64 { return math.Round(x*10) / 10 }
