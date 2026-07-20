package emergency

import (
	"fmt"
	"regexp"
	"strings"

	"gstop/internal/dbconn"
	"gstop/internal/model"
)

// sessionTimeSQL aggregates per-session DB/CPU/IO time, joined to the sessions
// under analysis. Preserved verbatim from emergency_base.py.
const sessionTimeSQL = `SELECT substring(sessid from '\.(.*)$') AS "sessid", stat_name, value FROM pv_session_time();`

// tableFromSQL extracts the target table for an ANALYZE suggestion. The original
// used a non-capturing group yet read group(1) (a latent bug that aborted the
// cycle); this makes the table group capturing so the intended ANALYZE command
// is produced.
var tableFromSQL = regexp.MustCompile(`(?i)(?:FROM|UPDATE|INSERT\s+INTO|DELETE\s+FROM)\s+("[^"]+"|[\w\.]+)`)

// Base holds the state and helpers shared by every scenario. Concrete scenarios
// embed *Base and satisfy Scenario via the Base method. Port of the Python
// Emergency base class.
type Base struct {
	name   string
	header string
	deps   Deps
	dbID   int

	snap   Snapshot
	snapID int

	triggered bool
	info      []string
	sqlIDs    []int64
	pids      []int64

	// analyze_session outputs, consumed by CPU/IO full.
	sortedTopSQL     []TopSQL
	overtimeSessList []string

	// terminate whitelist for thread-pool/connections full.
	whitelist []string

	snapshotPersistNumber int
	needPersist           bool
	startPersistSnapID    int
	startPersistTime      string
	persistSnapIDs        map[int]bool
	persistWriter         *rawLog
	sessionPersistWriter  *rawLog
}

// TopSQL is one aggregated hot SQL from analyze_session.
type TopSQL struct {
	SQLID      int64
	ActiveNum  int
	DBTime     float64
	CPUTime    float64
	IOTime     float64
	AnalyzeCmd string
	Query      string
}

// NewBase builds a Base for a scenario.
func NewBase(name, header string, deps Deps, snapshotPersistNumber int) *Base {
	return &Base{
		name:                  name,
		header:                header,
		deps:                  deps,
		snapshotPersistNumber: snapshotPersistNumber,
		persistSnapIDs:        map[int]bool{},
	}
}

// Common satisfies the Scenario interface by exposing the shared base. It is not
// named Base to avoid colliding with the embedded *Base field of each scenario.
func (b *Base) Common() *Base { return b }

// Name / Header expose identity for the dispatcher and renderer.
func (b *Base) Name() string   { return b.name }
func (b *Base) Header() string { return b.header }

// Triggered / Info / SQLIDs / PIDs expose analysis results.
func (b *Base) Triggered() bool    { return b.triggered }
func (b *Base) Info() []string     { return b.info }
func (b *Base) SQLIDs() []int64    { return b.sqlIDs }
func (b *Base) PIDs() []int64      { return b.pids }
func (b *Base) Snapshot() Snapshot { return b.snap }

// Reset clears per-cycle analysis state before the dispatcher injects a new
// snapshot, matching the field resets in emergency_main.
func (b *Base) Reset() {
	b.triggered = false
	b.info = nil
	b.sqlIDs = nil
	b.pids = nil
}

// Inject sets the current snapshot for this cycle.
func (b *Base) Inject(snap Snapshot) {
	b.snap = snap
	b.snapID = snap.SnapID
}

// Trigger marks the scenario as fired.
func (b *Base) Trigger() { b.triggered = true }

// AddInfo appends a display line.
func (b *Base) AddInfo(line string) { b.info = append(b.info, line) }

// osCell / instanceCell / dbCell return a monitor cell or "" when out of range.
func (b *Base) osCell(i int) string       { return cell(b.snap.OS, i) }
func (b *Base) instanceCell(i int) string { return cell(b.snap.Instance, i) }
func (b *Base) dbCell(i int) string       { return cell(b.snap.DB, i) }

func cell(cells []string, i int) string {
	if i < 0 || i >= len(cells) {
		return ""
	}
	return cells[i]
}

// AppendSplitString wraps a long command across 140-character lines into the
// info list, matching append_split_string.
func (b *Base) AppendSplitString(text, prefix string) {
	const width = 140
	full := prefix + ": " + text
	for i := 0; i < len(full); i += width {
		end := i + width
		if end > len(full) {
			end = len(full)
		}
		b.info = append(b.info, full[i:end])
	}
}

