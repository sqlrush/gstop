package emergency

import (
	"sort"

	"gstop/internal/config"
)

// MemPersist is the in-memory statistics backend used in production. It keeps
// bounded, per-snapshot maps of instance, SQL, and emergency-SQL samples that the
// plan-change scenario compares across time. Port of mem_persist.MemPersist.
type MemPersist struct {
	cfg     *config.Config
	snapID  int
	maxSnap int

	insInfo      map[int]InsInfo
	sqlInfo      map[int][]SQLInfo
	emergencySQL map[int][]EmergencySQLInfo
	compareScope int
}

// NewMemPersist builds the in-memory backend.
func NewMemPersist(cfg *config.Config) *MemPersist {
	return &MemPersist{
		cfg:          cfg,
		maxSnap:      cfg.GetInt("emergency.plan_change.max_sql_snapshot_number", 600),
		compareScope: cfg.GetInt("emergency.plan_change.snapshot_compare_scope", 10),
		insInfo:      map[int]InsInfo{},
		sqlInfo:      map[int][]SQLInfo{},
		emergencySQL: map[int][]EmergencySQLInfo{},
	}
}

// GetSnapID returns a new monotonically increasing snapshot id.
func (m *MemPersist) GetSnapID() int {
	m.snapID++
	return m.snapID
}

// PersistInsInfo stores an instance sample, evicting the oldest at capacity.
func (m *MemPersist) PersistInsInfo(info InsInfo) {
	evictOldest(m.insInfo, m.maxSnap)
	m.insInfo[info.SnapID] = info
}

// PersistSQLInfo appends a SQL sample to its snapshot bucket.
func (m *MemPersist) PersistSQLInfo(info SQLInfo) {
	evictOldestSlice(m.sqlInfo, m.maxSnap)
	m.sqlInfo[info.SnapID] = append(m.sqlInfo[info.SnapID], info)
}

// PersistEmergencySQLInfo appends an emergency-SQL baseline to its bucket.
func (m *MemPersist) PersistEmergencySQLInfo(info EmergencySQLInfo) {
	evictOldestSlice(m.emergencySQL, m.maxSnap)
	m.emergencySQL[info.SnapID] = append(m.emergencySQL[info.SnapID], info)
}

// GetSQLInfoSnap returns samples of uniqueSQLID in the comparison window
// [targetSnapID-scope, targetSnapID-1], oldest first.
func (m *MemPersist) GetSQLInfoSnap(dbID, targetSnapID int, uniqueSQLID int64) []SQLInfo {
	var out []SQLInfo
	for _, snapID := range sortedKeys(m.sqlInfo) {
		if snapID < targetSnapID-m.compareScope || snapID > targetSnapID-1 {
			continue
		}
		for _, info := range m.sqlInfo[snapID] {
			if info.UniqueSQLID == uniqueSQLID {
				out = append(out, info)
			}
		}
	}
	return out
}

// GetInsInfoSnap returns the instance sample at snapID.
func (m *MemPersist) GetInsInfoSnap(dbID, snapID int) (InsInfo, bool) {
	info, ok := m.insInfo[snapID]
	return info, ok
}

// GetEmergencySQLInfoSnap returns the first un-recovered baseline for uniqueSQLID.
func (m *MemPersist) GetEmergencySQLInfoSnap(dbID int, uniqueSQLID int64) (EmergencySQLInfo, bool) {
	for _, infos := range m.emergencySQL {
		for _, info := range infos {
			if info.UniqueSQLID == uniqueSQLID && !info.Recovered {
				return info, true
			}
		}
	}
	return EmergencySQLInfo{}, false
}

// GetEmergencySQLUnrecovered returns every stored emergency-SQL baseline.
func (m *MemPersist) GetEmergencySQLUnrecovered(dbID int) []EmergencySQLInfo {
	var out []EmergencySQLInfo
	for _, infos := range m.emergencySQL {
		out = append(out, infos...)
	}
	return out
}

// UpdateEmergencySQLRecovered removes uniqueSQLID from the given snapshot's bucket.
func (m *MemPersist) UpdateEmergencySQLRecovered(dbID, snapID int, uniqueSQLID int64) {
	infos, ok := m.emergencySQL[snapID]
	if !ok {
		return
	}
	kept := infos[:0:0]
	for _, info := range infos {
		if info.UniqueSQLID != uniqueSQLID {
			kept = append(kept, info)
		}
	}
	if len(kept) == 0 {
		delete(m.emergencySQL, snapID)
	} else {
		m.emergencySQL[snapID] = kept
	}
}

func evictOldest[V any](m map[int]V, max int) {
	if len(m) < max {
		return
	}
	if k, ok := minKey(m); ok {
		delete(m, k)
	}
}

func evictOldestSlice[V any](m map[int][]V, max int) {
	if len(m) < max {
		return
	}
	if k, ok := minKeySlice(m); ok {
		delete(m, k)
	}
}

func minKey[V any](m map[int]V) (int, bool) {
	first := true
	var min int
	for k := range m {
		if first || k < min {
			min, first = k, false
		}
	}
	return min, !first
}

func minKeySlice[V any](m map[int][]V) (int, bool) {
	first := true
	var min int
	for k := range m {
		if first || k < min {
			min, first = k, false
		}
	}
	return min, !first
}

func sortedKeys[V any](m map[int][]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}
