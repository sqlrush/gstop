package emergency

import (
	"time"

	"gstop/internal/alarm"
	"gstop/internal/config"
	"gstop/internal/dbconn"
	"gstop/internal/logging"
	"gstop/internal/oscmd"
)

// Deps bundles the collaborators every scenario shares.
type Deps struct {
	Cfg     *config.Config
	DB      *dbconn.DB
	OS      *oscmd.Runner
	Logger  *logging.Logger
	Alarm   *alarm.Alarm
	Persist Persister
}

// SQLInfo is one per-SQL statistics sample stored by the plan-change persistence
// backend. Fields are in the order the Python dict preserved, so index-based
// comparisons in the port remain meaningful.
type SQLInfo struct {
	DBID        int
	SnapID      int
	SnapTS      time.Time
	UniqueSQLID int64
	SQLAcsCnt   int
	SQLLatency  float64
	SQLCPUTime  float64
	SQLQPS      float64
}

// InsInfo is one instance-level statistics sample.
type InsInfo struct {
	DBID      int
	SnapID    int
	SnapTS    time.Time
	InsAcsCnt int
	InsCPUUtl float64
}

// EmergencySQLInfo records a SQL that first triggered a plan-change emergency,
// used as the "before" baseline until it recovers.
type EmergencySQLInfo struct {
	DBID        int
	SnapID      int
	SnapTS      time.Time
	UniqueSQLID int64
	SQLAcsCnt   int
	SQLLatency  float64
	SQLCPUTime  float64
	SQLQPS      float64
	EmergencyTS time.Time
	Recovered   bool
}

// Persister stores and queries the rolling statistics snapshots the plan-change
// scenario compares against. Two implementations exist: an in-memory store
// (production) and a database-backed store (optional). Port of the MemPersist /
// Persist duo.
type Persister interface {
	GetSnapID() int
	PersistInsInfo(InsInfo)
	PersistSQLInfo(SQLInfo)
	PersistEmergencySQLInfo(EmergencySQLInfo)
	// GetSQLInfoSnap returns samples of uniqueSQLID in the comparison window
	// [targetSnapID-scope, targetSnapID-1], oldest first.
	GetSQLInfoSnap(dbID, targetSnapID int, uniqueSQLID int64) []SQLInfo
	GetInsInfoSnap(dbID, snapID int) (InsInfo, bool)
	// GetEmergencySQLInfoSnap returns the un-recovered baseline for uniqueSQLID.
	GetEmergencySQLInfoSnap(dbID int, uniqueSQLID int64) (EmergencySQLInfo, bool)
	GetEmergencySQLUnrecovered(dbID int) []EmergencySQLInfo
	UpdateEmergencySQLRecovered(dbID, snapID int, uniqueSQLID int64)
}
