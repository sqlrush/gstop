// Package healthdash collects and renders the independent database health
// dashboard. Its published Snapshot values are immutable copies so database
// refreshes never block terminal rendering.
package healthdash

import "time"

// StatementSample is one instance-wide dbe_perf.statement aggregate.
type StatementSample struct {
	SQLID     int64
	Calls     int64
	DBTimeUS  float64
	Query     string
	Databases []string
	Users     []string
}

// ActiveSQL is one active pg_stat_activity row. Parallel-worker rows may share
// a logical SessionID. ElapsedUS is its current runtime; MemoryMB is populated
// only when dynamic-memory collection is enabled.
type ActiveSQL struct {
	SQLID      int64
	PID        int64
	SessionID  string
	Query      string
	Database   string
	User       string
	ElapsedUS  float64
	MemoryMB   float64
	QueryStart time.Time
}

// SQLMetric is a row shared by the three SQL rankings. Fields irrelevant to a
// particular ranking remain zero.
type SQLMetric struct {
	SQLID                    int64
	Query                    string
	Databases                []string
	Users                    []string
	AverageUS                float64
	Calls                    int64
	CallsDelta               int64
	ActiveSessions           int
	TotalMemoryMB            float64
	MaxMemoryMB              float64
	RepresentativePID        int64
	RepresentativeSessionID  string
	RepresentativeElapsedUS  float64
	RepresentativeQueryStart time.Time
	CapturedAt               time.Time
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

// LockChain is one current waiter-to-blocker relationship. Parallel thread
// duplicates are collapsed by session and lock identity before publication.
type LockChain struct {
	WaiterPID      int64
	WaiterSession  string
	BlockerPID     int64
	BlockerSession string
	LockType       string
	Mode           string
	LockTag        string
	SQLID          int64
	Query          string
	ElapsedUS      float64
}

// LockHealth summarizes the current lock-wait sample.
type LockHealth struct {
	Waiters       int
	Blockers      int
	LongestWaitUS float64
	Chains        []LockChain
}

// ReplicationChannel is one normalized WAL sender or receiver channel.
type ReplicationChannel struct {
	Direction        string
	LocalRole        string
	PeerRole         string
	PeerState        string
	State            string
	Channel          string
	SenderSent       string
	ReceiverReceived string
	ReceiverWrite    string
	ReceiverFlush    string
	ReceiverReplay   string
	SyncPercent      float64
	SyncState        string
	SyncPriority     int64
	LagBytes         int64
}

// ReplicationHealth contains the local role and every current WAL channel.
type ReplicationHealth struct {
	LocalRole string
	Channels  []ReplicationChannel
}

// ClusterNode is one SQL topology row from PGXC_NODE. ActiveKnown is true only
// for coordinator rows because Huawei documents nodeis_active as CN-scoped.
type ClusterNode struct {
	Name        string
	Type        string
	Host        string
	Port        int64
	StandbyHost string
	StandbyPort int64
	HostPrimary bool
	NodePrimary bool
	Preferred   bool
	Central     bool
	Active      bool
	ActiveKnown bool
	ID          int64
}

// ClusterComponent is one normalized component row from cm_ctl query output.
type ClusterComponent struct {
	Kind     string
	Node     string
	Address  string
	Instance string
	Role     string
	State    string
	Raw      string
}

// ClusterHealth combines SQL topology and optional CM runtime state.
type ClusterHealth struct {
	SQLAvailable bool
	CMAvailable  bool
	Nodes        []ClusterNode
	Components   []ClusterComponent
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

	FastRefreshedAt  time.Time
	FastError        string
	AverageSQL       []SQLMetric
	ExecutionSQL     []SQLMetric
	ActiveElapsedSQL []SQLMetric
	Waits            []WaitMetric
	CPU              CPUStat
	Lock             LockHealth
	LockError        string
	Replication      ReplicationHealth
	ReplicationError string
	PlanChanges      []PlanChangeEvent

	MemoryEnabled     bool
	MemoryRefreshedAt time.Time
	MemoryError       string
	MemorySQL         []SQLMetric

	SlowRefreshedAt time.Time
	SlowRefreshing  bool
	AnalyzeHistory  []AnalyzeRecord
	InvalidIndexes  []InvalidIndex
	DatabaseErrors  []DatabaseError
	Cluster         ClusterHealth
}

// Clone returns a deep copy suitable for publishing across goroutines.
func (s Snapshot) Clone() Snapshot {
	out := s
	out.AverageSQL = append([]SQLMetric(nil), s.AverageSQL...)
	out.ExecutionSQL = append([]SQLMetric(nil), s.ExecutionSQL...)
	out.ActiveElapsedSQL = append([]SQLMetric(nil), s.ActiveElapsedSQL...)
	out.Waits = append([]WaitMetric(nil), s.Waits...)
	out.Lock.Chains = append([]LockChain(nil), s.Lock.Chains...)
	out.Replication.Channels = append([]ReplicationChannel(nil), s.Replication.Channels...)
	out.PlanChanges = append([]PlanChangeEvent(nil), s.PlanChanges...)
	out.MemorySQL = append([]SQLMetric(nil), s.MemorySQL...)
	out.AnalyzeHistory = append([]AnalyzeRecord(nil), s.AnalyzeHistory...)
	out.InvalidIndexes = append([]InvalidIndex(nil), s.InvalidIndexes...)
	out.DatabaseErrors = append([]DatabaseError(nil), s.DatabaseErrors...)
	out.Cluster.Nodes = append([]ClusterNode(nil), s.Cluster.Nodes...)
	out.Cluster.Components = append([]ClusterComponent(nil), s.Cluster.Components...)
	cloneSQLMetricIdentity(out.AverageSQL)
	cloneSQLMetricIdentity(out.ExecutionSQL)
	cloneSQLMetricIdentity(out.ActiveElapsedSQL)
	cloneSQLMetricIdentity(out.MemorySQL)
	return out
}

func cloneSQLMetricIdentity(rows []SQLMetric) {
	for i := range rows {
		rows[i].Databases = append([]string(nil), rows[i].Databases...)
		rows[i].Users = append([]string(nil), rows[i].Users...)
	}
}
