// Package healthdash collects and renders the independent database health
// dashboard. Its published Snapshot values are immutable copies so database
// refreshes never block terminal rendering.
package healthdash

import "time"

// StatementSample is one instance-wide dbe_perf.statement aggregate.
type StatementSample struct {
	SQLID    int64
	Calls    int64
	DBTimeUS float64
	Query    string
}

// ActiveSQL is one currently running session. ElapsedUS is its current runtime;
// MemoryMB is populated only when dynamic-memory collection is enabled.
type ActiveSQL struct {
	SQLID     int64
	PID       int64
	SessionID string
	Query     string
	ElapsedUS float64
	MemoryMB  float64
}

// SQLMetric is a row shared by the three SQL rankings. Fields irrelevant to a
// particular ranking remain zero.
type SQLMetric struct {
	SQLID          int64
	Query          string
	AverageUS      float64
	Calls          int64
	CallsDelta     int64
	ActiveSessions int
	TotalMemoryMB  float64
	MaxMemoryMB    float64
}

// WaitSample is one raw cumulative wait-event sample.
type WaitSample struct {
	Event  string
	Waits  int64
	TimeUS int64
	Type   string
}

// WaitMetric is one gstop-start-relative wait-event row.
type WaitMetric struct {
	Event       string
	WaitsDelta  int64
	TimeUSDelta int64
	AverageUS   float64
	Share       float64
	Type        string
}

// CPUStat is the separately displayed DB CPU delta and share.
type CPUStat struct {
	TimeUSDelta int64
	Share       float64
}

// PlanChangeEvent is the dashboard-facing immutable form of a plan regression.
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

// AnalyzeRecord is one reliable ANALYZE/AUTOANALYZE history timestamp.
type AnalyzeRecord struct {
	Database string
	Schema   string
	Table    string
	Source   string
	At       time.Time
}

// InvalidIndex describes one unusable, unready, or invalid user index.
type InvalidIndex struct {
	Database string
	Schema   string
	Table    string
	Index    string
	Usable   bool
	Ready    bool
	Valid    bool
}

// DatabaseError preserves one database's failure without hiding successful
// results from other databases.
type DatabaseError struct {
	Database string
	Area     string
	Message  string
}

// Snapshot is the complete health dashboard state published by Collector.
type Snapshot struct {
	StartedAt time.Time

	FastRefreshedAt time.Time
	FastError       string
	AverageSQL      []SQLMetric
	ExecutionSQL    []SQLMetric
	Waits           []WaitMetric
	CPU             CPUStat
	PlanChanges     []PlanChangeEvent

	MemoryEnabled     bool
	MemoryRefreshedAt time.Time
	MemoryError       string
	MemorySQL         []SQLMetric

	SlowRefreshedAt time.Time
	SlowRefreshing  bool
	AnalyzeHistory  []AnalyzeRecord
	InvalidIndexes  []InvalidIndex
	DatabaseErrors  []DatabaseError
}

// Clone returns a deep copy suitable for publishing across goroutines.
func (s Snapshot) Clone() Snapshot {
	out := s
	out.AverageSQL = append([]SQLMetric(nil), s.AverageSQL...)
	out.ExecutionSQL = append([]SQLMetric(nil), s.ExecutionSQL...)
	out.Waits = append([]WaitMetric(nil), s.Waits...)
	out.PlanChanges = append([]PlanChangeEvent(nil), s.PlanChanges...)
	out.MemorySQL = append([]SQLMetric(nil), s.MemorySQL...)
	out.AnalyzeHistory = append([]AnalyzeRecord(nil), s.AnalyzeHistory...)
	out.InvalidIndexes = append([]InvalidIndex(nil), s.InvalidIndexes...)
	out.DatabaseErrors = append([]DatabaseError(nil), s.DatabaseErrors...)
	return out
}