// analyzeSession implements the CPU/IO-full shared core: it finds active
// over-threshold sessions in the target execution state and aggregates their
// DB/CPU/IO time per unique SQL id. Port of Emergency.analyze_session.
func (b *Base) analyzeSession(targetSTE string, overtimeThreshMS float64) {
	b.sortedTopSQL = nil
	b.overtimeSessList = nil

	timeRows := b.deps.DB.Query(sessionTimeSQL)
	if timeRows == nil {
		b.deps.Logger.Error("Exec session time query failed.")
		return
	}
	sessionTime := buildSessionTime(timeRows)

	agg := map[int64]*TopSQL{}
	var order []int64
	for _, row := range b.snap.Session {
		if !b.qualifiesForAnalyze(row, targetSTE, overtimeThreshMS) {
			continue
		}
		sqlID, _ := dbInt64(row.Col(4))
		sessionID := model.DisplayValue(row.Col(3))
		b.overtimeSessList = append(b.overtimeSessList, sessionID)
		if pid, ok := dbInt64(row.Col(0)); ok {
			b.pids = append(b.pids, pid)
		}
		top := agg[sqlID]
		if top == nil {
			top = newTopSQL(sqlID, row.Str(5))
			agg[sqlID] = top
			order = append(order, sqlID)
		}
		top.ActiveNum++
		accumulateSessionTime(top, sessionTime[sessionID])
	}
	b.sortedTopSQL = sortTopSQL(agg, order)
}

// qualifiesForAnalyze reports whether a session is an active, over-threshold
// transaction in the target execution state with a valid SQL id.
func (b *Base) qualifiesForAnalyze(row dbconn.Row, targetSTE string, overtimeThreshMS float64) bool {
	if row.Str(9) != "active" || row.IsNull(16) {
		return false
	}
	if sqlID, ok := dbInt64(row.Col(4)); !ok || sqlID == 0 {
		return false
	}
	if row.Str(10) != targetSTE {
		return false
	}
	xactRunMS := round2(dbFloat(row.Col(18)) / 1000)
	return xactRunMS > overtimeThreshMS
}

// newTopSQL seeds an aggregate with the ANALYZE command and first query line.
func newTopSQL(sqlID int64, query string) *TopSQL {
	analyzeCmd := "None"
	if m := tableFromSQL.FindStringSubmatch(query); m != nil {
		analyzeCmd = "ANALYZE " + m[1] + ";"
	}
	firstLine := query
	if idx := strings.IndexByte(query, '\n'); idx >= 0 {
		firstLine = query[:idx]
	}
	return &TopSQL{SQLID: sqlID, AnalyzeCmd: analyzeCmd, Query: firstLine}
}

func accumulateSessionTime(top *TopSQL, stats []statValue) {
	for _, s := range stats {
		switch s.name {
		case "DB_TIME":
			top.DBTime += s.value
		case "CPU_TIME":
			top.CPUTime += s.value
		case "DATA_IO_TIME":
			top.IOTime += s.value
		}
	}
}

// Terminate helpers -----------------------------------------------------------

func (b *Base) terminateSession(pid, sessionID any) {
	cmd := model.TerminateSessionCmd(pid, sessionID)
	b.deps.Logger.Warning("Exec command: %s", cmd)
	b.deps.DB.NoReturn(cmd)
}

func (b *Base) terminateLimitedSessions(sqlID string, maxCount int) {
	block, err := model.TerminateLimitedSessions(sqlID, maxCount)
	if err != nil {
		b.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	b.deps.Logger.Warning("Exec anonymous: %s", block)
	b.deps.DB.NoReturn(block)
}

func (b *Base) terminateLimitedSessionsWithTime(sqlID string, maxCount, timeoutMS int) {
	block, err := model.TerminateLimitedSessionsWithTime(sqlID, maxCount, timeoutMS)
	if err != nil {
		b.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	b.deps.Logger.Warning("Exec anonymous: %s", block)
	b.deps.DB.NoReturn(block)
}

func (b *Base) terminateUnlimitedSessionsWithTime(sqlID string, timeoutMS int) {
	block, err := model.TerminateUnlimitedSessionsWithTime(sqlID, timeoutMS)
	if err != nil {
		b.deps.Logger.Error("build terminate block failed: %v", err)
		return
	}
	b.deps.Logger.Warning("Exec anonymous: %s", block)
	b.deps.DB.NoReturn(block)
}

func (b *Base) terminateStateSessions(state string) {
	where := stateWhere(state)
	sql := fmt.Sprintf("SELECT pg_terminate_session(pid, sessionid) FROM pg_stat_activity WHERE datname != 'postgres' AND %s", where)
	if users := b.whitelistClause(); users != "" {
		sql = strings.TrimSuffix(sql, ";") + " AND usename NOT IN (" + users + ");"
	}
	b.deps.DB.Query(sql)
}

func (b *Base) terminateIdleSessions()       { b.terminateStateSessions("idle") }
func (b *Base) terminateIdleInXactSessions() { b.terminateStateSessions("idle in transaction") }
func (b *Base) terminateNoneSessions()       { b.terminateStateSessions("none") }

// stateWhere renders the state predicate; "none" maps to SQL NULL.
func stateWhere(state string) string {
	if state == "none" {
		return "state IS NULL;"
	}
	return fmt.Sprintf("state = '%s';", state)
}

// whitelistClause renders the quoted user whitelist, or "" when none is set.
func (b *Base) whitelistClause() string {
	if len(b.whitelist) == 0 {
		return ""
	}
	quoted := make([]string, len(b.whitelist))
	for i, u := range b.whitelist {
		quoted[i] = "'" + u + "'"
	}
	return strings.Join(quoted, ",")
}
